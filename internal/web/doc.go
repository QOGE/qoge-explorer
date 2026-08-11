// Package web is the Phase 2E.1 server-rendered HTML explorer UI. It is
// PRESENTATION ONLY: every fact it displays comes from an internal/query.Store
// call already reviewed in Phase 2D.1, and this package performs no SQL, no
// script classification, no balance/UTXO accounting, and no Core RPC of its
// own. It is a sibling of internal/api over the same query.Store — see
// docs/ARCHITECTURE.md §20 — never a client of internal/api's HTTP surface
// (no loopback HTTP call from web back into the API).
//
// Rendering uses html/template exclusively (never text/template) so every
// blockchain-derived string (addresses, script hex, txids, hashes) is
// auto-escaped; templates and the one static stylesheet are embedded via
// go:embed, so the built binary has no working-directory or npm/Node
// dependency and remains a single self-contained executable.
package web
