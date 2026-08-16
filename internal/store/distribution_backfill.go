package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// backfillDistributionAdvisoryLockNamespace is distinct from both
// backfillAccountingAdvisoryLockNamespace and
// backfillSupplyRollupAdvisoryLockNamespace (backfill.go,
// supply_rollup_backfill.go), so none of the three backfill commands ever
// contend for the same advisory lock key against the same schema — each
// reads/writes disjoint tables. See schemaScopedLockKey.
const backfillDistributionAdvisoryLockNamespace int32 = 728341003

// ErrDistributionBackfillAlreadyRunning means another
// BackfillAddressDistribution call already holds the advisory lock for the
// SAME explorer schema.
var ErrDistributionBackfillAlreadyRunning = errors.New("store: another backfill-address-distribution run holds the advisory lock for this explorer schema")

// ErrDistributionCrossCheckFailed means the freshly rebuilt bucket rows do
// not match an INDEPENDENTLY computed direct aggregate of `addresses`
// (§15 of the Phase 2H.4a spec) — either per-bucket or in global total.
// BackfillAddressDistribution refuses to publish ANY row in this case; the
// whole transaction rolls back.
var ErrDistributionCrossCheckFailed = errors.New("store: backfill address distribution: independently computed cross-check does not match the freshly built rollup — refusing to publish")

// DistributionBackfillResult summarizes one successful
// BackfillAddressDistribution run.
type DistributionBackfillResult struct {
	TotalPositiveAddresses int64  // every address with balance_satoshis > 0, at the time of this run
	TotalBalanceSatoshis   int64  // sum of every such address's balance
	AnchorHeight           int64  // sync_state's height this run anchored to (-1 if uninitialized)
	AnchorHash             string // sync_state's hash this run anchored to ("" if uninitialized)
}

// distributionCrossCheckSQL independently recomputes every bucket's
// (address_count, balance_satoshis) directly from `addresses`, using
// literal satoshi boundaries (mirroring distributionBuckets in
// distribution.go) rather than joining against
// address_balance_distribution's own bucket-definition rows — a
// genuinely separate computation path from the one BackfillAddressDistribution
// uses to build the rollup itself (§15: "independently calculate"), not a
// second read of the same join.
const distributionCrossCheckSQL = `
SELECT
	COUNT(*) FILTER (WHERE balance_satoshis BETWEEN 1 AND 99999999),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis BETWEEN 1 AND 99999999), 0),
	COUNT(*) FILTER (WHERE balance_satoshis BETWEEN 100000000 AND 999999999),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis BETWEEN 100000000 AND 999999999), 0),
	COUNT(*) FILTER (WHERE balance_satoshis BETWEEN 1000000000 AND 9999999999),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis BETWEEN 1000000000 AND 9999999999), 0),
	COUNT(*) FILTER (WHERE balance_satoshis BETWEEN 10000000000 AND 99999999999),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis BETWEEN 10000000000 AND 99999999999), 0),
	COUNT(*) FILTER (WHERE balance_satoshis BETWEEN 100000000000 AND 999999999999),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis BETWEEN 100000000000 AND 999999999999), 0),
	COUNT(*) FILTER (WHERE balance_satoshis BETWEEN 1000000000000 AND 9999999999999),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis BETWEEN 1000000000000 AND 9999999999999), 0),
	COUNT(*) FILTER (WHERE balance_satoshis BETWEEN 10000000000000 AND 99999999999999),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis BETWEEN 10000000000000 AND 99999999999999), 0),
	COUNT(*) FILTER (WHERE balance_satoshis >= 100000000000000),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis >= 100000000000000), 0),
	COUNT(*) FILTER (WHERE balance_satoshis > 0),
	COALESCE(SUM(balance_satoshis) FILTER (WHERE balance_satoshis > 0), 0)
FROM addresses
`

// BackfillAddressDistribution reconstructs and publishes
// address_balance_distribution for the CURRENT addresses table, using only
// already-indexed PostgreSQL data — never Core RPC, never entity/ownership
// inference (§13 of the Phase 2H.4a spec). Required once for any database
// that had already been indexed before migration 0007 was applied; a
// database indexed entirely on or after 0007 already has every bucket kept
// correct live (Store.ApplyBlock/RollbackTo write it incrementally — see
// distribution.go). REQUIRES the index process to be stopped first — an
// operational requirement, not mechanically enforced (the advisory lock
// below only excludes a second concurrent backfill run, exactly like
// BackfillAccounting/BackfillSupplyRollup).
//
// # Atomicity (§13)
//
// The ENTIRE operation — the rebuild, the independent cross-check, and the
// anchor update — runs inside ONE PostgreSQL transaction. Any disagreement
// or error rolls back everything; no partially-published distribution
// state can ever become visible.
//
// # Rebuild (§13)
//
// A single aggregate query joins address_balance_distribution's own eight
// bucket-definition rows against `addresses` by range, producing exactly
// one (address_count, balance_satoshis) pair per bucket — including zero
// for a bucket with no addresses (LEFT JOIN, not INNER). This is a full
// scan of `addresses`, acceptable ONLY here (an explicit offline/operator
// backfill), never on the hot ApplyBlock/RollbackTo path (§20).
//
// # Cross-check (§15)
//
// Before committing, the freshly rebuilt bucket rows are compared — per
// bucket AND in global total — against distributionCrossCheckSQL, an
// INDEPENDENTLY computed direct aggregate of `addresses` using literal
// satoshi boundaries rather than a join against the same bucket-definition
// rows the rebuild itself used. Any disagreement is
// ErrDistributionCrossCheckFailed and aborts the whole transaction.
//
// # Idempotency (§19)
//
// Rerunning this command against an already-correct distribution is a
// no-op beyond re-deriving and re-verifying the same values (the rebuild
// query always recomputes every bucket from scratch — there is no partial-
// progress state to resume).
func BackfillAddressDistribution(ctx context.Context, pool *pgxpool.Pool) (DistributionBackfillResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	schemaKey, _, err := schemaScopedLockKey(ctx, tx)
	if err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: %w", err)
	}
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, backfillDistributionAdvisoryLockNamespace, schemaKey).Scan(&locked); err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: acquire advisory lock: %w", err)
	}
	if !locked {
		return DistributionBackfillResult{}, ErrDistributionBackfillAlreadyRunning
	}

	var syncHeight int64
	var syncHash *string
	if err := tx.QueryRow(ctx, `SELECT indexed_height, indexed_block_hash FROM sync_state WHERE name = 'main'`).Scan(&syncHeight, &syncHash); err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: read sync_state: %w", err)
	}

	// Rebuild: one aggregate query per bucket via range join against
	// address_balance_distribution's own bucket-definition rows.
	// balance_satoshis > 0 is implied by min_balance_satoshis >= 1 on every
	// bucket, kept explicit here for clarity/defense.
	rows, err := tx.Query(ctx, `
		SELECT d.bucket_id, COUNT(a.address), COALESCE(SUM(a.balance_satoshis), 0)
		FROM address_balance_distribution d
		LEFT JOIN addresses a
		  ON a.balance_satoshis >= d.min_balance_satoshis
		 AND (d.max_balance_satoshis IS NULL OR a.balance_satoshis <= d.max_balance_satoshis)
		 AND a.balance_satoshis > 0
		GROUP BY d.bucket_id
	`)
	if err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: rebuild aggregate: %w", err)
	}
	rebuilt := map[string]struct {
		count   int64
		balance int64
	}{}
	for rows.Next() {
		var bucketID string
		var count, balance int64
		if err := rows.Scan(&bucketID, &count, &balance); err != nil {
			rows.Close()
			return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: scan rebuild row: %w", err)
		}
		rebuilt[bucketID] = struct {
			count   int64
			balance int64
		}{count, balance}
	}
	if err := rows.Err(); err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: iterate rebuild rows: %w", err)
	}

	// Independent cross-check, BEFORE anything is written — a genuinely
	// separate SQL formulation of the same question (§15).
	// 8 buckets * (count, balance) + 1 global (count, balance) = 18 columns.
	var checkVals [18]int64
	scanArgs := make([]any, len(checkVals))
	for i := range checkVals {
		scanArgs[i] = &checkVals[i]
	}
	if err := tx.QueryRow(ctx, distributionCrossCheckSQL).Scan(scanArgs...); err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: cross-check scan: %w", err)
	}
	bucketOrder := DistributionBucketIDs()
	var totalCountDirect, totalBalanceDirect int64
	for i, bucketID := range bucketOrder {
		wantCount := checkVals[i*2]
		wantBalance := checkVals[i*2+1]
		got := rebuilt[bucketID]
		if got.count != wantCount || got.balance != wantBalance {
			return DistributionBackfillResult{}, fmt.Errorf("%w: bucket %s rebuilt(count=%d,balance=%d) != independent(count=%d,balance=%d)",
				ErrDistributionCrossCheckFailed, bucketID, got.count, got.balance, wantCount, wantBalance)
		}
		totalCountDirect += wantCount
		totalBalanceDirect += wantBalance
	}
	globalCount := checkVals[16]
	globalBalance := checkVals[17]
	if globalCount != totalCountDirect || globalBalance != totalBalanceDirect {
		// Structurally unreachable (the eight literal-boundary ranges
		// above partition balance_satoshis > 0 exactly), kept as an
		// explicit guard rather than silently trusted.
		return DistributionBackfillResult{}, fmt.Errorf("%w: global count/balance (%d,%d) != sum of bucket cross-checks (%d,%d)",
			ErrDistributionCrossCheckFailed, globalCount, globalBalance, totalCountDirect, totalBalanceDirect)
	}

	// Also require the rebuild's own total to match the direct global
	// aggregate over `addresses` (§13's "independently validate count +
	// balance totals").
	var directCount, directBalance int64
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(balance_satoshis), 0) FROM addresses WHERE balance_satoshis > 0`).Scan(&directCount, &directBalance); err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: direct global scan: %w", err)
	}
	if directCount != totalCountDirect || directBalance != totalBalanceDirect {
		return DistributionBackfillResult{}, fmt.Errorf("%w: direct addresses aggregate (%d,%d) != cross-check total (%d,%d)",
			ErrDistributionCrossCheckFailed, directCount, directBalance, totalCountDirect, totalBalanceDirect)
	}

	// Publish: every bucket, in deterministic order.
	for _, bucketID := range bucketOrder {
		v := rebuilt[bucketID]
		if _, err := tx.Exec(ctx, `
			UPDATE address_balance_distribution SET address_count = $2, balance_satoshis = $3 WHERE bucket_id = $1
		`, bucketID, v.count, v.balance); err != nil {
			return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: publish bucket %s: %w", bucketID, err)
		}
	}

	if err := setDistributionAnchor(ctx, tx, syncHash); err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return DistributionBackfillResult{}, fmt.Errorf("store: backfill address distribution: commit: %w", err)
	}

	result := DistributionBackfillResult{
		TotalPositiveAddresses: totalCountDirect,
		TotalBalanceSatoshis:   totalBalanceDirect,
		AnchorHeight:           syncHeight,
	}
	if syncHash != nil {
		result.AnchorHash = *syncHash
	}
	return result, nil
}
