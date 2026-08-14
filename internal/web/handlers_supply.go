package web

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleSupply resolves GET /supply. An initialized indexed chain whose
// tip has no block_supply_rollup row yet (not backfilled, or a pre-0005
// schema) renders a clear operator-facing HTTP 503 explanation rather than
// a partial/understated page — never pretends partial data is complete
// (docs/ARCHITECTURE.md §28).
func (s *Server) handleSupply(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.SupplyOverview(r.Context())
	if errors.Is(err, query.ErrSupplyRollupUnavailable) {
		s.renderError(w, http.StatusServiceUnavailable, "Service Unavailable",
			"Monetary rollup data is not available for the indexed tip. Ensure the database migrations are current and run \"qoge-explorer backfill-supply-rollup\". If block accounting has not yet been backfilled, run \"qoge-explorer backfill-accounting\" first.")
		return
	}
	if err != nil {
		s.renderInternalError(w, "supply overview", err)
		return
	}
	s.render(w, "supply", http.StatusOK, supplyView{Overview: overview})
}
