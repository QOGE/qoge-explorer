package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SatoshisPerQOGE is the exact integer scale factor between QOGE and
// satoshis (1 QOGE = 100,000,000 satoshis) — used ONLY to define bucket
// boundaries below, never as a division for classification. Bucket
// membership is always decided by exact integer comparison against
// distributionBuckets, never by chain.Amount.WholeQOGE(): WholeQOGE
// truncates (deliberately lossy, documented on chain.Amount itself), which
// would silently misclassify a balance close to a bucket boundary (e.g.
// 99,999,999 satoshis, one satoshi below 1 QOGE, must stay in lt_1 — a
// truncating conversion could not even distinguish it from 0 QOGE).
const SatoshisPerQOGE = 100_000_000

// distributionBucket is one of the eight fixed, positive-balance buckets —
// see migrations/0007_address_balance_distribution.up.sql, whose seeded
// (bucket_id, min_balance_satoshis, max_balance_satoshis) rows this slice
// mirrors EXACTLY (TestDistributionBuckets_MatchMigrationSeed cross-checks
// the two never drift apart). maxSatoshis == nil means open-ended (the
// gte_1m top bucket). Both bounds are inclusive.
type distributionBucket struct {
	id          string
	minSatoshis int64
	maxSatoshis *int64 // nil = unbounded above
}

func boundedBucket(id string, min, max int64) distributionBucket {
	m := max
	return distributionBucket{id: id, minSatoshis: min, maxSatoshis: &m}
}

// distributionBuckets is ordered ascending by range — DistributionBucketIDs
// preserves this order for deterministic iteration/reporting.
var distributionBuckets = []distributionBucket{
	boundedBucket("lt_1", 1, 1*SatoshisPerQOGE-1),
	boundedBucket("1_10", 1*SatoshisPerQOGE, 10*SatoshisPerQOGE-1),
	boundedBucket("10_100", 10*SatoshisPerQOGE, 100*SatoshisPerQOGE-1),
	boundedBucket("100_1k", 100*SatoshisPerQOGE, 1_000*SatoshisPerQOGE-1),
	boundedBucket("1k_10k", 1_000*SatoshisPerQOGE, 10_000*SatoshisPerQOGE-1),
	boundedBucket("10k_100k", 10_000*SatoshisPerQOGE, 100_000*SatoshisPerQOGE-1),
	boundedBucket("100k_1m", 100_000*SatoshisPerQOGE, 1_000_000*SatoshisPerQOGE-1),
	{id: "gte_1m", minSatoshis: 1_000_000 * SatoshisPerQOGE, maxSatoshis: nil},
}

// DistributionBucketIDs returns the eight fixed bucket IDs in ascending
// balance order — exported for the backfill command and tests.
func DistributionBucketIDs() []string {
	ids := make([]string, len(distributionBuckets))
	for i, b := range distributionBuckets {
		ids[i] = b.id
	}
	return ids
}

// bucketForBalance returns the bucket ID a positive balance belongs to.
// ok is false for balance <= 0 — a zero or negative balance belongs to NO
// bucket (§3/§11 of the Phase 2H.4a spec: zero balances belong to no
// bucket, every positive balance belongs to exactly one).
func bucketForBalance(balanceSatoshis int64) (bucketID string, ok bool) {
	if balanceSatoshis <= 0 {
		return "", false
	}
	for _, b := range distributionBuckets {
		if balanceSatoshis < b.minSatoshis {
			break // buckets are ascending; no later bucket can match either
		}
		if b.maxSatoshis == nil || balanceSatoshis <= *b.maxSatoshis {
			return b.id, true
		}
	}
	// Unreachable for any balanceSatoshis > 0: distributionBuckets covers
	// [1, +inf) with no gap (the eight ranges above are contiguous and the
	// last is open-ended).
	return "", false
}

// ErrDistributionInvariant means an incremental distribution delta would
// make a bucket's address_count or balance_satoshis negative — a hard
// integrity error (§16 of the Phase 2H.4a spec: never clamp to zero, fail
// the transaction instead). This should never trigger from genuine chain
// data; the whole ApplyBlock/RollbackTo transaction rolls back with this
// error wrapped in it.
var ErrDistributionInvariant = errors.New("store: address balance distribution: delta would make a bucket negative")

// distributionDelta accumulates per-bucket address_count/balance_satoshis
// deltas across every address touched by one ApplyBlock or RollbackTo call,
// so a block touching many addresses produces at most one UPDATE per
// bucket (eight, at most) rather than one per touched address (§6 of the
// Phase 2H.4a spec).
type distributionDelta struct {
	count   map[string]int64
	balance map[string]int64
}

func newDistributionDelta() *distributionDelta {
	return &distributionDelta{count: map[string]int64{}, balance: map[string]int64{}}
}

// observe records one address's balance transition (oldBalance ->
// newBalance) into the accumulated per-bucket deltas — see §6's transition
// table: zero->positive, positive->zero, cross-bucket, same-bucket (a
// same-bucket transition still adjusts the bucket's balance_satoshis by
// newBalance-oldBalance even though address_count is unchanged), and
// zero->zero (both branches below no-op, since bucketForBalance's ok is
// false for both).
func (d *distributionDelta) observe(oldBalance, newBalance int64) {
	if oldBucket, ok := bucketForBalance(oldBalance); ok {
		d.count[oldBucket]--
		d.balance[oldBucket] -= oldBalance
	}
	if newBucket, ok := bucketForBalance(newBalance); ok {
		d.count[newBucket]++
		d.balance[newBucket] += newBalance
	}
}

// bucketsTouched returns every bucket ID with a nonzero accumulated delta,
// in distributionBuckets' deterministic order — so applyDistributionDeltas
// issues its (at most eight) UPDATEs in a stable order rather than Go map
// iteration order.
func (d *distributionDelta) bucketsTouched() []string {
	var ids []string
	for _, b := range distributionBuckets {
		if d.count[b.id] != 0 || d.balance[b.id] != 0 {
			ids = append(ids, b.id)
		}
	}
	return ids
}

// currentAddressBalance returns address's currently-cached balance_satoshis
// (0 if no addresses row exists for it) — used to capture the "old" balance
// immediately before recomputeAddress overwrites/deletes it, so the
// resulting distribution delta can be derived. A point lookup by primary
// key, O(1) — never a table scan.
func currentAddressBalance(ctx context.Context, tx pgx.Tx, address string) (int64, error) {
	var balance int64
	err := tx.QueryRow(ctx, `SELECT balance_satoshis FROM addresses WHERE address = $1`, address).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: address balance distribution: read current balance %s: %w", address, err)
	}
	return balance, nil
}

// recomputeAddressTracked wraps recomputeAddress: it captures address's
// balance immediately before recomputation, calls recomputeAddress exactly
// as ApplyBlock/RollbackTo always have, and records the resulting
// old->new transition into deltas. This is the ONLY way ApplyBlock/
// RollbackTo touch an address's cache from this phase onward — never a
// second, independent balance read (§7 of the Phase 2H.4a spec: no second
// address-balance engine).
func recomputeAddressTracked(ctx context.Context, tx pgx.Tx, address string, deltas *distributionDelta) error {
	oldBalance, err := currentAddressBalance(ctx, tx, address)
	if err != nil {
		return err
	}
	newBalance, err := recomputeAddress(ctx, tx, address)
	if err != nil {
		return err
	}
	deltas.observe(oldBalance, newBalance)
	return nil
}

// applyDistributionDeltas publishes deltas' accumulated per-bucket changes
// to address_balance_distribution, inside the same transaction ApplyBlock/
// RollbackTo is already running. Each touched bucket's CURRENT values are
// read first, the resulting new values are computed and checked explicitly
// for negativity in Go (ErrDistributionInvariant) BEFORE any UPDATE is
// issued — the same "compute the new value, check its own invariant, only
// then persist" discipline applySupplyRollup uses for
// cumulative_utxo_set_value_satoshis/ErrRollupInvariant (supply_rollup.go),
// so a violation always surfaces as this clear, typed error rather than a
// raw CHECK-constraint failure from the database. The migration's own
// CHECK constraints on address_count/balance_satoshis remain a second,
// independent guard that should never actually fire given this ordering —
// defense in depth, not the primary mechanism (§16: fail the transaction,
// never clamp).
//
// No concurrent writer can observe or race this read-then-write: every
// ApplyBlock/RollbackTo call already holds the sync_state row lock
// acquired by lockCheckpoint before this function is ever reached, which
// serializes every canonical mutation against every other one.
func applyDistributionDeltas(ctx context.Context, tx pgx.Tx, deltas *distributionDelta) error {
	for _, bucketID := range deltas.bucketsTouched() {
		countDelta := deltas.count[bucketID]
		balanceDelta := deltas.balance[bucketID]

		var curCount, curBalance int64
		if err := tx.QueryRow(ctx, `
			SELECT address_count, balance_satoshis FROM address_balance_distribution WHERE bucket_id = $1
		`, bucketID).Scan(&curCount, &curBalance); err != nil {
			return fmt.Errorf("store: address balance distribution: read bucket %s: %w", bucketID, err)
		}

		newCount := curCount + countDelta
		newBalance := curBalance + balanceDelta
		if newCount < 0 || newBalance < 0 {
			return fmt.Errorf("%w: bucket %s address_count=%d balance_satoshis=%d",
				ErrDistributionInvariant, bucketID, newCount, newBalance)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE address_balance_distribution SET address_count = $2, balance_satoshis = $3 WHERE bucket_id = $1
		`, bucketID, newCount, newBalance); err != nil {
			return fmt.Errorf("store: address balance distribution: apply delta to bucket %s: %w", bucketID, err)
		}
	}
	return nil
}

// setDistributionAnchor updates address_balance_distribution_state's
// singleton row to hash — mirroring exactly how ApplyBlock/RollbackTo
// update sync_state itself (only the hash is ever supplied; indexed_height
// is mechanically derived by address_balance_distribution_state_validate_
// checkpoint_trigger from blocks.height, never trusted from the caller).
// hash == nil represents the uninitialized/bootstrap state (height stays
// -1) — never used by ApplyBlock/RollbackTo themselves (both always have a
// real block hash), but reused by the backfill command for the "sync_state
// itself is still uninitialized" case.
//
// This is called unconditionally on every ApplyBlock/RollbackTo call,
// regardless of whether this block's deltas were empty — the core
// invariant (§5 of the Phase 2H.4a spec) is that the anchor equals
// sync_state's own checkpoint at EVERY committed canonical state, not only
// at states where a distribution-relevant address changed.
func setDistributionAnchor(ctx context.Context, tx pgx.Tx, hash *string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE address_balance_distribution_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'
	`, hash); err != nil {
		return fmt.Errorf("store: address balance distribution: update anchor: %w", err)
	}
	return nil
}
