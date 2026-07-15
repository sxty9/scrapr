package scrape

import (
	"context"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// robotsCache fetches and caches robots.txt per origin and answers Allowed for scraprbot's
// UA. Honoring Disallow (and Crawl-delay) is the politeness contract for M1. On any fetch/parse
// failure it fails OPEN (allow) — a missing or broken robots.txt conventionally means no rules.
type robotsCache struct {
	fetch Fetcher
	token string // lowercased UA product token to match, e.g. "scraprbot"

	mu    sync.Mutex
	hosts map[string]*robotsRules
}

type robotsRules struct {
	rules      []robotRule // ordered as written; longest match wins
	crawlDelay time.Duration
}

type robotRule struct {
	pattern string
	allow   bool
}

func newRobotsCache(f Fetcher, ua string) *robotsCache {
	token := "scraprbot"
	if i := strings.IndexAny(ua, "/ "); i > 0 {
		token = strings.ToLower(ua[:i])
	} else if ua != "" {
		token = strings.ToLower(ua)
	}
	return &robotsCache{fetch: f, token: token, hosts: map[string]*robotsRules{}}
}

// allowed reports whether scraprbot may fetch raw per the origin's robots.txt.
func (rc *robotsCache) allowed(ctx context.Context, raw string) bool {
	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}
	rules := rc.rulesFor(ctx, u)
	if rules == nil {
		return true
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	best := -1
	allow := true // default allow when nothing matches
	for _, r := range rules.rules {
		if n, ok := robotsMatch(r.pattern, path); ok && n > best {
			best = n
			allow = r.allow
		}
	}
	return allow
}

// crawlDelay returns the Crawl-delay for raw's origin (0 if none/unknown).
func (rc *robotsCache) crawlDelay(ctx context.Context, raw string) time.Duration {
	u, err := neturl.Parse(raw)
	if err != nil {
		return 0
	}
	if rules := rc.rulesFor(ctx, u); rules != nil {
		return rules.crawlDelay
	}
	return 0
}

func (rc *robotsCache) rulesFor(ctx context.Context, u *neturl.URL) *robotsRules {
	origin := u.Scheme + "://" + u.Host
	rc.mu.Lock()
	cached, ok := rc.hosts[origin]
	rc.mu.Unlock()
	if ok {
		return cached
	}
	rules := rc.load(ctx, origin)
	rc.mu.Lock()
	rc.hosts[origin] = rules
	rc.mu.Unlock()
	return rules
}

func (rc *robotsCache) load(ctx context.Context, origin string) *robotsRules {
	fetched, err := rc.fetch.Fetch(ctx, origin+"/robots.txt", 512<<10)
	if err != nil || fetched == nil || len(fetched.Body) == 0 {
		return nil // fail open
	}
	return parseRobots(string(fetched.Body), rc.token)
}

// parseRobots extracts the rules applying to token, preferring an exact UA group over "*".
func parseRobots(body, token string) *robotsRules {
	type group struct {
		rules []robotRule
		delay time.Duration
	}
	groups := map[string]*group{}
	var current []string   // agents of the group currently being defined
	startingGroup := false // true right after a User-agent line, before any rule

	ensure := func(agent string) *group {
		g := groups[agent]
		if g == nil {
			g = &group{}
			groups[agent] = g
		}
		return g
	}

	for _, line := range strings.Split(body, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "user-agent":
			if !startingGroup {
				current = nil
				startingGroup = true
			}
			current = append(current, strings.ToLower(val))
		case "disallow", "allow":
			startingGroup = false
			for _, a := range current {
				g := ensure(a)
				// An empty Disallow means "allow all" — record it as an allow of "/".
				pat := val
				allow := key == "allow"
				if key == "disallow" && val == "" {
					pat, allow = "/", true
				}
				g.rules = append(g.rules, robotRule{pattern: pat, allow: allow})
			}
		case "crawl-delay":
			startingGroup = false
			if d, err := strconv.ParseFloat(val, 64); err == nil && d > 0 {
				for _, a := range current {
					ensure(a).delay = time.Duration(d * float64(time.Second))
				}
			}
		}
	}

	g := groups[token]
	if g == nil {
		g = groups["*"]
	}
	if g == nil {
		return &robotsRules{}
	}
	return &robotsRules{rules: g.rules, crawlDelay: g.delay}
}

// robotsMatch reports whether path matches a robots pattern (supporting '*' wildcards and a
// trailing '$' end-anchor), and the pattern's length as the specificity score.
func robotsMatch(pattern, path string) (int, bool) {
	score := len(pattern)
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	segs := strings.Split(pattern, "*")
	pos := 0
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		idx := strings.Index(path[pos:], seg)
		if idx < 0 {
			return 0, false
		}
		if i == 0 && idx != 0 {
			return 0, false // first literal must anchor at the start
		}
		pos += idx + len(seg)
	}
	if anchored && pos != len(path) {
		return 0, false
	}
	return score, true
}

// hostLimiter enforces a polite per-host request rate across concurrent workers.
type hostLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	base     time.Duration
}

func newHostLimiter(base time.Duration) *hostLimiter {
	if base <= 0 {
		base = 700 * time.Millisecond
	}
	return &hostLimiter{limiters: map[string]*rate.Limiter{}, base: base}
}

// wait blocks until the host's rate limiter permits a request. minInterval (e.g. a robots
// Crawl-delay) slows the host below the default when larger.
func (h *hostLimiter) wait(ctx context.Context, host string, minInterval time.Duration) error {
	interval := h.base
	if minInterval > interval {
		interval = minInterval
	}
	h.mu.Lock()
	l := h.limiters[host]
	if l == nil {
		l = rate.NewLimiter(rate.Every(interval), 1)
		h.limiters[host] = l
	}
	h.mu.Unlock()
	return l.Wait(ctx)
}
