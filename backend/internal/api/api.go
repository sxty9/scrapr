// Package api serves scrapr's HTTP surface under /api/services/scrapr/, behind the shared
// holistic session (h_access JWT + CSRF double-submit + Linux-group rights). The JSON shapes
// match studiq's DataSource types (Scraper/Document/ScraperRun) so studiq's httpSource maps
// them 1:1. Error bodies follow holistic's contract: {"detail": "..."}. There is also an
// /internal/* surface guarded by a per-pair shared secret for a future studiqd daemon.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sxty9/prizm/graveyard"
	"github.com/sxty9/prizm/prizm"

	"scrapr/internal/auth"
	"scrapr/internal/rights"
	"scrapr/internal/store"
	"scrapr/scrape"
)

const (
	base    = "/api/services/scrapr/"
	service = "scrapr"
	version = "0.1.0"
)

// Server wires the verifier, the prizm registry (scrape + aigentic leaves), the graveyard and
// the metadata store into HTTP handlers.
type Server struct {
	v              *auth.Verifier
	reg            *prizm.Registry
	grave          graveyard.Graveyard
	store          *store.Store
	internalSecret string
}

// New builds a server.
func New(v *auth.Verifier, reg *prizm.Registry, grave graveyard.Graveyard, st *store.Store, internalSecret string) *Server {
	return &Server{v: v, reg: reg, grave: grave, store: st, internalSecret: internalSecret}
}

type handler func(w http.ResponseWriter, r *http.Request, u *auth.User)

// Handler returns the routed http.Handler (Go 1.22 method+path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+base+"health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET "+base+"info", s.guard("", false, s.info))

	// FUSE: scraper registry (studiq DataSource.scrapers/addScraper/updateScraper/triggerScraper).
	mux.HandleFunc("GET "+base+"scrapers", s.guard(rights.GroupUse, false, s.listScrapers))
	mux.HandleFunc("POST "+base+"scrapers", s.guard(rights.GroupRun, true, s.addScraper))
	mux.HandleFunc("POST "+base+"scrapers/{id}", s.guard(rights.GroupRun, true, s.updateScraper))
	mux.HandleFunc("PATCH "+base+"scrapers/{id}", s.guard(rights.GroupRun, true, s.updateScraper))
	mux.HandleFunc("POST "+base+"scrapers/{id}/trigger", s.guard(rights.GroupRun, true, s.triggerScraper))

	// FUSE: documents (studiq DataSource.documents/addManualDocument + content fetch).
	mux.HandleFunc("GET "+base+"documents", s.guard(rights.GroupUse, false, s.listDocuments))
	mux.HandleFunc("POST "+base+"documents", s.guard(rights.GroupRun, true, s.addManualDocument))
	mux.HandleFunc("GET "+base+"documents/{id}/content", s.guard(rights.GroupUse, false, s.documentContent))

	// Machine-to-machine surface for a future studiqd (shared secret, no session). Stubbed.
	mux.HandleFunc("GET "+base+"internal/health", s.guardInternal(s.internalHealth))
	mux.HandleFunc("POST "+base+"internal/scrape", s.guardInternal(s.internalScrape))

	return mux
}

// guard authenticates, optionally requires a fine-grained right, and optionally enforces CSRF.
func (s *Server) guard(perm string, csrf bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.v.User(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		if perm != "" && !u.Can(perm) {
			writeErr(w, http.StatusForbidden, "You do not have permission for this action")
			return
		}
		if csrf && !s.v.CheckCSRF(r) {
			writeErr(w, http.StatusForbidden, "CSRF check failed")
			return
		}
		h(w, r, u)
	}
}

// info echoes the resolved identity and the registered processor kinds.
func (s *Server) info(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	kinds := make([]string, 0)
	for _, k := range s.reg.Kinds() {
		kinds = append(kinds, string(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": service,
		"version": version,
		"user":    u.Username,
		"isAdmin": u.IsAdmin,
		"kinds":   kinds,
	})
}

// ── studiq-shaped JSON encoders ──────────────────────────────────────────────────────

func isoTime(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// scraperJSON renders a store.Scraper as studiq's Scraper type.
func scraperJSON(sc store.Scraper) map[string]any {
	m := map[string]any{
		"id":           sc.ID,
		"name":         sc.Name,
		"model":        sc.Model,
		"source":       sc.Source,
		"scheduleKind": sc.ScheduleKind,
		"enabled":      sc.Enabled,
	}
	if sc.ScheduleCustom != "" {
		m["scheduleCustom"] = sc.ScheduleCustom
	}
	if sc.LastRunStatus != "" && sc.LastRunStatus != "never" {
		m["lastRun"] = map[string]any{
			"at":     isoTime(sc.LastRunAt),
			"status": sc.LastRunStatus,
			"added":  sc.LastRunAdded,
		}
	}
	return m
}

// documentJSON renders a store.Document as studiq's Document type.
func documentJSON(d store.Document) map[string]any {
	m := map[string]any{
		"id":        d.ID,
		"title":     d.Title,
		"kategorie": d.Kategorie,
		"source":    d.Source,
		"addedAt":   isoTime(d.Added),
	}
	if d.ScraperID != "" {
		m["scraperId"] = d.ScraperID
	}
	if d.ModuleID != "" {
		m["moduleId"] = d.ModuleID
	}
	return m
}

func documentJSONList(ds []store.Document) []map[string]any {
	out := make([]map[string]any, 0, len(ds))
	for _, d := range ds {
		out = append(out, documentJSON(d))
	}
	return out
}

// goalFor derives the LLM crawl objective from a scraper (its explicit Goal, or a synthesized one).
func goalFor(sc store.Scraper) string {
	if sc.Goal != "" {
		return sc.Goal
	}
	name := sc.Name
	if name == "" {
		name = "this source"
	}
	return "Collect study-relevant materials (documents, slides, exercises, notes, media, images) for \"" +
		name + "\". Extract each page's readable content and download relevant files."
}

// jobBudget is the crawl budget for a scraper run (M1 uses scrapr's defaults).
func jobBudget() scrape.Budget { return scrape.Budget{} } // zero => processor default

// graveRef converts a stored ref string to the graveyard's opaque Ref type.
func graveRef(s string) graveyard.Ref { return graveyard.Ref(s) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
