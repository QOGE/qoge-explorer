// Package indexer drives block-by-block synchronization from Qogecoin Core
// into internal/store: sequential historical catch-up from genesis, a live
// polling loop, and reorg detection/rollback/replacement. See
// docs/ARCHITECTURE.md §18.
//
// indexer is orchestration only. It does not parse SQL (that's
// internal/store), does not classify scripts (internal/script), and does
// not decode raw Core JSON itself (internal/decode) — it calls those
// already-reviewed components in the fixed pipeline:
//
//	Core best chain -> indexer -> RPC fetch -> DecodeBlock -> Store.ApplyBlock
//
// and, on a fork:
//
//	detect mismatch -> find common ancestor -> depth policy ->
//	Store.RollbackTo -> apply replacement branch normally
package indexer
