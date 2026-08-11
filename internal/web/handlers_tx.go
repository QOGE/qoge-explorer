package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleTx resolves GET /tx/{id}, mirroring internal/api's handleTransaction
// fallback semantics exactly: id is tried as a txid first, and only on a
// miss as a wtxid — the response always reports both identities distinctly.
// Raw witness bytes (e.g. a full 17,088-byte P2QPK signature) are only
// fetched/rendered when the caller explicitly opts in with
// ?include_witness=true — see docs/ARCHITECTURE.md §20 "P2QPK large-witness
// default-hidden policy".
func (s *Server) handleTx(w http.ResponseWriter, r *http.Request) {
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

	tx, err := s.q.TransactionByTxID(r.Context(), id, includeWitness)
	if errors.Is(err, query.ErrNotFound) {
		tx, err = s.q.TransactionByWTxID(r.Context(), id, includeWitness)
	}
	if errors.Is(err, query.ErrNotFound) {
		s.renderNotFound(w, "Transaction not found.")
		return
	}
	if err != nil {
		s.renderInternalError(w, "transaction", err)
		return
	}
	s.render(w, "tx", http.StatusOK, txView{TransactionDetail: tx, IncludeWitness: includeWitness})
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
