package web

import "net/http"

// handleBlocks resolves GET /blocks — canonical blocks, newest first,
// keyset-paginated on the existing height cursor (query.RecentBlocks),
// never arbitrary OFFSET pagination.
func (s *Server) handleBlocks(w http.ResponseWriter, r *http.Request) {
	before, ok := parseBeforeHeight(r)
	if !ok {
		s.renderBadRequest(w, "Malformed \"before\" query parameter: must be a non-negative decimal integer.")
		return
	}
	limit, ok := parsePageSize(r)
	if !ok {
		s.renderBadRequest(w, "Malformed \"limit\" query parameter: must be a non-negative decimal integer.")
		return
	}

	page, err := s.q.RecentBlocks(r.Context(), before, limit)
	if err != nil {
		s.renderInternalError(w, "recent blocks", err)
		return
	}
	s.render(w, "blocks", http.StatusOK, blocksView{
		Blocks:     page.Blocks,
		Pagination: blocksPagination(limit, page.NextBeforeHeight),
		FirstPage:  before == nil,
	})
}
