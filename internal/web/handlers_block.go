package web

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleBlock resolves GET /block/{id}, mirroring internal/api's
// handleBlock semantics exactly: id may be a non-negative decimal height
// (canonical-only lookup) or a 64-char lowercase hex hash (canonical OR
// orphaned — the template renders the orphan banner from
// BlockDetail.Canonical, never assuming a hash lookup is canonical).
func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	height, isHeight, isHash := blockIdentifierKind(id)
	if !isHeight && !isHash {
		s.renderBadRequest(w, "Malformed block identifier: must be a 64-char lowercase hex hash or a non-negative decimal height.")
		return
	}

	var (
		block query.BlockDetail
		err   error
	)
	if isHash {
		block, err = s.q.BlockByHash(r.Context(), id)
	} else {
		block, err = s.q.BlockByHeight(r.Context(), height)
	}
	if errors.Is(err, query.ErrNotFound) {
		s.renderNotFound(w, "Block not found.")
		return
	}
	if err != nil {
		s.renderInternalError(w, "block", err)
		return
	}
	s.render(w, "block", http.StatusOK, block)
}
