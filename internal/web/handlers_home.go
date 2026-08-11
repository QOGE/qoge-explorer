package web

import "net/http"

// handleHome resolves GET /. Shows the indexed database's own checkpoint
// (never Core's live tip — see query.Status's doc comment) and the most
// recent canonical blocks.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	status, err := s.q.Status(r.Context())
	if err != nil {
		s.renderInternalError(w, "status", err)
		return
	}
	page, err := s.q.RecentBlocks(r.Context(), nil, 10)
	if err != nil {
		s.renderInternalError(w, "recent blocks", err)
		return
	}
	s.render(w, "home", http.StatusOK, homeView{Status: status, RecentBlocks: page.Blocks})
}
