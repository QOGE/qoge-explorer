package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleTransaction resolves GET /api/v1/tx/{id}. id must be exactly 64
// lowercase hex characters — a txid or a wtxid, without conflating the two:
// the lookup first tries id as a txid, and only if no such transaction
// exists falls back to trying it as a wtxid. The response always reports
// its own txid and wtxid as separate, distinct fields, so which identity
// space id belonged to is never ambiguous in the result — see
// docs/ARCHITECTURE.md §19.
//
// Raw witness data (e.g. a full 17,088-byte P2QPK signature) is only
// included when the caller explicitly opts in with ?include_witness=true —
// see query.WitnessItem and docs/ARCHITECTURE.md §19 "P2QPK large-witness
// response policy".
func (s *Server) handleTransaction(w http.ResponseWriter, r *http.Request) {
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

	tx, err := s.q.TransactionByTxID(r.Context(), id, includeWitness)
	if errors.Is(err, query.ErrNotFound) {
		tx, err = s.q.TransactionByWTxID(r.Context(), id, includeWitness)
	}
	if errors.Is(err, query.ErrNotFound) {
		writeNotFound(w, "transaction not found")
		return
	}
	if err != nil {
		writeInternalError(s.log, w, "transaction", err)
		return
	}
	writeJSON(w, tx)
}

func parseIncludeWitness(r *http.Request) (bool, bool) {
	raw := r.URL.Query().Get("include_witness")
	if raw == "" {
		return false, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false
	}
	return v, true
}
