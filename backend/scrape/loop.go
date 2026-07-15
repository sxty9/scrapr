package scrape

import (
	"context"
	"errors"
	neturl "net/url"
	"strings"
	"time"

	"github.com/sxty9/aigentic/aigentic"
	"github.com/sxty9/prizm/prizm"
	"github.com/sxty9/prizm/subprizm"
)

// coordinate runs the whole crawl (prizm depth 0): it owns the runState, seeds the frontier
// and expands it breadth-first, spawning one worker per frontier URL each level via
// subprizm.SpawnAll. It never recurses into itself — crawl depth is the loop counter, so prizm
// recursion stays flat (coordinator → worker → choose → leaf).
func (p *processor) coordinate(ctx context.Context, job JobSpec, env prizm.Env) (Out, error) {
	b := clampBudget(job.Budget, p.cfg.DefaultBudget)
	if b.DeadlineMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(b.DeadlineMS)*time.Millisecond)
		defer cancel()
	}

	allow := newAllowlist(job.Allow)
	if allow.empty() {
		allow = deriveAllow(job.Seeds) // fail-safe: confine the crawl to the seeds' domains
	}
	if allow.empty() {
		return Out{}, errors.New("scrape: no valid seed URL / allowlist")
	}

	rs := &runState{goal: job.Goal, allow: allow, budget: b, visited: map[string]bool{}}
	p.runs.put(job.RunID, rs)
	defer p.runs.del(job.RunID)

	var frontier []PageTask
	for _, s := range job.Seeds {
		if u, ok := rs.reserve(s); ok {
			frontier = append(frontier, PageTask{RunID: job.RunID, URL: u, Depth: 0})
		}
	}

	status := "ok"
	for depth := 0; len(frontier) > 0 && depth <= b.MaxDepth; depth++ {
		if ctx.Err() != nil {
			status = "partial"
			break
		}
		reqs := make([]prizm.Request, len(frontier))
		for i := range frontier {
			task := frontier[i]
			data, err := prizm.EncodeData(In{Page: &task})
			if err != nil {
				return Out{}, err
			}
			reqs[i] = prizm.SubRequest(env.Header, Kind, data)
		}

		resps, err := subprizm.SpawnAll(ctx, env.Spawn, reqs, workerConcurrency)
		// A worker returns an error only for a fatal LLM/engine unavailability; a page that
		// merely 404s or is robots-blocked returns an empty Out. So an unavailable engine
		// aborts the whole crawl (→ 503), while page-level failures are tolerated.
		if err != nil && errors.Is(err, aigentic.ErrProcessorUnavailable) {
			return Out{}, err
		}

		var next []PageTask
		for _, resp := range resps {
			if len(resp.Data) == 0 {
				continue
			}
			wo, derr := prizm.DecodeData[Out](resp.Data)
			if derr != nil {
				continue
			}
			rs.collect(wo.Artifacts, wo.Bytes)
			if depth+1 > b.MaxDepth {
				continue
			}
			for _, f := range wo.Follow {
				if rs.overBudget() {
					break
				}
				if u, ok := rs.reserve(f.URL); ok {
					next = append(next, PageTask{RunID: job.RunID, URL: u, Depth: depth + 1})
				}
			}
		}
		if rs.overBudget() {
			status = "partial"
			break
		}
		frontier = next
	}

	arts, pages, bytes := rs.snapshot()
	return Out{RunID: job.RunID, Status: status, Pages: pages, Bytes: bytes, Artifacts: arts}, nil
}

// work processes one page (prizm depth 1): fetch → parse → decide (spawns choose) → act. It
// tolerates fetch/parse failures by returning an empty Out (the page just yields nothing);
// only an unavailable decision engine is returned as an error.
func (p *processor) work(ctx context.Context, task PageTask, env prizm.Env) (Out, error) {
	rs := p.runs.get(task.RunID)
	if rs == nil {
		return Out{}, nil // run finished/cancelled — nothing to do
	}
	host := hostOf(task.URL)
	if !p.robots.allowed(ctx, task.URL) {
		return Out{}, nil
	}
	if err := p.limiter.wait(ctx, host, p.robots.crawlDelay(ctx, task.URL)); err != nil {
		return Out{}, nil // context cancelled/deadline — drop this page
	}
	fetched, err := p.fetch.Fetch(ctx, task.URL, rs.budget.MaxFileBytes)
	if err != nil {
		return Out{}, nil
	}
	if !rs.allow.allowedURL(fetched.FinalURL) {
		return Out{}, nil // redirected off the allowlist
	}

	// A non-HTML seed/asset is stored directly — no decision needed.
	if !isHTML(fetched.ContentType) {
		ref, perr := env.Grave.Put(ctx, "", fetched.Body)
		if perr != nil {
			return Out{}, nil
		}
		n := int64(len(fetched.Body))
		return Out{
			Pages: 1,
			Bytes: n,
			Artifacts: []Artifact{{
				URL:       fetched.FinalURL,
				Title:     titleFromURL(fetched.FinalURL),
				Kategorie: defaultKategorie,
				MediaType: fetched.ContentType,
				Bytes:     n,
				Ref:       ref,
			}},
		}, nil
	}

	page, perr := parseHTML(fetched, rs.allow, p.cfg.AllowPrivateHosts)
	if perr != nil {
		return Out{}, nil
	}

	act, aerr := p.decideNext(ctx, env, rs.goal, page)
	if aerr != nil {
		if errors.Is(aerr, aigentic.ErrProcessorUnavailable) {
			return Out{}, aerr // fatal: abort the crawl
		}
		return Out{}, nil // a malformed decision on one page shouldn't kill the crawl
	}

	out := Out{Pages: 1}
	if act.Keep && page.Markdown != "" {
		md := []byte(page.Markdown)
		if ref, e := env.Grave.Put(ctx, "", md); e == nil {
			out.Bytes += int64(len(md))
			out.Artifacts = append(out.Artifacts, Artifact{
				URL:       fetched.FinalURL,
				Title:     firstNonEmpty(act.Title, page.Title),
				Kategorie: act.Kategorie,
				MediaType: "text/markdown",
				Bytes:     int64(len(md)),
				Ref:       ref,
			})
		}
	}
	for _, d := range act.Downloads {
		if out.Bytes >= rs.budget.MaxBytes {
			break
		}
		if a, ok := p.download(ctx, env, rs, d); ok {
			out.Bytes += a.Bytes
			out.Artifacts = append(out.Artifacts, a)
		}
	}
	if !act.Done {
		follows := act.Follows
		if len(follows) > rs.budget.MaxFanOut {
			follows = follows[:rs.budget.MaxFanOut]
		}
		for _, u := range follows {
			out.Follow = append(out.Follow, PageTask{RunID: task.RunID, URL: u, Depth: task.Depth + 1})
		}
	}
	return out, nil
}

// download fetches one LLM-selected asset (size/rate/robots guarded) and stores its bytes.
func (p *processor) download(ctx context.Context, env prizm.Env, rs *runState, d resolvedDownload) (Artifact, bool) {
	if !isPublicHTTP(d.URL, p.cfg.AllowPrivateHosts) {
		return Artifact{}, false
	}
	if !p.robots.allowed(ctx, d.URL) {
		return Artifact{}, false
	}
	if err := p.limiter.wait(ctx, hostOf(d.URL), p.robots.crawlDelay(ctx, d.URL)); err != nil {
		return Artifact{}, false
	}
	fetched, err := p.fetch.Fetch(ctx, d.URL, rs.budget.MaxFileBytes)
	if err != nil {
		return Artifact{}, false
	}
	ref, e := env.Grave.Put(ctx, "", fetched.Body)
	if e != nil {
		return Artifact{}, false
	}
	return Artifact{
		URL:       fetched.FinalURL,
		Title:     firstNonEmpty(d.Title, titleFromURL(fetched.FinalURL)),
		Kategorie: d.Kategorie,
		MediaType: fetched.ContentType,
		Bytes:     int64(len(fetched.Body)),
		Ref:       ref,
	}, true
}

// deriveAllow builds an allowlist from the registrable domains of the seed URLs.
func deriveAllow(seeds []string) *allowlist {
	var domains []string
	for _, s := range seeds {
		u, err := neturl.Parse(strings.TrimSpace(s))
		if err != nil {
			continue
		}
		if d := registrableDomain(u.Hostname()); d != "" {
			domains = append(domains, d)
		}
	}
	return newAllowlist(domains)
}
