package web

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleMempool resolves GET /mempool. Mirrors internal/api's
// handleMempool's cursor/pagination handling exactly, but on a
// GenerationChanged pagination failure (a concurrent ReplaceSnapshot moved
// the mempool past the cursor's generation) the SSR behavior is to
// redirect/reset cleanly to the current first page — never render an error
// page for what is normal asynchronous mempool replacement.
func (s *Server) handleMempool(w http.ResponseWriter, r *http.Request) {
	generation, genPresent, ok := queryParamInt(r, "generation")
	if !ok {
		s.renderBadRequest(w, "Malformed \"generation\" query parameter: must be a non-negative decimal integer.")
		return
	}
	beforeEntryTime, timePresent, ok := queryParamInt(r, "before_entry_time")
	if !ok {
		s.renderBadRequest(w, "Malformed \"before_entry_time\" query parameter: must be a non-negative decimal integer.")
		return
	}
	beforeTxID := r.URL.Query().Get("before_txid")
	if beforeTxID != "" && !isHash64(beforeTxID) {
		s.renderBadRequest(w, "Malformed \"before_txid\" query parameter: must be exactly 64 lowercase hex characters.")
		return
	}
	if genPresent != timePresent || genPresent != (beforeTxID != "") {
		s.renderBadRequest(w, "\"generation\", \"before_entry_time\", and \"before_txid\" must all be supplied together, or all omitted.")
		return
	}

	var cursor *query.MempoolCursor
	if genPresent {
		cursor = &query.MempoolCursor{Generation: generation, EntryTime: beforeEntryTime, TxID: beforeTxID}
	}

	limit, ok := parsePageSize(r)
	if !ok {
		s.renderBadRequest(w, "Malformed \"limit\" query parameter: must be a non-negative decimal integer.")
		return
	}

	overview, err := s.q.MempoolOverview(r.Context(), cursor, limit)
	if errors.Is(err, query.ErrMempoolGenerationChanged) {
		http.Redirect(w, r, "/mempool", http.StatusFound)
		return
	}
	if err != nil {
		s.renderInternalError(w, "mempool overview", err)
		return
	}

	s.render(w, "mempool", http.StatusOK, mempoolView{
		Overview:   overview,
		Pagination: mempoolPagination(limit, overview.Transactions.NextCursor),
		FirstPage:  cursor == nil,
	})
}

// handleMempoolTx resolves GET /mempool/tx/{id}, mirroring handleTx's
// txid-then-wtxid fallback. A missing id when the mempool IS initialized is
// a plain 404 — no Core fallback, no stale/expired history, since none
// exists by design.
func (s *Server) handleMempoolTx(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isHash64(id) {
		s.renderBadRequest(w, "Malformed transaction identifier: must be exactly 64 lowercase hex characters.")
		return
	}

	includeWitness, ok := parseIncludeWitness(r)
	if !ok {
		s.renderBadRequest(w, "Malformed \"include_witness\" query parameter: must be \"true\" or \"false\".")
		return
	}

	tx, err := s.q.MempoolTransactionByTxID(r.Context(), id, includeWitness)
	if errors.Is(err, query.ErrNotFound) {
		tx, err = s.q.MempoolTransactionByWTxID(r.Context(), id, includeWitness)
	}
	if errors.Is(err, query.ErrNotFound) {
		s.renderNotFound(w, "Mempool transaction not found.")
		return
	}
	if err != nil {
		s.renderInternalError(w, "mempool transaction", err)
		return
	}
	s.render(w, "mempooltx", http.StatusOK, mempoolTxView{MempoolTransactionDetail: tx, IncludeWitness: includeWitness})
}
