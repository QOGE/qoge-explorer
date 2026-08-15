package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDistributionCoverageMissing means the database has already been
// indexed past genesis (sync_state's canonical tip is set) and migration
// 0007 has been applied, but address_balance_distribution_state does not
// exactly match sync_state's own checkpoint — the exact "legacy database:
// migration 0007 applied, but backfill-address-distribution never run" gap
// (§14 of the Phase 2H.4a spec) `index` startup must refuse to proceed on,
// rather than silently applying incremental deltas on top of a stale or
// nonexistent baseline.
var ErrDistributionCoverageMissing = errors.New("store: address balance distribution state does not match sync_state; run `qoge-explorer backfill-address-distribution`")

// VerifyDistributionCoverage is an O(1) `index`-startup preflight (§14),
// mirroring VerifySupplyRollupCoverage's shape exactly (same three
// outcomes below) but comparing full EQUALITY with sync_state's checkpoint
// rather than mere existence — because, unlike block_supply_rollup,
// address_balance_distribution_state must equal sync_state's checkpoint
// EXACTLY at every valid state (§5 of the Phase 2H.4a spec), never just
// "has some row for the tip." NOT called automatically by Store itself —
// only cmd/qoge-explorer's runIndex calls this, before starting the live
// indexing loop, so ordinary tests using an uninitialized/arbitrary-
// bootstrap Store are completely unaffected.
//
// Three outcomes:
//
//   - sync_state is uninitialized (indexed_height == -1): nil. A
//     completely fresh database before genesis has nothing to have
//     coverage of yet.
//   - migration 0007 has not been applied to this database yet
//     (address_balance_distribution_state doesn't exist): nil. Whether
//     `migrate up` has been run is a separate, pre-existing operational
//     requirement this check doesn't own or duplicate.
//   - Otherwise: address_balance_distribution_state's (indexed_height,
//     indexed_block_hash) must equal sync_state's exactly. A mismatch (the
//     legacy-upgrade gap, or any other divergence) is
//     ErrDistributionCoverageMissing with an actionable remediation
//     pointer.
func VerifyDistributionCoverage(ctx context.Context, pool *pgxpool.Pool) error {
	var syncHeight int64
	var syncHash *string
	if err := pool.QueryRow(ctx, `SELECT indexed_height, indexed_block_hash FROM sync_state WHERE name = 'main'`).Scan(&syncHeight, &syncHash); err != nil {
		return fmt.Errorf("store: verify distribution coverage: read sync_state: %w", err)
	}
	if syncHeight == -1 {
		return nil
	}

	var tableExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('address_balance_distribution_state') IS NOT NULL`).Scan(&tableExists); err != nil {
		return fmt.Errorf("store: verify distribution coverage: check table existence: %w", err)
	}
	if !tableExists {
		return nil
	}

	var distHeight int64
	var distHash *string
	if err := pool.QueryRow(ctx, `SELECT indexed_height, indexed_block_hash FROM address_balance_distribution_state WHERE name = 'main'`).Scan(&distHeight, &distHash); err != nil {
		return fmt.Errorf("store: verify distribution coverage: read address_balance_distribution_state: %w", err)
	}

	if distHeight != syncHeight || !equalNullableString(distHash, syncHash) {
		return fmt.Errorf("%w: sync_state tip height=%d hash=%s, distribution state height=%d hash=%s",
			ErrDistributionCoverageMissing, syncHeight, formatNullableHash(syncHash), distHeight, formatNullableHash(distHash))
	}
	return nil
}

// equalNullableString reports whether two nullable strings hold the same
// value — both nil, or both non-nil with equal contents.
func equalNullableString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
