package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSupplyRollupCoverageMissing means the database has already been
// indexed past genesis (sync_state's canonical tip is set) and migration
// 0005 has been applied, but the canonical tip itself has no
// block_supply_rollup row — the exact "legacy database: migration 0005
// applied, but backfill-supply-rollup never run" gap task item 35 exists to
// catch at `index` startup, rather than only failing later on the first
// live ApplyBlock call once a new block extends this same lineage-less
// tip.
var ErrSupplyRollupCoverageMissing = errors.New("store: canonical tip has no block_supply_rollup row; run `qoge-explorer backfill-supply-rollup`")

// VerifySupplyRollupCoverage is an O(1) `index`-startup preflight (task
// item 35) — NOT called automatically by Store itself, exactly like
// VerifyNetworkIdentity (network_identity.go), so ordinary tests using an
// uninitialized/arbitrary-bootstrap Store are completely unaffected; only
// cmd/qoge-explorer's runIndex calls this, before starting the live
// indexing loop.
//
// Three outcomes:
//
//   - sync_state is uninitialized (indexed_height == -1): nil. A
//     completely fresh database before genesis is never treated as a
//     coverage gap — there is nothing to have coverage of yet.
//   - migration 0005 has not been applied to this database yet
//     (block_supply_rollup doesn't exist): nil. Whether `migrate up` has
//     been run is a separate, pre-existing operational requirement this
//     check doesn't own or duplicate.
//   - Otherwise: the canonical tip (sync_state.indexed_block_hash) must
//     have a block_supply_rollup row. This is a cheap, O(1) primary-key
//     existence check — not a full-chain scan (that's
//     BackfillSupplyRollup's job, and task item 29 explicitly forbids
//     putting a full-chain scan on any path index startup runs on every
//     restart). If the tip lacks a row, ErrSupplyRollupCoverageMissing is
//     returned with an actionable remediation pointer.
func VerifySupplyRollupCoverage(ctx context.Context, pool *pgxpool.Pool) error {
	var indexedHeight int64
	var indexedHash *string
	if err := pool.QueryRow(ctx, `SELECT indexed_height, indexed_block_hash FROM sync_state WHERE name = 'main'`).Scan(&indexedHeight, &indexedHash); err != nil {
		return fmt.Errorf("store: verify supply rollup coverage: read sync_state: %w", err)
	}
	if indexedHeight == -1 {
		return nil
	}

	var tableExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('block_supply_rollup') IS NOT NULL`).Scan(&tableExists); err != nil {
		return fmt.Errorf("store: verify supply rollup coverage: check table existence: %w", err)
	}
	if !tableExists {
		return nil
	}

	var hasRollup bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM block_supply_rollup WHERE block_hash = $1)`, *indexedHash).Scan(&hasRollup); err != nil {
		return fmt.Errorf("store: verify supply rollup coverage: check tip rollup: %w", err)
	}
	if !hasRollup {
		return fmt.Errorf("%w: tip %s (height %d)", ErrSupplyRollupCoverageMissing, *indexedHash, indexedHeight)
	}
	return nil
}
