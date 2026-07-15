package scrape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Fetched is one HTTP response, body-capped. FinalURL is the post-redirect URL (re-checked
// against the allowlist by the caller).
type Fetched struct {
	URL         string
	FinalURL    string
	ContentType string
	Status      int
	Body        []byte
}

// Fetcher retrieves a URL. It is an interface so the Moodle phase can slot a chromedp-backed
// implementation in behind the same seam without touching the loop, and so tests inject a stub.
type Fetcher interface {
	Fetch(ctx context.Context, url string, maxBytes int64) (*Fetched, error)
}

// httpFetcher is the M1 plain-HTTP fetcher: stdlib net/http with a bounded redirect chain and
// a scraprbot User-Agent. No JS execution (fine for Wikipedia; Moodle needs the chrome fetcher).
type httpFetcher struct {
	client *http.Client
	ua     string
}

func newHTTPFetcher(cfg Config) *httpFetcher {
	client := cfg.Client
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return nil
			},
		}
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	return &httpFetcher{client: client, ua: ua}
}

func (f *httpFetcher) Fetch(ctx context.Context, url string, maxBytes int64) (*Fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,*/*;q=0.8")
	req.Header.Set("Accept-Language", "de,en;q=0.8")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape: %s: status %d", url, resp.StatusCode)
	}
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, err
	}
	final := url
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return &Fetched{
		URL:         url,
		FinalURL:    final,
		ContentType: resp.Header.Get("Content-Type"),
		Status:      resp.StatusCode,
		Body:        body,
	}, nil
}

// isHTML reports whether a Content-Type is an HTML document (so the worker parses+decides
// rather than storing the bytes as a raw asset).
func isHTML(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml")
}
