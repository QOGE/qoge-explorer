// Package mempool is the Phase 2F.1 mempool cache foundation: Core
// mempool RPC acquisition, strict transaction decoding, and an isolated,
// atomically-replaceable PostgreSQL cache for currently-unconfirmed
// transactions.
//
// Mempool state is a fundamentally different model from the confirmed
// blockchain (docs/ARCHITECTURE.md §22): it is ephemeral, non-consensus,
// and Core is the sole authority on the current set. This package never
// writes to any confirmed-chain table (transactions, blocks, utxo_state,
// addresses, sync_state, ...) — only to the dedicated mempool_* tables
// (migrations/0002_mempool_cache.up.sql). A mempool transaction failure
// (RPC error, a transaction disappearing, a transaction becoming
// confirmed, Core's tip moving mid-acquisition) is expected and normal;
// it is logged and retried, never allowed to halt confirmed indexing.
//
// This package is used by `qoge-explorer index` only (Synchronizer needs
// live Core RPC credentials, which `qoge-explorer serve` deliberately
// never has). Phase 2F.1 adds no public read API or UI — see
// docs/ARCHITECTURE.md §22 "Explicitly deferred."
package mempool
