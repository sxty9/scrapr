// Command scraprd is the scrapr service daemon: an agentic web scraper exposed under
// /api/services/scrapr/. It validates the shared holistic session (a signed JWT in the
// h_access cookie) without any RPC to the holistic backend and enforces the holistic rights
// standard. It runs unprivileged behind the holistic Caddy proxy.
//
// It builds one prizm registry holding aigentic's LLM processors (ollama/claude-cli/claude-api
// + the choose router) AND scrapr's own "scrape" processor. The scrape processor owns the
// crawl (fetch/parse/download) and reasons by spawning the choose router as a sub-prizm. Bytes
// go to the graveyard (in-memory by default; lakearch with -tags lakearch); metadata to SQLite.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sxty9/aigentic/aigentic"
	"github.com/sxty9/prizm/prizm"

	"scrapr/internal/api"
	"scrapr/internal/auth"
	"scrapr/internal/grave"
	"scrapr/internal/store"
	"scrapr/scrape"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8782", "address to listen on")
	maxDepth := flag.Int("max-depth", prizm.DefaultMaxDepth, "sub-prizm recursion ceiling")
	flag.Parse()

	secret, err := auth.LoadSecret()
	if err != nil {
		log.Fatalf("scraprd: %v", err)
	}
	// Admin = membership in this group (the single Linux source of truth). The systemd unit
	// sets SCRAPR_ADMIN_GROUP; the verifier defaults to "sudo" when it is empty.
	v := auth.NewVerifier(secret, os.Getenv("SCRAPR_ADMIN_GROUP"))

	// Graveyard (artifact bytes): memory by default; lakearch (cgo) with -tags lakearch.
	g, err := grave.Open(os.Getenv("SCRAPR_GRAVEYARD"), os.Getenv("SCRAPR_GRAVEYARD_DIR"))
	if err != nil {
		log.Fatalf("scraprd: graveyard: %v", err)
	}

	// Metadata index (scrapers/runs/documents).
	st, err := store.Open(os.Getenv("SCRAPR_DB"))
	if err != nil {
		log.Fatalf("scraprd: store: %v", err)
	}
	defer st.Close()

	// One registry hosts the aigentic LLM leaves + choose router AND the scrape processor.
	reg := prizm.NewRegistry(*maxDepth)
	if err := aigentic.Register(reg, g, aigenticConfigFromEnv()); err != nil {
		log.Fatalf("scraprd: aigentic: %v", err)
	}
	if err := scrape.Register(reg, g, scrapeConfigFromEnv()); err != nil {
		log.Fatalf("scraprd: scrape: %v", err)
	}

	srv := &http.Server{
		Handler:           api.New(v, reg, g, st, internalSecret()).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Bind synchronously so an "address in use" surfaces here, not in a goroutine.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("scraprd: listen %s: %v", *listen, err)
	}
	go func() {
		log.Printf("scraprd listening on %s", *listen)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("scraprd: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Print("scraprd stopped")
}

// aigenticConfigFromEnv wires the LLM engines from the environment. Engines self-report
// unavailability when their backing service/secret is absent, so a partial environment still
// runs — the choose router falls back down the available chain. For M1 the Anthropic key is a
// static env value (no per-user secret store yet); Moodle-phase credentials arrive in M3.
func aigenticConfigFromEnv() aigentic.Config {
	ollama := aigentic.OllamaConfig{
		BaseURL: os.Getenv("OLLAMA_HOST"),
		Model:   os.Getenv("SCRAPR_OLLAMA_MODEL"),
	}
	return aigentic.Config{
		MaxTokens: atoi(os.Getenv("SCRAPR_MAX_TOKENS")),
		Ollama:    ollama,
		ClaudeCLI: aigentic.ClaudeCLIConfig{
			Bin:   os.Getenv("AIGENTIC_CLAUDE_BIN"),
			Model: os.Getenv("SCRAPR_CLAUDE_CLI_MODEL"),
		},
		ClaudeAPI: aigentic.ClaudeAPIConfig{
			BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
			APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
			Model:   os.Getenv("SCRAPR_CLAUDE_MODEL"),
		},
		Choose: aigentic.ChooseConfig{
			// Classify with a cheap ollama call; falls back to a deterministic heuristic when
			// ollama is unreachable, so this is safe even with ollama absent.
			Classify: aigentic.OllamaClassifier(ollama, os.Getenv("SCRAPR_CLASSIFIER_MODEL")),
		},
	}
}

// scrapeConfigFromEnv configures the crawl engine. SCRAPR_ENGINE pins the LLM leaf (ollama|
// claude-cli|claude-api) for the decision call; empty lets the choose router estimate.
func scrapeConfigFromEnv() scrape.Config {
	return scrape.Config{
		UserAgent:       os.Getenv("SCRAPR_USER_AGENT"),
		DecideModel:     os.Getenv("SCRAPR_DECIDE_MODEL"),
		ForceEngine:     prizm.Kind(strings.TrimSpace(os.Getenv("SCRAPR_ENGINE"))),
		MaxDecideTokens: atoi(os.Getenv("SCRAPR_DECIDE_TOKENS")),
	}
}

// internalSecret is the per-pair shared secret for the /internal/* surface (studiqd → scrapr).
// Empty (unset) disables the internal API — it fails closed.
func internalSecret() string {
	if s := os.Getenv("SCRAPR_INTERNAL_SECRET"); s != "" {
		return strings.TrimSpace(s)
	}
	if p := os.Getenv("SCRAPR_INTERNAL_SECRET_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
