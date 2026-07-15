package scrape_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sxty9/aigentic/aigentic"
	"github.com/sxty9/prizm/graveyard"
	"github.com/sxty9/prizm/prizm"

	"scrapr/scrape"
)

// fakeChoose registers a stub under aigentic.KindChoose that returns a fixed action JSON for
// every decision — so the loop is exercised end-to-end with no live LLM or ollama.
func fakeChoose(action string) prizm.Processor {
	return prizm.NewTyped(func(_ context.Context, _ aigentic.Request, _ prizm.Env) (aigentic.Result, error) {
		return aigentic.Result{Output: action, Engine: aigentic.KindChoose}, nil
	})
}

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
	})
	article := func(title, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html><head><title>" + title + "</title></head><body><h1>" + title + "</h1><p>" + body + "</p></body></html>"))
		}
	}
	mux.HandleFunc("/wiki/Article_One", article("Article One", "Discrete math content one."))
	mux.HandleFunc("/wiki/Article_Two", article("Article Two", "Graph theory content two."))
	mux.HandleFunc("/private/secret", article("Secret", "should never be fetched"))
	mux.HandleFunc("/files/slides.pdf", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 fake slides"))
	})
	mux.HandleFunc("/img/pic.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n fake image bytes"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Hub</title></head><body>
<h1>Hub Page</h1>
<p>Welcome to the hub of study materials.</p>
<a href="/wiki/Article_One">Article One</a>
<a href="https://evil.example.com/x">Evil Offsite</a>
<a href="/wiki/Article_Two">Article Two</a>
<a href="/private/secret">Secret Area</a>
<a href="/files/slides.pdf">Lecture Slides</a>
<img src="/img/pic.png" alt="Picture">
</body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// action: keep every page, follow the first three link candidates (Article_One, Article_Two,
// the robots-blocked /private/secret) and download the first asset candidate (slides.pdf).
const fakeAction = `{"extract":{"keep":true,"title":"","kategorie":"Quellen","summary":"relevant"},` +
	`"follow":[{"index":0,"reason":"a"},{"index":1,"reason":"b"},{"index":2,"reason":"c"}],` +
	`"download":[{"index":0,"kategorie":"Foliensatz","reason":"slides"}],"done":false}`

func newReg(t *testing.T, srv *httptest.Server, grave graveyard.Graveyard, budget scrape.Budget) *prizm.Registry {
	t.Helper()
	reg := prizm.NewRegistry(0)
	if err := reg.Register(aigentic.KindChoose, prizm.NewPrizm(fakeChoose(fakeAction), grave)); err != nil {
		t.Fatalf("register choose: %v", err)
	}
	cfg := scrape.Config{
		RequestInterval:   time.Millisecond,
		AllowPrivateHosts: true, // httptest is 127.0.0.1 (loopback)
		Client:            srv.Client(),
		DefaultBudget:     budget,
	}
	if err := scrape.Register(reg, grave, cfg); err != nil {
		t.Fatalf("register scrape: %v", err)
	}
	return reg
}

func runCrawl(t *testing.T, reg *prizm.Registry, runID, seed string, budget scrape.Budget) scrape.Out {
	t.Helper()
	job := scrape.JobSpec{RunID: runID, Goal: "collect study materials", Seeds: []string{seed}, Budget: budget}
	data, err := prizm.EncodeData(scrape.In{Job: &job})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := reg.Route(ctx, prizm.Request{Header: prizm.Header{Kind: scrape.Kind, ID: runID}, Data: data})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	out, err := prizm.DecodeData[scrape.Out](resp.Data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestAgenticCrawl(t *testing.T) {
	srv := testServer(t)
	grave := graveyard.NewMemory()
	budget := scrape.Budget{MaxPages: 10, MaxDepth: 2, MaxFanOut: 8, MaxBytes: 10 << 20, MaxFileBytes: 1 << 20, DeadlineMS: 15000}
	reg := newReg(t, srv, grave, budget)

	out := runCrawl(t, reg, "run-a", srv.URL+"/", budget)

	if out.Status != "ok" {
		t.Errorf("status = %q, want ok", out.Status)
	}
	// hub md + Article_One md + Article_Two md + slides.pdf = 4. The robots-blocked /private
	// page and the off-domain evil link must NOT appear; pic.png (download index 1) is not selected.
	if len(out.Artifacts) != 4 {
		t.Fatalf("artifacts = %d, want 4: %+v", len(out.Artifacts), out.Artifacts)
	}

	var mdCount, pdfCount int
	seen := map[string]bool{}
	for _, a := range out.Artifacts {
		if seen[a.URL] {
			t.Errorf("duplicate artifact URL (dedupe failed): %s", a.URL)
		}
		seen[a.URL] = true
		if strings.Contains(a.URL, "evil.example.com") {
			t.Errorf("off-allowlist artifact leaked: %s", a.URL)
		}
		if strings.Contains(a.URL, "/private/") {
			t.Errorf("robots-disallowed artifact leaked: %s", a.URL)
		}
		if !kategorieValid(a.Kategorie) {
			t.Errorf("invalid kategorie %q on %s", a.Kategorie, a.URL)
		}
		if a.Ref == "" {
			t.Errorf("artifact %s has empty grave ref", a.URL)
		}
		switch {
		case strings.Contains(a.MediaType, "markdown"):
			mdCount++
		case strings.Contains(a.MediaType, "pdf"):
			pdfCount++
		}
	}
	if mdCount != 3 {
		t.Errorf("markdown artifacts = %d, want 3", mdCount)
	}
	if pdfCount != 1 {
		t.Errorf("pdf artifacts = %d, want 1", pdfCount)
	}

	// The stored bytes round-trip from the graveyard, and the hub markdown carries its text.
	var hubFound bool
	for _, a := range out.Artifacts {
		data, ok, err := grave.Get(context.Background(), a.Ref)
		if err != nil || !ok || len(data) == 0 {
			t.Errorf("grave.Get(%s) failed: ok=%v err=%v len=%d", a.URL, ok, err, len(data))
		}
		if strings.Contains(string(data), "Hub Page") {
			hubFound = true
		}
	}
	if !hubFound {
		t.Errorf("no stored artifact contained the hub page text")
	}

	if out.Pages < 3 {
		t.Errorf("pages = %d, want >= 3", out.Pages)
	}
}

func TestBudgetStopsCrawl(t *testing.T) {
	srv := testServer(t)
	grave := graveyard.NewMemory()
	// MaxPages=1: only the seed is reserved; no follow-link may be reserved, so no /wiki page
	// is fetched. The inline slides.pdf download is not a page reserve, so it still lands.
	budget := scrape.Budget{MaxPages: 1, MaxDepth: 3, MaxFanOut: 8, MaxBytes: 10 << 20, MaxFileBytes: 1 << 20, DeadlineMS: 15000}
	reg := newReg(t, srv, grave, budget)

	out := runCrawl(t, reg, "run-b", srv.URL+"/", budget)

	if out.Pages != 1 {
		t.Errorf("pages = %d, want 1 (MaxPages budget)", out.Pages)
	}
	for _, a := range out.Artifacts {
		if strings.Contains(a.URL, "/wiki/") {
			t.Errorf("MaxPages=1 but a followed page was fetched: %s", a.URL)
		}
	}
}

func kategorieValid(k string) bool {
	for _, v := range scrape.KATEGORIEN {
		if k == v {
			return true
		}
	}
	return false
}

// TestLiveOllamaCrawl exercises the REAL LLM-in-the-loop path: aigentic's actual choose router
// routes each decision to a live ollama model (no fake leaf). Opt-in — set SCRAPR_LIVE_OLLAMA=1
// and have ollama running with a pulled model. Content is model-dependent, so it asserts the
// loop completed and any produced artifact is well-formed, not exact counts.
func TestLiveOllamaCrawl(t *testing.T) {
	if os.Getenv("SCRAPR_LIVE_OLLAMA") == "" {
		t.Skip("set SCRAPR_LIVE_OLLAMA=1 (and run ollama) to exercise the real LLM loop")
	}
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://localhost:11434"
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Second)
	req, _ := http.NewRequestWithContext(pctx, http.MethodGet, host+"/api/tags", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil {
		pcancel()
		t.Skipf("ollama unreachable at %s: %v", host, err)
	} else {
		resp.Body.Close()
	}
	pcancel()

	srv := testServer(t)
	grave := graveyard.NewMemory()
	reg := prizm.NewRegistry(0)
	ollama := aigentic.OllamaConfig{BaseURL: host, Model: os.Getenv("SCRAPR_OLLAMA_MODEL")}
	if err := aigentic.Register(reg, grave, aigentic.Config{
		Ollama: ollama,
		Choose: aigentic.ChooseConfig{Classify: aigentic.OllamaClassifier(ollama, "")},
	}); err != nil {
		t.Fatalf("register aigentic: %v", err)
	}
	if err := scrape.Register(reg, grave, scrape.Config{RequestInterval: time.Millisecond, AllowPrivateHosts: true}); err != nil {
		t.Fatalf("register scrape: %v", err)
	}

	budget := scrape.Budget{MaxPages: 3, MaxDepth: 1, MaxFanOut: 4, MaxBytes: 10 << 20, MaxFileBytes: 1 << 20, DeadlineMS: 90000}
	out := runCrawl(t, reg, "live", srv.URL+"/", budget)

	if out.Status != "ok" && out.Status != "partial" {
		t.Errorf("status = %q, want ok/partial", out.Status)
	}
	if out.Pages < 1 {
		t.Errorf("pages = %d, want >= 1 (the seed ran through the real LLM)", out.Pages)
	}
	for _, a := range out.Artifacts {
		if !kategorieValid(a.Kategorie) {
			t.Errorf("live artifact bad kategorie %q on %s", a.Kategorie, a.URL)
		}
		if a.Ref == "" {
			t.Errorf("live artifact %s has no grave ref", a.URL)
		}
	}
	t.Logf("live ollama crawl: status=%s pages=%d artifacts=%d", out.Status, out.Pages, len(out.Artifacts))
}
