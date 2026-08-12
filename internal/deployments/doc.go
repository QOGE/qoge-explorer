// Package deployments is the Phase 2G.1 consensus deployment observer:
// Core getdeploymentinfo acquisition, strict BIP9 decoding, and an
// isolated, atomically-replaceable PostgreSQL cache of the current BIP9
// deployment set.
//
// Core is the sole authority on BIP9/versionbits state. This package
// never recreates VersionBits state transitions, never infers
// LOCKED_IN/ACTIVE itself, and never counts signalling blocks
// independently of Core's own reported statistics — it only strictly
// decodes and caches what getdeploymentinfo returns (docs/
// ARCHITECTURE.md §24). Buried deployments (static historical consensus
// rules with no BIP9 status model) are decoded just enough to prove they
// aren't malformed, then intentionally dropped; only entries whose
// "type" is "bip9" are persisted.
//
// This package is a fundamentally different model from both the
// confirmed blockchain and the mempool cache: it writes only to
// chain_deployments and deployment_state (migrations/
// 0001_initial.up.sql, 0003_deployment_state.up.sql), never to any
// confirmed-chain or mempool_* table, and it never mutates them either.
// A deployment observation failure (RPC error, malformed response, a
// Core/confirmed-tip race during acquisition, a PostgreSQL write
// failure) is expected and normal; it is logged and retried, never
// allowed to halt confirmed indexing or mempool observation.
//
// This package is used by `qoge-explorer index` only (Synchronizer needs
// live Core RPC credentials, which `qoge-explorer serve` deliberately
// never has). Phase 2G.1 adds no public read API or UI — see
// docs/ARCHITECTURE.md §24 "Explicitly deferred."
package deployments
