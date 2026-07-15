package api

import (
	"crypto/subtle"
	"net/http"
)

// internalHeader carries the per-pair shared secret on daemon→daemon calls (no session).
const internalHeader = "X-Scrapr-Internal"

// guardInternal gates the /internal/* surface on a constant-time shared-secret comparison. It
// fails CLOSED: if scrapr has no internal secret configured, every internal call is rejected
// (a partial deployment can't be abused). Mirrors notify's internalEmit gate.
func (s *Server) guardInternal(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.internalSecret == "" {
			writeErr(w, http.StatusForbidden, "Internal API disabled (no shared secret configured)")
			return
		}
		got := r.Header.Get(internalHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.internalSecret)) != 1 {
			writeErr(w, http.StatusForbidden, "Bad internal secret")
			return
		}
		h(w, r)
	}
}

// internalHealth is the unauthenticated-by-session health for a peer daemon.
func (s *Server) internalHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// internalScrape is the future studiqd→scrapr entrypoint: it will accept a JobSpec plus a
// caller-designated lakearch StoreRef and run a crawl into the caller's store. Stubbed (501)
// until M2 (needs the lakearchd gRPC client for a shared, single-writer store).
func (s *Server) internalScrape(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, http.StatusNotImplemented, "internal/scrape lands in M2 (caller-designated lakearch store)")
}
