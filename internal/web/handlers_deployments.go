package web

import (
	"errors"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// handleDeployments resolves GET /deployments. An uninitialized deployment
// cache renders a clear "not synchronized yet" message — never "0
// deployments" as if a successful Core observation had returned none
// (docs/ARCHITECTURE.md §25, spec item 32).
func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	overview, err := s.q.DeploymentOverview(r.Context())
	if err != nil {
		s.renderInternalError(w, "deployment overview", err)
		return
	}
	s.render(w, "deployments", http.StatusOK, deploymentsView{
		State:       overview.State,
		Deployments: overview.Deployments,
	})
}

// handleDeployment resolves GET /deployments/{name}. name is looked up by
// exact database equality; there is no Core RPC fallback. When Core
// reports a deployment named "p2qpk", /deployments/p2qpk is this same
// generic route — no special-cased P2QPK handler exists
// (docs/ARCHITECTURE.md §25, spec item 27).
func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !isValidDeploymentNameShape(name) {
		s.renderBadRequest(w, "Malformed deployment name.")
		return
	}

	detail, err := s.q.DeploymentByName(r.Context(), name)
	if errors.Is(err, query.ErrDeploymentCacheUninitialized) {
		s.renderError(w, http.StatusServiceUnavailable, "Service Unavailable", "The deployment cache has not synchronized yet.")
		return
	}
	if errors.Is(err, query.ErrNotFound) {
		s.renderNotFound(w, "No deployment with this name in the cached snapshot.")
		return
	}
	if err != nil {
		s.renderInternalError(w, "deployment detail", err)
		return
	}
	s.render(w, "deployment", http.StatusOK, deploymentView{
		State:      detail.State,
		Deployment: detail.Deployment,
		RawJSON:    string(detail.Deployment.Raw),
	})
}
