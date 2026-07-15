package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/sxty9/aigentic/aigentic"
	"github.com/sxty9/prizm/graveyard"
	"github.com/sxty9/prizm/prizm"

	"scrapr/internal/api"
	"scrapr/internal/auth"
	"scrapr/internal/store"
	"scrapr/scrape"
)

const testSecret = "test-shared-jwt-secret"

// fakeChoose returns a fixed keep+download action so the API path is exercised without a real LLM.
func fakeChoose() prizm.Processor {
	const action = `{"extract":{"keep":true,"title":"Kept","kategorie":"Quellen","summary":"s"},` +
		`"download":[{"index":0,"kategorie":"Foliensatz","reason":"pdf"}],"follow":[],"done":true}`
	return prizm.NewTyped(func(_ context.Context, _ aigentic.Request, _ prizm.Env) (aigentic.Result, error) {
		return aigentic.Result{Output: action, Engine: aigentic.KindChoose}, nil
	})
}

func contentServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
	})
	mux.HandleFunc("/notes.pdf", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 course notes"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Course</title></head><body><h1>Course</h1>
<p>Discrete mathematics materials.</p><a href="/notes.pdf">Notes</a></body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// setup builds the full daemon handler over a fake choose leaf + memory grave + temp store.
func setup(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	me, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve current user: %v", err)
	}
	gids, err := me.GroupIds()
	if err != nil || len(gids) == 0 {
		t.Skipf("cannot resolve current user's groups")
	}
	g, err := user.LookupGroupId(gids[0])
	if err != nil {
		t.Skipf("cannot resolve group name: %v", err)
	}
	// Admin = one of the caller's real groups, so the minted session is admin and holds every
	// right (isAdmin || group ∈ groups) without needing the hp_scrapr_* groups on this host.
	v := auth.NewVerifier([]byte(testSecret), g.Name)

	grave := graveyard.NewMemory()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	reg := prizm.NewRegistry(0)
	if err := reg.Register(aigentic.KindChoose, prizm.NewPrizm(fakeChoose(), grave)); err != nil {
		t.Fatalf("register choose: %v", err)
	}
	if err := scrape.Register(reg, grave, scrape.Config{
		RequestInterval: time.Millisecond, AllowPrivateHosts: true,
	}); err != nil {
		t.Fatalf("register scrape: %v", err)
	}

	ts := httptest.NewServer(api.New(v, reg, grave, st, "").Handler())
	t.Cleanup(ts.Close)
	return ts, me.Username
}

func mintToken(t *testing.T, sub string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  sub,
		"type": "access",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	s, err := tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// call performs an authenticated request (session cookie + CSRF double-submit).
func call(t *testing.T, ts *httptest.Server, token, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "h_access", Value: token})
	req.AddCookie(&http.Cookie{Name: "h_csrf", Value: "csrf1"})
	req.Header.Set("X-CSRF-Token", "csrf1")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func TestAPIEndToEnd(t *testing.T) {
	ts, sub := setup(t)
	token := mintToken(t, sub)
	content := contentServer(t)
	const base = "/api/services/scrapr/"

	// health is public.
	if resp, _ := call(t, ts, token, "GET", base+"health", ""); resp.StatusCode != 200 {
		t.Fatalf("health = %d", resp.StatusCode)
	}

	// create a scraper pointing at the local content server.
	resp, data := call(t, ts, token, "POST", base+"scrapers", `{"name":"Course","model":"website","source":"`+content.URL+`/","scheduleKind":"manual","enabled":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("addScraper = %d: %s", resp.StatusCode, data)
	}
	var sc map[string]any
	mustJSON(t, data, &sc)
	id, _ := sc["id"].(string)
	if id == "" {
		t.Fatalf("no scraper id: %s", data)
	}

	// trigger the crawl.
	resp, data = call(t, ts, token, "POST", base+"scrapers/"+id+"/trigger", "")
	if resp.StatusCode != 200 {
		t.Fatalf("trigger = %d: %s", resp.StatusCode, data)
	}
	var run struct {
		ScraperID string `json:"scraperId"`
		Status    string `json:"status"`
		Added     int    `json:"added"`
		Documents []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Kategorie string `json:"kategorie"`
			Source    string `json:"source"`
		} `json:"documents"`
	}
	mustJSON(t, data, &run)
	if run.Added < 2 || len(run.Documents) < 2 {
		t.Fatalf("run added=%d docs=%d, want >=2 (page md + pdf): %s", run.Added, len(run.Documents), data)
	}
	for _, d := range run.Documents {
		if d.Source != "scraper" {
			t.Errorf("doc source = %q, want scraper", d.Source)
		}
		if !validKat(d.Kategorie) {
			t.Errorf("doc kategorie invalid: %q", d.Kategorie)
		}
	}

	// list documents.
	resp, data = call(t, ts, token, "GET", base+"documents", "")
	if resp.StatusCode != 200 {
		t.Fatalf("documents = %d: %s", resp.StatusCode, data)
	}
	var docs []map[string]any
	mustJSON(t, data, &docs)
	if len(docs) < 2 {
		t.Fatalf("documents list = %d, want >=2", len(docs))
	}

	// fetch one document's content from the graveyard.
	docID, _ := docs[0]["id"].(string)
	resp, data = call(t, ts, token, "GET", base+"documents/"+docID+"/content", "")
	if resp.StatusCode != 200 || len(data) == 0 {
		t.Fatalf("content = %d len=%d", resp.StatusCode, len(data))
	}

	// unauthenticated request is rejected.
	if resp, err := http.Get(ts.URL + base + "scrapers"); err == nil {
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no-auth scrapers = %d, want 401", resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func validKat(k string) bool {
	for _, v := range scrape.KATEGORIEN {
		if k == v {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("json: %v (%s)", err, data)
	}
}
