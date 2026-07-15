package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/sxty9/aigentic/aigentic"
	"github.com/sxty9/prizm/prizm"

	"scrapr/internal/auth"
	"scrapr/internal/store"
	"scrapr/scrape"
)

const maxBody = 64 << 10

// listScrapers → studiq DataSource.scrapers().
func (s *Server) listScrapers(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	scs, err := s.store.Scrapers(u.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not list scrapers")
		return
	}
	out := make([]map[string]any, 0, len(scs))
	for _, sc := range scs {
		out = append(out, scraperJSON(sc))
	}
	writeJSON(w, http.StatusOK, out)
}

// addScraper → studiq DataSource.addScraper(input). input = Omit<Scraper,'id'|'lastRun'>.
func (s *Server) addScraper(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var body struct {
		Name           string   `json:"name"`
		Model          string   `json:"model"`
		Source         string   `json:"source"`
		ScheduleKind   string   `json:"scheduleKind"`
		ScheduleCustom string   `json:"scheduleCustom"`
		Enabled        *bool    `json:"enabled"`
		Categories     []string `json:"categories"` // optional taxonomy the caller imposes; empty => free-form
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	sc, err := s.store.AddScraper(store.Scraper{
		Owner:          u.Username,
		Name:           body.Name,
		Model:          body.Model,
		Source:         body.Source,
		ScheduleKind:   body.ScheduleKind,
		ScheduleCustom: body.ScheduleCustom,
		Enabled:        enabled,
		Categories:     body.Categories,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create scraper")
		return
	}
	writeJSON(w, http.StatusOK, scraperJSON(sc))
}

// updateScraper → studiq DataSource.updateScraper(id, patch). Only present fields change.
func (s *Server) updateScraper(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id := r.PathValue("id")
	var body struct {
		Name           *string   `json:"name"`
		Model          *string   `json:"model"`
		Source         *string   `json:"source"`
		ScheduleKind   *string   `json:"scheduleKind"`
		ScheduleCustom *string   `json:"scheduleCustom"`
		Enabled        *bool     `json:"enabled"`
		Categories     *[]string `json:"categories"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	sc, err := s.store.UpdateScraper(u.Username, id, store.ScraperPatch{
		Name:           body.Name,
		Model:          body.Model,
		Source:         body.Source,
		ScheduleKind:   body.ScheduleKind,
		ScheduleCustom: body.ScheduleCustom,
		Enabled:        body.Enabled,
		Categories:     body.Categories,
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "No such scraper")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not update scraper")
		return
	}
	writeJSON(w, http.StatusOK, scraperJSON(sc))
}

// triggerScraper → studiq DataSource.triggerScraper(id): run the agentic crawl SYNCHRONOUSLY,
// persist a run + one document per stored artifact, and return the ScraperRun.
func (s *Server) triggerScraper(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id := r.PathValue("id")
	sc, err := s.store.Scraper(u.Username, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "No such scraper")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load scraper")
		return
	}
	if sc.Source == "" {
		writeErr(w, http.StatusBadRequest, "Scraper has no source URL")
		return
	}

	runID, err := s.store.StartRun(u.Username, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start run")
		return
	}

	job := scrape.JobSpec{
		RunID:      runID,
		Goal:       goalFor(sc),
		Seeds:      []string{sc.Source},
		Allow:      sc.Allow,
		Categories: sc.Categories,
		Budget:     jobBudget(),
	}
	data, err := prizm.EncodeData(scrape.In{Job: &job})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not encode job")
		return
	}

	resp, rerr := s.reg.Route(r.Context(), prizm.Request{
		Header: prizm.Header{Kind: scrape.Kind, Subject: u.Username, ID: runID},
		Data:   data,
	})
	if rerr != nil {
		_ = s.store.FinishRun(store.Run{ID: runID, ScraperID: id, Owner: u.Username, Status: "error", Error: rerr.Error()})
		status, detail := statusForErr(rerr)
		writeErr(w, status, detail)
		return
	}

	out, derr := prizm.DecodeData[scrape.Out](resp.Data)
	if derr != nil {
		_ = s.store.FinishRun(store.Run{ID: runID, ScraperID: id, Owner: u.Username, Status: "error", Error: derr.Error()})
		writeErr(w, http.StatusBadGateway, "Could not decode scrape result")
		return
	}

	docs := make([]store.Document, 0, len(out.Artifacts))
	for _, a := range out.Artifacts {
		d, ierr := s.store.InsertDocument(store.Document{
			RunID:       runID,
			ScraperID:   id,
			Owner:       u.Username,
			Title:       a.Title,
			Kategorie:   a.Kategorie,
			Source:      "scraper",
			URL:         a.URL,
			MediaType:   a.MediaType,
			Bytes:       a.Bytes,
			LakearchRef: string(a.Ref),
		})
		if ierr == nil {
			docs = append(docs, d)
		}
	}

	_ = s.store.FinishRun(store.Run{
		ID: runID, ScraperID: id, Owner: u.Username,
		Status: "ok", Added: len(docs), Pages: out.Pages, Bytes: out.Bytes,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"scraperId": id,
		"at":        time.Now().UTC().Format(time.RFC3339),
		"status":    "ok",
		"added":     len(docs),
		"documents": documentJSONList(docs),
	})
}

// statusForErr maps a crawl/registry error to an HTTP status + detail (mirrors aigentic's run).
func statusForErr(err error) (int, string) {
	switch {
	case errors.Is(err, aigentic.ErrProcessorUnavailable):
		return http.StatusServiceUnavailable, "No scraping engine available — configure ollama or set ANTHROPIC_API_KEY"
	case errors.Is(err, prizm.ErrInvalidRequest):
		return http.StatusBadRequest, "Invalid scrape request"
	case errors.Is(err, prizm.ErrDepthExceeded):
		return http.StatusUnprocessableEntity, "Crawl recursion limit exceeded"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusGatewayTimeout, "Crawl timed out"
	default:
		return http.StatusBadGateway, "Crawl failed"
	}
}

// decodeJSON reads a bounded JSON body into v; on failure it writes a 400 and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return false
	}
	return true
}
