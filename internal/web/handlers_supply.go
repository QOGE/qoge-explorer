package web

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleSupply resolves GET /supply. An indexed chain with incomplete
// canonical block_accounting coverage renders a clear operator-facing HTTP
// 503 explanation rather than a partial/understated page — never pretends
// partial data is complete (docs/ARCHITECTURE.md's Phase 2H.2 section).
func (s *Server) handleSupply(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.SupplyOverview(r.Context())
	if errors.Is(err, query.ErrAccountingIncomplete) {
		s.renderError(w, http.StatusServiceUnavailable, "Service Unavailable",
			"Monetary accounting has not been fully backfilled for the indexed chain. An operator can resolve this by running \"qoge-explorer backfill-accounting\".")
		return
	}
	if err != nil {
		s.renderInternalError(w, "supply overview", err)
		return
	}
	s.render(w, "supply", http.StatusOK, supplyView{Overview: overview})
}
