package api

import (
	"encoding/base64"
	"errors"
	"net/http"

	"scrapr/internal/auth"
	"scrapr/internal/store"
)

// listDocuments → studiq DataSource.documents().
func (s *Server) listDocuments(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	docs, err := s.store.Documents(u.Username, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not list documents")
		return
	}
	writeJSON(w, http.StatusOK, documentJSONList(docs))
}

// addManualDocument → studiq DataSource.addManualDocument(input). Metadata-only in studiq;
// optionally accepts base64 content to store in the graveyard.
func (s *Server) addManualDocument(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var body struct {
		Title      string `json:"title"`
		Kategorie  string `json:"kategorie"`
		ScraperID  string `json:"scraperId"`
		ModuleID   string `json:"moduleId"`
		ContentB64 string `json:"contentB64"`
		MediaType  string `json:"mediaType"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Title == "" {
		writeErr(w, http.StatusBadRequest, "Title is required")
		return
	}
	doc := store.Document{
		Owner:     u.Username,
		Title:     body.Title,
		Kategorie: body.Kategorie,
		Source:    "manual",
		ScraperID: body.ScraperID,
		ModuleID:  body.ModuleID,
		MediaType: body.MediaType,
	}
	if body.ContentB64 != "" {
		raw, err := base64.StdEncoding.DecodeString(body.ContentB64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Invalid base64 content")
			return
		}
		ref, perr := s.grave.Put(r.Context(), "", raw)
		if perr != nil {
			writeErr(w, http.StatusInternalServerError, "Could not store content")
			return
		}
		doc.LakearchRef = string(ref)
		doc.Bytes = int64(len(raw))
	}
	d, err := s.store.InsertDocument(doc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create document")
		return
	}
	writeJSON(w, http.StatusOK, documentJSON(d))
}

// documentContent streams a document's bytes from the graveyard (owner-scoped).
func (s *Server) documentContent(w http.ResponseWriter, r *http.Request, u *auth.User) {
	id := r.PathValue("id")
	doc, err := s.store.Document(u.Username, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "No such document")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load document")
		return
	}
	if doc.LakearchRef == "" {
		writeErr(w, http.StatusNotFound, "Document has no stored content")
		return
	}
	data, found, gerr := s.grave.Get(r.Context(), graveRef(doc.LakearchRef))
	if gerr != nil {
		writeErr(w, http.StatusInternalServerError, "Could not read content")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "Content not found in store")
		return
	}
	ct := doc.MediaType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
