package store

import (
	"context"
	"errors"
	"testing"
)

// TestBackfillAddressDistribution_EmptyDatabase covers §19 "empty
// database": sync_state uninitialized, nothing to backfill, anchor stays
// uninitialized, all buckets zero.
func TestBackfillAddressDistribution_EmptyDatabase(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	result, err := BackfillAddressDistribution(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillAddressDistribution: %v", err)
	}
	if result.TotalPositiveAddresses != 0 || result.TotalBalanceSatoshis != 0 {
		t.Fatalf("result = %+v, want zero", result)
	}
	if result.AnchorHash != "" || result.AnchorHeight != -1 {
		t.Fatalf("result anchor = (%q,%d), want (\"\",-1)", result.AnchorHash, result.AnchorHeight)
	}
	requireDistributionStateUninitialized(t, ctx, pool)
	for _, id := range DistributionBucketIDs() {
		count, balance := distBucket(t, ctx, pool, id)
		if count != 0 || balance != 0 {
			t.Errorf("bucket %s = (count=%d,balance=%d), want (0,0)", id, count, balance)
		}
	}
}

// TestBackfillAddressDistribution_PopulatedDatabase covers §19 "populated
// database", "all eight buckets populated", and "zero-balance historical
// addresses excluded". Seeds `addresses` and a raw checkpoint directly via
// SQL — mirroring TestVerifySupplyRollupCoverage_TableNotYetMigrated's
// pattern — rather than via ApplyBlock, because BackfillAddressDistribution
// operates purely on the addresses cache however it got there, and driving
// eight buckets spanning up to 5,000,000 QOGE through real coinbase
// transactions would collide with block_accounting's subsidy-limit
// invariant (unrelated to this feature). "Multiple UTXOs same address
// counted once", "no-address outputs absent", and "bare-multisig
// participants absent" are properties of the addresses cache itself
// (recomputeAddress, §7/§10) — already proven directly against real chain
// data in distribution_test.go's incremental-maintenance tests — not
// re-derived by the backfill, which never reads transaction_outputs/
// output_participants at all.
func TestBackfillAddressDistribution_PopulatedDatabase(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	genesisHash := hash64("backfillPop")
	if _, err := pool.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, 0, NULL, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, genesisHash); err != nil {
		t.Fatalf("insert raw genesis block: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'`, genesisHash); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}

	type seedAddr struct {
		address string
		balance int64
	}
	seeds := []seedAddr{
		{"qBackfillLt1", 1},
		{"qBackfill1_10a", 5 * SatoshisPerQOGE},
		{"qBackfill1_10b", 3 * SatoshisPerQOGE}, // second address, same bucket
		{"qBackfill10_100", 50 * SatoshisPerQOGE},
		{"qBackfill100_1k", 500 * SatoshisPerQOGE},
		{"qBackfill1k_10k", 5_000 * SatoshisPerQOGE},
		{"qBackfill10k_100k", 50_000 * SatoshisPerQOGE},
		{"qBackfill100k_1m", 500_000 * SatoshisPerQOGE},
		{"qBackfillGte1m", 5_000_000 * SatoshisPerQOGE},
		{"qBackfillZeroHistorical", 0}, // zero-balance historical row
	}
	for _, sd := range seeds {
		if _, err := pool.Exec(ctx, `
			INSERT INTO addresses (address, total_received_satoshis, total_sent_satoshis, balance_satoshis, tx_count)
			VALUES ($1, $2, 0, $2, 1)
		`, sd.address, sd.balance); err != nil {
			t.Fatalf("seed address %s: %v", sd.address, err)
		}
	}
	requireDistributionStateUninitialized(t, ctx, pool)

	result, err := BackfillAddressDistribution(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillAddressDistribution: %v", err)
	}
	if result.AnchorHash != genesisHash || result.AnchorHeight != 0 {
		t.Fatalf("result anchor = (%s,%d), want (%s,0)", result.AnchorHash, result.AnchorHeight, genesisHash)
	}

	want := map[string]struct {
		count   int64
		balance int64
	}{
		"lt_1":     {1, 1},
		"1_10":     {2, 5*SatoshisPerQOGE + 3*SatoshisPerQOGE},
		"10_100":   {1, 50 * SatoshisPerQOGE},
		"100_1k":   {1, 500 * SatoshisPerQOGE},
		"1k_10k":   {1, 5_000 * SatoshisPerQOGE},
		"10k_100k": {1, 50_000 * SatoshisPerQOGE},
		"100k_1m":  {1, 500_000 * SatoshisPerQOGE},
		"gte_1m":   {1, 5_000_000 * SatoshisPerQOGE},
	}
	for id, w := range want {
		count, balance := distBucket(t, ctx, pool, id)
		if count != w.count || balance != w.balance {
			t.Errorf("bucket %s = (count=%d,balance=%d), want (%d,%d)", id, count, balance, w.count, w.balance)
		}
	}
	// qBackfillZeroHistorical (balance 0) must not appear in any bucket.
	requireBucketsSumToAddresses(t, ctx, pool)
	if result.TotalPositiveAddresses != int64(len(seeds)-1) {
		t.Errorf("TotalPositiveAddresses = %d, want %d (excludes the zero-balance row)", result.TotalPositiveAddresses, len(seeds)-1)
	}

	// Idempotent: rerunning produces byte-identical results.
	result2, err := BackfillAddressDistribution(ctx, pool)
	if err != nil {
		t.Fatalf("second BackfillAddressDistribution: %v", err)
	}
	if result2 != result {
		t.Fatalf("second backfill result = %+v, want identical to first %+v", result2, result)
	}
}

// TestBackfillAddressDistribution_CrossCheckFailure covers §19
// "mismatch/failure rolls back completely": if the bucket-definition rows
// disagree with the independent literal-boundary cross-check (simulated by
// corrupting one bucket's own min/max via direct SQL, bypassing the
// immutable-bounds trigger through a session-local trigger disable), the
// whole run must fail and publish nothing.
func TestBackfillAddressDistribution_CrossCheckFailure(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("backfillXCheck"), 100, "",
		coinbaseTx(hash64("backfillXChecktx"), out(0, 5*SatoshisPerQOGE, "qBackfillXCheck")),
	)
	mustApply(t, ctx, s, block)

	// Corrupt the "1_10" bucket's own max_balance_satoshis DOWN below
	// qBackfillXCheck's real 5-QOGE (500,000,000 satoshi) balance, so the
	// join-based rebuild (which reads this table's own bucket-definition
	// rows) no longer matches this address to "1_10" at all, while
	// distributionCrossCheckSQL's literal, hardcoded boundary (unaffected
	// by this corruption) still does — guaranteeing a disagreement.
	if _, err := pool.Exec(ctx, `ALTER TABLE address_balance_distribution DISABLE TRIGGER address_balance_distribution_immutable_bounds_trigger`); err != nil {
		t.Fatalf("disable immutability trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE address_balance_distribution SET max_balance_satoshis = 300000000 WHERE bucket_id = '1_10'`); err != nil {
		t.Fatalf("corrupt bucket definition: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE address_balance_distribution ENABLE TRIGGER address_balance_distribution_immutable_bounds_trigger`); err != nil {
		t.Fatalf("re-enable immutability trigger: %v", err)
	}

	before, _ := distBucket(t, ctx, pool, "1_10")

	_, err := BackfillAddressDistribution(ctx, pool)
	if err == nil {
		t.Fatal("expected BackfillAddressDistribution to fail on cross-check mismatch")
	}
	if !errors.Is(err, ErrDistributionCrossCheckFailed) {
		t.Fatalf("expected ErrDistributionCrossCheckFailed, got: %v", err)
	}

	// Nothing published: the corrupted bucket's mutable columns are
	// unchanged (still whatever ApplyBlock's live path had already
	// written, "before").
	after, _ := distBucket(t, ctx, pool, "1_10")
	if after != before {
		t.Errorf("bucket 1_10 count changed from %d to %d despite failed cross-check (nothing should be published)", before, after)
	}
}

// TestBackfillAddressDistribution_ConcurrentLock covers the advisory-lock
// concurrency guard, mirroring TestBackfillSupplyRollup's own lock test
// pattern: a lock held under the SAME namespace/schema key
// BackfillAddressDistribution uses must cause a concurrent call to fail
// with ErrDistributionBackfillAlreadyRunning.
func TestBackfillAddressDistribution_ConcurrentLock(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock-holder connection: %v", err)
	}
	defer holder.Release()

	schemaKey, _, err := schemaScopedLockKey(ctx, holder)
	if err != nil {
		t.Fatalf("schemaScopedLockKey: %v", err)
	}
	var locked bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, backfillDistributionAdvisoryLockNamespace, schemaKey).Scan(&locked); err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}
	if !locked {
		t.Fatal("failed to acquire advisory lock as precondition")
	}
	defer func() {
		_, _ = holder.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, backfillDistributionAdvisoryLockNamespace, schemaKey)
	}()

	_, err = BackfillAddressDistribution(ctx, pool)
	if !errors.Is(err, ErrDistributionBackfillAlreadyRunning) {
		t.Fatalf("expected ErrDistributionBackfillAlreadyRunning, got: %v", err)
	}
}
