// Package api implements the Phase 2D.1 read-only JSON API: thin
// stdlib net/http handlers over internal/query. No handler issues SQL —
// every response is built from a single internal/query call — and no route
// ever mutates anything (see docs/ARCHITECTURE.md §19).
//
// Routes are versioned under /api/v1/ from the start. /healthz is cheap
// process liveness (no database dependency); /readyz additionally confirms
// the configured PostgreSQL database is reachable and holds the expected
// checkpoint row.
package api
