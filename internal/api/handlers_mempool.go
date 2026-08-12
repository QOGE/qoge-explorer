package api

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleMempool resolves GET /api/v1/mempool. "generation",
// "before_entry_time", and "before_txid" together form a generation-safe
// pagination cursor (query.MempoolCursor) — they must all be supplied
// together (a continuation of a previous page) or all omitted (the first
// page); a cursor minted against a generation the mempool cache has since
// moved past is rejected with HTTP 409 mempool_generation_changed rather
// than silently paginated against the new snapshot — see
// docs/ARCHITECTURE.md §23 "Generation-safe pagination".
//
// An uninitialized mempool cache (the index process has never published a
// snapshot) is a valid HTTP 200 response with state.initialized=false and
// no transactions — never a fake synchronized-and-empty response.
func (s *Server) handleMempool(w http.ResponseWriter, r *http.Request) {
	generation, genPresent, ok := queryParamInt(r, "generation")
	if !ok {
		writeBadRequest(w, "malformed \"generation\" query parameter: must be a non-negative decimal integer")
		return
	}
	beforeEntryTime, timePresent, ok := queryParamInt(r, "before_entry_time")
	if !ok {
		writeBadRequest(w, "malformed \"before_entry_time\" query parameter: must be a non-negative decimal integer")
		return
	}
	beforeTxID := r.URL.Query().Get("before_txid")
	if beforeTxID != "" && !isHash64(beforeTxID) {
		writeBadRequest(w, "malformed \"before_txid\" query parameter: must be exactly 64 lowercase hex characters")
		return
	}
	if genPresent != timePresent || genPresent != (beforeTxID != "") {
		writeBadRequest(w, "\"generation\", \"before_entry_time\", and \"before_txid\" must all be supplied together, or all omitted")
		return
	}

	var cursor *query.MempoolCursor
	if genPresent {
		cursor = &query.MempoolCursor{Generation: generation, EntryTime: beforeEntryTime, TxID: beforeTxID}
	}

	limit, ok := parsePageSize(r)
	if !ok {
		writeBadRequest(w, "malformed \"limit\" query parameter: must be a non-negative decimal integer")
		return
	}

	overview, err := s.q.MempoolOverview(r.Context(), cursor, limit)
	if errors.Is(err, query.ErrMempoolGenerationChanged) {
		writeError(w, http.StatusConflict, "mempool_generation_changed", "the mempool snapshot changed since this pagination cursor was issued; restart from the first page")
		return
	}
	if err != nil {
		writeInternalError(s.log, w, "mempool overview", err)
		return
	}
	writeJSON(w, overview)
}

// handleMempoolTransaction resolves GET /api/v1/mempool/tx/{id}. id may be
// a txid or a wtxid (tried in that order, mirroring handleTransaction's
// confirmed-chain fallback exactly) — the response always reports its own
// txid and wtxid distinctly. If the mempool is initialized but id is not
// part of the current snapshot, this is a plain 404 — never a Core RPC
// fallback and never old/expired mempool history, since none exists by
// design (full-replacement snapshots only).
//
// Raw witness data is only included when the caller explicitly opts in
// with ?include_witness=true — same default-hidden policy as confirmed
// transaction detail (docs/ARCHITECTURE.md §23).
func (s *Server) handleMempoolTransaction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isHash64(id) {
		writeBadRequest(w, "malformed transaction identifier: must be exactly 64 lowercase hex characters")
		return
	}

	includeWitness, ok := parseIncludeWitness(r)
	if !ok {
		writeBadRequest(w, "malformed \"include_witness\" query parameter: must be \"true\" or \"false\"")
		return
	}

	tx, err := s.q.MempoolTransactionByTxID(r.Context(), id, includeWitness)
	if errors.Is(err, query.ErrNotFound) {
		tx, err = s.q.MempoolTransactionByWTxID(r.Context(), id, includeWitness)
	}
	if errors.Is(err, query.ErrNotFound) {
		writeNotFound(w, "mempool transaction not found")
		return
	}
	if err != nil {
		writeInternalError(s.log, w, "mempool transaction", err)
		return
	}
	writeJSON(w, tx)
}
