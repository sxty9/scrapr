// Package scrape is scrapr's core: a prizm processor (Kind "scrape") that performs a full
// agentic, LLM-driven crawl. scrapr is "the hands" — it fetches pages, parses HTML, follows
// links and downloads assets — while aigentic's "choose" router is "the brain": for every
// page the LLM decides which links to follow, what to download, and what to extract, as a
// strict JSON action. The processor supplies enumerated candidates and validates every LLM
// proposal, so the model can only pick from what scrapr found (it can never invent an
// off-domain URL), and hard per-run budgets guarantee termination regardless of the output.
//
// A crawl is a coordinator/worker split over ONE registry (in-process sub-prizms):
//   - Job  request → coordinator: BFS over the frontier, budget/dedup/allowlist in runState,
//     fan-out of pages via subprizm.SpawnAll.
//   - Page request → worker: fetch → parse → decide (spawns "choose") → act (Put artifacts,
//     return accepted follow-links to the coordinator).
//
// prizm recursion stays flat — coordinator(0) → worker(1) → choose(2) → leaf(3) — because
// crawl depth is a runState counter, not prizm recursion (cf. prizm/echo breadth cap).
package scrape

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sxty9/prizm/graveyard"
	"github.com/sxty9/prizm/prizm"
)

// Kind is the routing discriminator the scrape processor registers under.
const Kind prizm.Kind = "scrape"

const (
	// maxFanOutCeiling bounds how many follow-links a single page may expand — the
	// processor's own breadth guard (depth is bounded by the registry). Mirrors echo.maxFanOut.
	maxFanOutCeiling = 64
	// workerConcurrency bounds concurrent page workers within one crawl level.
	workerConcurrency = 4
	// defaultUserAgent identifies scraprbot to origins and robots.txt.
	defaultUserAgent = "scraprbot/0.1 (+https://holistic.local/about/scrapr)"
	// defaultDecideTokens caps the answer tokens for one LLM decision.
	defaultDecideTokens = 1024
)

// maxCategoryLen caps a free-form (model-authored) category label.
const maxCategoryLen = 40

// In is a scrape request. Exactly one of Job (top-level coordinator) or Page (fanned-out
// worker) is set — the tagged union that lets one Kind serve both roles.
type In struct {
	Job  *JobSpec  `json:"job,omitempty"`
	Page *PageTask `json:"page,omitempty"`
}

// JobSpec is one crawl: one studiq Scraper maps to one of these.
// scrapr is domain-agnostic: it imposes no taxonomy. Categories is an OPTIONAL vocabulary the
// caller supplies (e.g. studiq passes its learning taxonomy); when set, the LLM must classify
// each artifact into one of them, else it assigns a free-form label.
type JobSpec struct {
	RunID      string   `json:"runId"`
	Goal       string   `json:"goal"`       // natural-language objective shown to the LLM
	Seeds      []string `json:"seeds"`      // starting URLs (from Scraper.source)
	Allow      []string `json:"allow"`      // registrable-domain allowlist; derived from seeds when empty
	Categories []string `json:"categories"` // optional category vocabulary; empty => free-form
	Budget     Budget   `json:"budget"`     // hard limits; zero fields fall back to the config default
}

// PageTask is one page for a worker to process.
type PageTask struct {
	RunID string `json:"runId"`
	URL   string `json:"url"`
	Depth int    `json:"depth"` // CRAWL depth (a runState counter, not prizm recursion)
}

// Budget is the hard termination guard: the LLM may propose, but these dispose.
type Budget struct {
	MaxPages     int   `json:"maxPages"`
	MaxDepth     int   `json:"maxDepth"`
	MaxFanOut    int   `json:"maxFanOut"`
	MaxBytes     int64 `json:"maxBytes"`
	MaxFileBytes int64 `json:"maxFileBytes"`
	DeadlineMS   int64 `json:"deadlineMs"`
}

// DefaultBudget is scrapr's conservative default for a crawl (also used to fill zero fields).
func DefaultBudget() Budget {
	return Budget{
		MaxPages:     25,
		MaxDepth:     2,
		MaxFanOut:    8,
		MaxBytes:     32 << 20,
		MaxFileBytes: 8 << 20,
		DeadlineMS:   90_000,
	}
}

// Out is a scrape result. From the coordinator it is the whole crawl; from a worker it is one
// page's artifacts plus the follow-links it accepts (Follow is worker→coordinator only).
type Out struct {
	RunID     string     `json:"runId,omitempty"`
	Status    string     `json:"status,omitempty"` // "ok" | "partial"
	Pages     int        `json:"pages,omitempty"`
	Bytes     int64      `json:"bytes,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Follow    []PageTask `json:"follow,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Artifact is one stored piece of scraped content: its bytes live in the graveyard under Ref
// (a lakearch BLAKE3 content-id, or a sha256 in the memory backend), the rest is metadata.
type Artifact struct {
	URL       string        `json:"url"`
	Title     string        `json:"title"`
	Kategorie string        `json:"kategorie"`
	MediaType string        `json:"mediaType"`
	Bytes     int64         `json:"bytes"`
	Ref       graveyard.Ref `json:"ref"`
}

// Config configures the scrape processor.
type Config struct {
	UserAgent       string        // robots + request UA; "" => defaultUserAgent
	DecideModel     string        // aigentic Request.Model for the decision call; "" => engine default
	ForceEngine     prizm.Kind    // pin the choose router to a leaf (KindOllama|KindClaudeCLI|KindClaudeAPI); "" => estimate
	MaxDecideTokens int           // answer-token cap for one decision; 0 => defaultDecideTokens
	RequestInterval time.Duration // min interval between requests to one host; 0 => 700ms
	Client          *http.Client  // injectable HTTP client (tests point it at httptest)
	DefaultBudget   Budget        // zero => DefaultBudget()
	// AllowPrivateHosts permits fetching assets from loopback/private/link-local addresses.
	// Default false (SSRF-safe: a public page can't make scrapr reach internal hosts). Set true
	// for tests (httptest is 127.0.0.1) or to scrape an internal Moodle on a private network.
	AllowPrivateHosts bool
}

// processor holds crawl state shared across coordinator + workers of the same registry.
type processor struct {
	cfg     Config
	fetch   Fetcher
	robots  *robotsCache
	limiter *hostLimiter
	runs    *runRegistry // per-RunID shared state, looked up by workers
}

func newProcessor(cfg Config) *processor {
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.MaxDecideTokens <= 0 {
		cfg.MaxDecideTokens = defaultDecideTokens
	}
	if cfg.DefaultBudget == (Budget{}) {
		cfg.DefaultBudget = DefaultBudget()
	}
	f := newHTTPFetcher(cfg)
	return &processor{
		cfg:     cfg,
		fetch:   f,
		robots:  newRobotsCache(f, cfg.UserAgent),
		limiter: newHostLimiter(cfg.RequestInterval),
		runs:    newRunRegistry(),
	}
}

// Register builds the scrape processor over grave and registers it under Kind, WithSpawner so
// it can reach aigentic's choose router (and its leaves) as sub-prizms.
func Register(reg *prizm.Registry, grave graveyard.Graveyard, cfg Config) error {
	p := newProcessor(cfg)
	return reg.Register(Kind, prizm.NewPrizm(prizm.NewTyped(p.process), grave, prizm.WithSpawner(reg)))
}

// process dispatches on the tagged union: Job → coordinator, Page → worker.
func (p *processor) process(ctx context.Context, in In, env prizm.Env) (Out, error) {
	switch {
	case in.Job != nil:
		return p.coordinate(ctx, *in.Job, env)
	case in.Page != nil:
		return p.work(ctx, *in.Page, env)
	default:
		return Out{}, fmt.Errorf("%w: scrape request has neither job nor page", prizm.ErrInvalidRequest)
	}
}
