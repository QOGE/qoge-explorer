package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrGenesisMissing means a network-identity preflight (VerifyNetworkIdentity)
// found no canonical block at height 0. A normal, already-indexed production
// database always has genesis — production historical sync always starts at
// height 0 (see apply.go's "Canonical tip continuity" doc comment; an
// uninitialized Store's arbitrary-synthetic-start-height bootstrap is a
// TEST-ONLY convenience, never a production database shape). A missing
// genesis therefore FAILS CLOSED rather than trusting the operator-supplied
// QOGE_NETWORK alone.
var ErrGenesisMissing = errors.New("store: canonical genesis block (height 0) is missing; cannot verify indexed network identity")

// ErrGenesisMismatch means the database's actual canonical genesis hash
// (height 0) does not match the genesis hash QOGE Core stable asserts for
// the configured network (chain.ExpectedGenesisHash) — the database was very
// likely indexed against a DIFFERENT network than QOGE_NETWORK claims. This
// is exactly the "regtest database + QOGE_NETWORK=main" misconfiguration
// that previously produced plausible-but-false block_accounting rows (real
// Core subsidy 50 QOGE, wrongly-backfilled subsidy 100 QOGE, false 50 QOGE
// "unclaimed reward") — see docs/ARCHITECTURE.md §26 "Backfill network
// identity verification".
var ErrGenesisMismatch = errors.New("store: indexed genesis does not match the configured network")

// VerifyNetworkIdentity proves that pool's already-indexed database actually
// belongs to network, by comparing its canonical genesis block hash (height
// 0) against QOGE Core stable's asserted genesis hash for that network
// (chain.ExpectedGenesisHash). It is PostgreSQL-only — it never calls Core
// RPC — and exists specifically for backfill-accounting's preflight
// (cmd/qoge-explorer's runBackfillAccounting), since that command has no
// other way to detect an operator-supplied QOGE_NETWORK that doesn't match
// what was actually indexed (unlike `index`, which cross-checks QOGE_NETWORK
// against Core's live getblockchaininfo via indexer.ValidateStartup).
//
// Returns ErrGenesisMissing if no canonical block exists at height 0 (fails
// closed rather than trusting QOGE_NETWORK alone), or ErrGenesisMismatch if
// one exists but its hash doesn't match — both wrapped with the configured
// network and expected genesis hash, and a mismatch additionally with the
// observed genesis hash, so a caller can print all three without
// re-deriving them. If network itself is unrecognized, or is signet (QOGE
// Core stable has no stable asserted signet genesis — see
// chain.ExpectedGenesisHash's doc comment), that error is returned as-is,
// wrapped only with this function's own context.
//
// Callers MUST call this — and require it to succeed — before writing any
// accounting row for a database they didn't just create themselves; it does
// not run automatically inside Store, so ordinary tests using an arbitrary
// synthetic bootstrap height (docs/ARCHITECTURE.md §16 "Store bootstrap
// convenience") are unaffected.
func VerifyNetworkIdentity(ctx context.Context, pool *pgxpool.Pool, network string) error {
	expected, err := chain.ExpectedGenesisHash(network)
	if err != nil {
		return fmt.Errorf("store: verify network identity: %w", err)
	}

	var observed string
	err = pool.QueryRow(ctx, `SELECT hash FROM blocks WHERE height = 0 AND canonical`).Scan(&observed)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: QOGE_NETWORK=%q expected genesis %s", ErrGenesisMissing, network, expected)
	}
	if err != nil {
		return fmt.Errorf("store: verify network identity: read canonical genesis: %w", err)
	}
	if observed != expected {
		return fmt.Errorf("%w: QOGE_NETWORK=%q expected genesis %s, observed genesis %s",
			ErrGenesisMismatch, network, expected, observed)
	}
	return nil
}
