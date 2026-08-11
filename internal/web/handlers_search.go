package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleSearch resolves GET /search?q=..., resolving conservatively and
// deterministically — never querying Core, never a broad SQL search engine
// (see docs/ARCHITECTURE.md §20 "search behavior"):
//
//   - a non-negative decimal integer redirects to /block/{height} (the
//     block page itself renders 404 if that height doesn't exist — search
//     does not duplicate that existence check);
//   - exactly 64 lowercase hex characters is tried, in this fixed order,
//     against the already-reviewed lookups: BlockByHash, TransactionByTxID,
//     TransactionByWTxID — the first hit redirects there; no hit renders an
//     explicit "nothing found" result page, never a guess;
//   - anything else within the ordinary address-shape bound is treated as
//     an address and redirected to /address/{address}.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("q")
	if len(raw) > maxSearchLength {
		s.renderBadRequest(w, "Search query too long.")
		return
	}
	q := strings.TrimSpace(raw)
	if q == "" {
		s.renderBadRequest(w, "Missing search query.")
		return
	}

	if height, ok := parseHeight(q); ok {
		http.Redirect(w, r, "/block/"+strconv.FormatInt(height, 10), http.StatusFound)
		return
	}

	if isHash64(q) {
		ctx := r.Context()

		if _, err := s.q.BlockByHash(ctx, q); err == nil {
			http.Redirect(w, r, "/block/"+q, http.StatusFound)
			return
		} else if !errors.Is(err, query.ErrNotFound) {
			s.renderInternalError(w, "search: block by hash", err)
			return
		}

		if _, err := s.q.TransactionByTxID(ctx, q, false); err == nil {
			http.Redirect(w, r, "/tx/"+q, http.StatusFound)
			return
		} else if !errors.Is(err, query.ErrNotFound) {
			s.renderInternalError(w, "search: transaction by txid", err)
			return
		}

		if _, err := s.q.TransactionByWTxID(ctx, q, false); err == nil {
			http.Redirect(w, r, "/tx/"+q, http.StatusFound)
			return
		} else if !errors.Is(err, query.ErrNotFound) {
			s.renderInternalError(w, "search: transaction by wtxid", err)
			return
		}

		s.render(w, "search", http.StatusOK, searchView{Query: q})
		return
	}

	if !isValidAddressShape(q) {
		s.renderBadRequest(w, "Unrecognized search input.")
		return
	}
	http.Redirect(w, r, "/address/"+url.PathEscape(q), http.StatusFound)
}
