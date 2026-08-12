package api

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleDeployments resolves GET /api/v1/deployments. An uninitialized
// deployment cache (the index process has never published a snapshot) is
// a valid HTTP 200 response with state.initialized=false and an empty
// deployments array — never a fake synchronized-and-empty response
// (docs/ARCHITECTURE.md §25, spec item 20).
func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.DeploymentOverview(r.Context())
	if err != nil {
		writeInternalError(s.log, w, "deployment overview", err)
		return
	}
	writeJSON(w, overview)
}

// handleDeployment resolves GET /api/v1/deployments/{name}. name is
// looked up by exact database equality — no Core RPC fallback ever.
//
//   - An uninitialized cache returns HTTP 503
//     "deployment_cache_uninitialized": it cannot truthfully claim a
//     deployment does not exist (spec item 21).
//   - An initialized cache with no matching name returns HTTP 404
//     "deployment_not_found_in_snapshot" — meaning "not present in the
//     cached snapshot", never "Core guarantees this deployment does not
//     exist" (spec item 22).
func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !isValidDeploymentNameShape(name) {
		writeBadRequest(w, "malformed deployment name")
		return
	}

	detail, err := s.q.DeploymentByName(r.Context(), name)
	if errors.Is(err, query.ErrDeploymentCacheUninitialized) {
		writeError(w, http.StatusServiceUnavailable, "deployment_cache_uninitialized", "the deployment cache has not synchronized yet")
		return
	}
	if errors.Is(err, query.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment_not_found_in_snapshot", "no deployment with this name in the cached snapshot")
		return
	}
	if err != nil {
		writeInternalError(s.log, w, "deployment detail", err)
		return
	}
	writeJSON(w, detail)
}
