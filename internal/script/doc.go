// Package script classifies scriptPubKeys (P2PK, P2PKH, P2SH, P2WPKH,
// P2WSH, P2QPK, NULLDATA, MULTISIG, UNKNOWN_WITNESS, UNKNOWN) and resolves
// addresses, preferring Core's own RPC-provided address wherever available.
// Reserved for Phase 2 — see docs/ARCHITECTURE.md §7, §9.
package script
