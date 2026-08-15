package api

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleSupply resolves GET /api/v1/supply. An initialized indexed chain
// whose tip has no block_supply_rollup row yet (not backfilled, or a
// pre-0005 schema) returns HTTP 503 rather than an understated total —
// docs/ARCHITECTURE.md §28.
func (s *Server) handleSupply(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.SupplyOverview(r.Context())
	if errors.Is(err, query.ErrSupplyRollupUnavailable) {
		writeError(w, http.StatusServiceUnavailable, "supply_rollup_unavailable", "supply rollup is unavailable for the indexed tip")
		return
	}
	if err != nil {
		writeInternalError(s.log, w, "supply overview", err)
		return
	}
	writeJSON(w, overview)
}
