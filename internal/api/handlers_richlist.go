package api

import (
	"net/http"
)

// handleRichList resolves GET /api/v1/richlist: the top RichListLimit
// positive addresses.balance_satoshis rows. Unlike /api/v1/supply, there
// is no rollup-unavailable error path — an uninitialized or genesis-only
// explorer is a valid HTTP 200 with an empty entries list (see
// docs/ARCHITECTURE.md §29).
func (s *Server) handleRichList(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.RichListOverview(r.Context())
	if err != nil {
		writeInternalError(s.log, w, "richlist overview", err)
		return
	}
	writeJSON(w, overview)
}
