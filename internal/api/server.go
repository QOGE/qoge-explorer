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

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)

	s.mux.HandleFunc("GET /api/v1/blocks", s.handleBlocks)
	s.mux.HandleFunc("GET /api/v1/block/{id}", s.handleBlock)

	s.mux.HandleFunc("GET /api/v1/tx/{id}", s.handleTransaction)

	s.mux.HandleFunc("GET /api/v1/address/{address}", s.handleAddress)
	s.mux.HandleFunc("GET /api/v1/address/{address}/transactions", s.handleAddressHistory)

	// Deliberately no catch-all "/" pattern: registering one would make it
	// a valid (if low-priority) match for every method, including on a
	// path that DOES have a route registered under a different method —
	// which silently defeats net/http.ServeMux's own automatic 405
	// Method Not Allowed detection (confirmed via the stdlib: a
	// registered "/" handler intercepts a wrong-method request as a 404
	// match instead of it hitting mux's built-in 405 path). Leaving no
	// catch-all means a truly unmatched route gets net/http's own 404,
	// and a wrong method on a real route gets its own automatic 405 (with
	// an Allow header) — both correct status codes, just plain-text
	// bodies rather than the JSON error envelope every handled route
	// below returns for its own 400/404s.
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
