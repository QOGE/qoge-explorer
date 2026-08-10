// Package query implements the Phase 2D.1 read-only query layer over the
// already-indexed PostgreSQL schema (migrations/0001_initial.up.sql). It is
// the only thing internal/api's HTTP handlers are allowed to call — no
// handler issues SQL directly.
//
// Every exported method issues SELECT statements only: no INSERT, UPDATE,
// or DELETE, and no interaction with sync_state's canonical-mutation lock
// (lockCheckpoint in internal/store) — see docs/ARCHITECTURE.md §19 "Phase
// 2D.1: read-only query layer" for the full read-only enforcement argument
// and the reasoning behind the isolation level chosen for multi-statement
// reads.
//
// Canonical vs. orphan semantics are enforced by construction, not left to
// callers to get right ad hoc:
//
//   - Height-based lookups (RecentBlocks, BlockByHeight) only ever consider
//     canonical blocks/state — orphaned data can never be reached by height.
//   - Hash-based block lookups (BlockByHash) may return an orphaned block,
//     but the result is always explicitly tagged Canonical: false.
//   - Address balances (AddressSummary) and history (AddressHistory) are
//     derived exclusively from utxo_state/addresses, which internal/store
//     already keeps canonical-only (see RollbackTo) — an orphaned output
//     never contributes.
//
// Money is never a float64 anywhere in this package: every value is an
// exact integer satoshi count plus, where useful, an exact decimal QOGE
// string derived from chain.Amount.String() (integer arithmetic only).
package query
