package web

import (
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// Server is the Phase 2E.1 presentation-only HTML explorer. It holds only a
// query.Store (never a write internal/store.Store, never internal/indexer,
// internal/rpc, or internal/decode) — see doc.go and
// docs/ARCHITECTURE.md §20.
type Server struct {
	q    *query.Store
	log  *slog.Logger
	mux  *http.ServeMux
	tmpl pages
}

// New builds a Server wired to q. log may be nil (errors are simply not
// logged server-side in that case; the client-facing response is
// unaffected). Panics if the embedded templates fail to parse — see
// loadTemplates's doc comment for why that can only be a build-time defect,
// never a runtime condition.
func New(q *query.Store, log *slog.Logger) *Server {
	t, err := loadTemplates()
	if err != nil {
		panic(err)
	}
	s := &Server{q: q, log: log, mux: http.NewServeMux(), tmpl: t}
	s.routes()
	return s
}

// ServeHTTP makes Server an http.Handler directly. Every response —
// including error pages and the static handler — gets the same baseline
// security headers; see docs/ARCHITECTURE.md §20 "HTML response headers".
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'")
	s.mux.ServeHTTP(w, r)
}

// knownGETRoutes lists every leaf/subtree HTML route this server serves,
// every one of them GET-only today. Mirrors internal/api/server.go's
// dual-registration precedence pattern exactly (method-qualified pattern
// for the real handler, bare pattern for a same-path wrong-method
// fallback, final catch-all "/" for anything unmatched) — see that file's
// extended doc comment for why a single naive catch-all is wrong. Here the
// fallback/catch-all render HTML, not JSON, per docs/ARCHITECTURE.md §20
// "web error pages are HTML".
var knownGETRoutes = []string{
	"/{$}",
	"/blocks",
	"/block/{id}",
	"/tx/{id}",
	"/address/{address}",
	"/search",
	"/mempool",
	"/mempool/tx/{id}",
	"/deployments",
	"/deployments/{name}",
	"/static/",
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.handleHome)
	s.mux.HandleFunc("GET /blocks", s.handleBlocks)
	s.mux.HandleFunc("GET /block/{id}", s.handleBlock)
	s.mux.HandleFunc("GET /tx/{id}", s.handleTx)
	s.mux.HandleFunc("GET /address/{address}", s.handleAddress)
	s.mux.HandleFunc("GET /search", s.handleSearch)
	s.mux.HandleFunc("GET /mempool", s.handleMempool)
	s.mux.HandleFunc("GET /mempool/tx/{id}", s.handleMempoolTx)

	s.mux.HandleFunc("GET /deployments", s.handleDeployments)
	s.mux.HandleFunc("GET /deployments/{name}", s.handleDeployment)

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticSub)))

	for _, pattern := range knownGETRoutes {
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", "GET")
			s.renderError(w, http.StatusMethodNotAllowed, "Method Not Allowed", "This page only supports GET.")
		})
	}

	// Final fallback: any path not matched by any pattern above (with any
	// method) gets the HTML 404 page, never net/http's plain-text default.
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, http.StatusNotFound, "Not Found", "There is nothing at this address.")
	})
}
