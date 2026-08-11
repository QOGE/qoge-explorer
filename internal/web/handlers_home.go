package web

import "net/http"

// handleHome resolves GET /. Shows the indexed database's own checkpoint
// (never Core's live tip — see query.Status's doc comment) and the most
// recent canonical blocks, read from ONE composite snapshot
// (query.Store.ExplorerOverview) so the rendered page can never pair a
// checkpoint from one canonical state with recent blocks from another —
// see docs/ARCHITECTURE.md §20 "Composite read snapshots".
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.ExplorerOverview(r.Context(), 10)
	if err != nil {
		s.renderInternalError(w, "explorer overview", err)
		return
	}
	s.render(w, "home", http.StatusOK, homeView{
		Status:       overview.Status,
		RecentBlocks: overview.RecentBlocks.Blocks,
	})
}
