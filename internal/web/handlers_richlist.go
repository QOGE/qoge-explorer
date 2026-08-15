package web

import "net/http"

// handleRichList resolves GET /richlist. Unlike /supply, there is no
// rollup-unavailable error path — an uninitialized or genesis-only
// explorer is a valid 200 page with an empty table and an explanatory
// message (see docs/ARCHITECTURE.md §29).
func (s *Server) handleRichList(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.RichListOverview(r.Context())
	if err != nil {
		s.renderInternalError(w, "richlist overview", err)
		return
	}
	s.render(w, "richlist", http.StatusOK, richListView{Overview: overview})
}
