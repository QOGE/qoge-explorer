package api

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

func (s *Server) handleBlocks(w http.ResponseWriter, r *http.Request) {
	before, ok := parseBeforeHeight(r)
	if !ok {
		writeBadRequest(w, "malformed \"before\" query parameter: must be a non-negative decimal integer")
		return
	}
	limit, ok := parsePageSize(r)
	if !ok {
		writeBadRequest(w, "malformed \"limit\" query parameter: must be a non-negative decimal integer")
		return
	}

	page, err := s.q.RecentBlocks(r.Context(), before, limit)
	if err != nil {
		writeInternalError(s.log, w, "recent blocks", err)
		return
	}
	writeJSON(w, page)
}

// handleBlock resolves GET /api/v1/block/{id}, where id may be a canonical
// decimal height (canonical-only lookup) or a 64-char hex block hash
// (canonical OR orphaned — the caller must check the response's
// "canonical" field).
func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	height, isHeight, isHash := blockIdentifierKind(id)
	if !isHeight && !isHash {
		writeBadRequest(w, "malformed block identifier: must be a 64-char lowercase hex hash or a non-negative decimal height")
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
		writeNotFound(w, "block not found")
		return
	}
	if err != nil {
		writeInternalError(s.log, w, "block", err)
		return
	}
	writeJSON(w, block)
}
