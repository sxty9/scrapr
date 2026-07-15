package scrape

import (
	"net"
	neturl "net/url"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"
)

// runRegistry maps a RunID to its shared runState. The coordinator creates/deletes the entry;
// concurrent workers of the same crawl look it up. Only the map access is locked — the
// per-run fields workers read (goal, allow, budget) are immutable after creation.
type runRegistry struct {
	mu   sync.RWMutex
	runs map[string]*runState
}

func newRunRegistry() *runRegistry { return &runRegistry{runs: map[string]*runState{}} }

func (r *runRegistry) put(id string, rs *runState) {
	r.mu.Lock()
	r.runs[id] = rs
	r.mu.Unlock()
}

func (r *runRegistry) get(id string) *runState {
	r.mu.RLock()
	rs := r.runs[id]
	r.mu.RUnlock()
	return rs
}

func (r *runRegistry) del(id string) {
	r.mu.Lock()
	delete(r.runs, id)
	r.mu.Unlock()
}

// runState is one crawl's shared state. goal/allow/budget are set once (immutable, workers
// read them lock-free); visited/pages/bytes/arts are mutated under mu (reserve from the
// coordinator, collect from result aggregation).
type runState struct {
	goal   string
	allow  *allowlist
	budget Budget

	mu      sync.Mutex
	visited map[string]bool
	pages   int
	bytes   int64
	arts    []Artifact
}

// reserve atomically checks allowlist + dedupe + page budget, marks the URL visited, and
// returns its canonical form. This is the one gate every URL passes before becoming a task.
func (rs *runState) reserve(raw string) (string, bool) {
	c, ok := canonical(raw)
	if !ok || !rs.allow.allowedURL(c) {
		return "", false
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.visited[c] || rs.pages >= rs.budget.MaxPages {
		return "", false
	}
	rs.visited[c] = true
	rs.pages++
	return c, true
}

func (rs *runState) collect(arts []Artifact, bytes int64) {
	rs.mu.Lock()
	rs.arts = append(rs.arts, arts...)
	rs.bytes += bytes
	rs.mu.Unlock()
}

func (rs *runState) overBudget() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.bytes >= rs.budget.MaxBytes || rs.pages >= rs.budget.MaxPages
}

func (rs *runState) snapshot() ([]Artifact, int, int64) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.arts, rs.pages, rs.bytes
}

// allowlist gates crawl navigation by registrable domain (eTLD+1). Follow-links and a page's
// post-redirect final URL must match; downloads use the looser isPublicHTTP (assets often live
// on a sibling CDN host, e.g. Wikipedia media on upload.wikimedia.org).
type allowlist struct {
	domains map[string]bool
}

func newAllowlist(allow []string) *allowlist {
	a := &allowlist{domains: map[string]bool{}}
	for _, d := range allow {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			a.domains[d] = true
		}
	}
	return a
}

func (a *allowlist) empty() bool { return len(a.domains) == 0 }

// allowedURL reports whether raw's registrable domain is on the allowlist. Fail-closed: an
// empty allowlist allows nothing (the coordinator always derives one from the seeds).
func (a *allowlist) allowedURL(raw string) bool {
	u, err := neturl.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if a.empty() {
		return false
	}
	e := registrableDomain(u.Hostname())
	return e != "" && a.domains[e]
}

// registrableDomain returns the eTLD+1 of host (e.g. de.wikipedia.org → wikipedia.org), or ""
// if it can't be determined. For an IP literal it returns the host unchanged.
func registrableDomain(host string) string {
	host = strings.ToLower(host)
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return host
	}
	e, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	return e
}

// canonical normalizes a URL for dedupe: http(s) only, lowercase scheme+host, no fragment,
// non-empty path, trailing slash stripped (except root).
func canonical(raw string) (string, bool) {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if u.Host == "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	if len(u.Path) > 1 && strings.HasSuffix(u.Path, "/") {
		u.Path = strings.TrimRight(u.Path, "/")
		if u.Path == "" {
			u.Path = "/"
		}
	}
	return u.String(), true
}

// isPublicHTTP reports whether raw is an http(s) URL scrapr may fetch as an asset. It guards
// against SSRF: a public page must not make scrapr reach internal addresses. allowPrivate
// relaxes the loopback/private/link-local check (tests on 127.0.0.1; an internal Moodle).
func isPublicHTTP(raw string, allowPrivate bool) bool {
	u, err := neturl.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if allowPrivate {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	}
	return true
}

// clampBudget fills zero/negative fields from def and clamps fan-out to the ceiling.
func clampBudget(b, def Budget) Budget {
	if b.MaxPages <= 0 {
		b.MaxPages = def.MaxPages
	}
	if b.MaxDepth <= 0 {
		b.MaxDepth = def.MaxDepth
	}
	if b.MaxFanOut <= 0 {
		b.MaxFanOut = def.MaxFanOut
	}
	if b.MaxFanOut > maxFanOutCeiling {
		b.MaxFanOut = maxFanOutCeiling
	}
	if b.MaxBytes <= 0 {
		b.MaxBytes = def.MaxBytes
	}
	if b.MaxFileBytes <= 0 {
		b.MaxFileBytes = def.MaxFileBytes
	}
	if b.DeadlineMS <= 0 {
		b.DeadlineMS = def.DeadlineMS
	}
	return b
}

// hostOf returns the lowercase host[:port] of raw, or "" if unparseable.
func hostOf(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func firstNonEmpty(xs ...string) string {
	for _, s := range xs {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// validKategorie returns k if it is one of KATEGORIEN, else defaultKategorie.
func validKategorie(k string) string {
	for _, v := range KATEGORIEN {
		if k == v {
			return k
		}
	}
	return defaultKategorie
}
