package api

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleSupply resolves GET /api/v1/supply. An indexed chain with
// incomplete canonical block_accounting coverage (a database upgraded from
// before Phase 2H.1 that has not yet run `backfill-accounting`) returns
// HTTP 503 rather than an understated total — docs/ARCHITECTURE.md's Phase
// 2H.2 section.
func (s *Server) handleSupply(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.SupplyOverview(r.Context())
	if errors.Is(err, query.ErrAccountingIncomplete) {
		writeError(w, http.StatusServiceUnavailable, "accounting_incomplete", "monetary accounting is incomplete for the indexed canonical chain")
		return
	}
	if err != nil {
		writeInternalError(s.log, w, "supply overview", err)
		return
	}
	writeJSON(w, overview)
}
