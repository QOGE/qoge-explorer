package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// Server is the Phase 2D.1 read-only JSON API. It holds only a query.Store
// (never a write internal/store.Store) — see docs/ARCHITECTURE.md §19.
type Server struct {
	q   *query.Store
	log *slog.Logger
	mux *http.ServeMux
}

// New builds a Server wired to q. log may be nil (errors are simply not
// logged server-side in that case; the client-facing response is
// unaffected either way).
func New(q *query.Store, log *slog.Logger) *Server {
	s := &Server{q: q, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP makes Server an http.Handler directly.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// knownGETRoutes lists every route pattern this API serves — every one of
// them is GET-only today. routes() registers each pattern twice: once as
// "GET <pattern>" for the real handler, and once as the bare "<pattern>"
// (method-agnostic) for a JSON 405. net/http.ServeMux resolves a request
// against the MORE SPECIFIC pattern first — a method-qualified pattern
// beats a bare one for a request whose method matches — so a GET request
// hits the real handler and any other method falls through to the 405
// registration for that exact path (confirmed directly against the
// stdlib: see internal/api's review notes / git history). This is why a
// naive single catch-all "/" registered ALONE (an earlier version of this
// file did that) is wrong: an unrestricted-method "/" pattern is itself a
// valid match for a wrong-method request on a real path, which pre-empts
// net/http's own built-in 405 detection and silently turns it into a 404
// instead.
var knownGETRoutes = []string{
	"/healthz",
	"/readyz",
	"/api/v1/status",
	"/api/v1/blocks",
	"/api/v1/block/{id}",
	"/api/v1/tx/{id}",
	"/api/v1/address/{address}",
	"/api/v1/address/{address}/transactions",
	"/api/v1/mempool",
	"/api/v1/mempool/tx/{id}",
	"/api/v1/deployments",
	"/api/v1/deployments/{name}",
	"/api/v1/supply",
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)

	s.mux.HandleFunc("GET /api/v1/blocks", s.handleBlocks)
	s.mux.HandleFunc("GET /api/v1/block/{id}", s.handleBlock)

	s.mux.HandleFunc("GET /api/v1/tx/{id}", s.handleTransaction)

	s.mux.HandleFunc("GET /api/v1/address/{address}", s.handleAddress)
	s.mux.HandleFunc("GET /api/v1/address/{address}/transactions", s.handleAddressHistory)

	s.mux.HandleFunc("GET /api/v1/mempool", s.handleMempool)
	s.mux.HandleFunc("GET /api/v1/mempool/tx/{id}", s.handleMempoolTransaction)

	s.mux.HandleFunc("GET /api/v1/deployments", s.handleDeployments)
	s.mux.HandleFunc("GET /api/v1/deployments/{name}", s.handleDeployment)

	s.mux.HandleFunc("GET /api/v1/supply", s.handleSupply)

	for _, pattern := range knownGETRoutes {
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeMethodNotAllowed(w, http.MethodGet)
		})
	}

	// Final fallback: any path not matched by any pattern above (with any
	// method) gets a JSON 404, never net/http's plain-text default.
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeNotFound(w, "no such route")
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"status": "ok"})
}

// handleReadyz confirms the configured PostgreSQL database is reachable and
// holds the expected sync_state checkpoint row — never Core RPC.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := s.q.Status(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "database not reachable")
		return
	}
	writeJSON(w, map[string]any{"status": "ready"})
}
