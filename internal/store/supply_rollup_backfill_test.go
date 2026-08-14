package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── basic backfill from a legacy (pre-0005) database ────────────────────

func TestBackfillSupplyRollup_LegacyDatabase(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfr0"), 0, "", coinbaseTx(hash64("bfr0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	block1 := testBlock(hash64("bfr1"), 1, g.Hash, coinbaseTx(hash64("bfr1tx"), out(0, era0Subsidy, "qMiner1")))
	mustApply(t, ctx, s, block1)
	block2 := testBlock(hash64("bfr2"), 2, block1.Hash, coinbaseTx(hash64("bfr2tx"), out(0, era0Subsidy, "qMiner2")))
	mustApply(t, ctx, s, block2)

	// Simulate the legacy pre-0005 scenario: blocks/accounting indexed, but
	// block_supply_rollup never populated.
	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup`); err != nil {
		t.Fatalf("clear rollups: %v", err)
	}

	result, err := BackfillSupplyRollup(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillSupplyRollup: %v", err)
	}
	if result.CanonicalBlocks != 3 {
		t.Errorf("CanonicalBlocks = %d, want 3", result.CanonicalBlocks)
	}
	if result.Inserted != 3 {
		t.Errorf("Inserted = %d, want 3", result.Inserted)
	}
	if result.Verified != 0 {
		t.Errorf("Verified = %d, want 0", result.Verified)
	}
	if result.TipHash != block2.Hash || result.TipHeight != 2 {
		t.Errorf("tip = %s@%d, want %s@2", result.TipHash, result.TipHeight, block2.Hash)
	}

	for _, h := range []string{g.Hash, block1.Hash, block2.Hash} {
		r, err := s.GetBlockSupplyRollup(ctx, h)
		if err != nil {
			t.Fatalf("GetBlockSupplyRollup(%s): %v", h, err)
		}
		if r == nil {
			t.Errorf("expected a rollup row for %s", h)
		}
	}

	tip, err := s.GetBlockSupplyRollup(ctx, block2.Hash)
	if err != nil || tip == nil {
		t.Fatalf("GetBlockSupplyRollup(tip): r=%+v err=%v", tip, err)
	}
	// Genesis is excluded (never enters the UTXO set); block1's and
	// block2's full-claim coinbases are both live.
	if want := 2 * era0Subsidy; tip.CumulativeUTXOSetValueSatoshis != want {
		t.Errorf("tip CumulativeUTXOSetValueSatoshis = %d, want %d", tip.CumulativeUTXOSetValueSatoshis, want)
	}
}

// ─── idempotency: rerun after full backfill is a safe no-op ──────────────

func TestBackfillSupplyRollup_IdempotentRerun(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfi0"), 0, "", coinbaseTx(hash64("bfi0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	block1 := testBlock(hash64("bfi1"), 1, g.Hash, coinbaseTx(hash64("bfi1tx"), out(0, era0Subsidy, "qMiner1")))
	mustApply(t, ctx, s, block1)

	// First run: ordinary live ApplyBlock already wrote both rollups, so
	// this is already a from-scratch idempotent verification.
	result, err := BackfillSupplyRollup(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillSupplyRollup (first): %v", err)
	}
	if result.Inserted != 0 || result.Verified != 2 {
		t.Errorf("first run = %+v, want Inserted=0 Verified=2", result)
	}

	// Second run: identical.
	result2, err := BackfillSupplyRollup(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillSupplyRollup (second): %v", err)
	}
	if result2.Inserted != 0 || result2.Verified != 2 {
		t.Errorf("second run = %+v, want Inserted=0 Verified=2", result2)
	}
}

// ─── partial gap: some rows exist (from live ApplyBlock), some don't ─────

func TestBackfillSupplyRollup_FillsOnlyMissingRows(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfp0"), 0, "", coinbaseTx(hash64("bfp0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	block1 := testBlock(hash64("bfp1"), 1, g.Hash, coinbaseTx(hash64("bfp1tx"), out(0, era0Subsidy, "qMiner1")))
	mustApply(t, ctx, s, block1)
	block2 := testBlock(hash64("bfp2"), 2, block1.Hash, coinbaseTx(hash64("bfp2tx"), out(0, era0Subsidy, "qMiner2")))
	mustApply(t, ctx, s, block2)

	// Delete only block1's rollup — genesis and block2's remain.
	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup WHERE block_hash = $1`, block1.Hash); err != nil {
		t.Fatalf("delete block1 rollup: %v", err)
	}

	result, err := BackfillSupplyRollup(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillSupplyRollup: %v", err)
	}
	if result.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1", result.Inserted)
	}
	if result.Verified != 2 {
		t.Errorf("Verified = %d, want 2", result.Verified)
	}

	r, err := s.GetBlockSupplyRollup(ctx, block1.Hash)
	if err != nil || r == nil {
		t.Fatalf("GetBlockSupplyRollup(block1) after fill: r=%+v err=%v", r, err)
	}
}

// ─── contradiction: an existing row disagrees with the recomputed value ──

func TestBackfillSupplyRollup_ContradictionDetected(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfc0"), 0, "", coinbaseTx(hash64("bfc0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	block1 := testBlock(hash64("bfc1"), 1, g.Hash, coinbaseTx(hash64("bfc1tx"), out(0, era0Subsidy, "qMiner1")))
	mustApply(t, ctx, s, block1)

	// Corrupt an already-persisted rollup row directly — inflate subsidy,
	// coinbase output, and UTXO value together by the same delta so the
	// row still satisfies block_supply_rollup's own CHECK constraints
	// (reward identity, UTXO identity) but no longer matches what a fresh
	// computation from block_accounting would produce.
	if _, err := pool.Exec(ctx, `
		UPDATE block_supply_rollup SET
			cumulative_subsidy_satoshis = cumulative_subsidy_satoshis + 1000,
			cumulative_coinbase_output_satoshis = cumulative_coinbase_output_satoshis + 1000,
			cumulative_utxo_set_value_satoshis = cumulative_utxo_set_value_satoshis + 1000
		WHERE block_hash = $1
	`, block1.Hash); err != nil {
		t.Fatalf("corrupt rollup: %v", err)
	}

	_, err := BackfillSupplyRollup(ctx, pool)
	if err == nil {
		t.Fatal("expected BackfillSupplyRollup to reject a contradictory existing row")
	}
	if !errors.Is(err, ErrImmutableConflict) {
		t.Errorf("error = %v, want ErrImmutableConflict", err)
	}
}

// ─── canonical shape invalid: a height gap ────────────────────────────────

func TestBackfillSupplyRollup_CanonicalShapeGap(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestStore(t)
	ctxBg := context.Background()

	// Fabricate an "impossible" state directly via SQL: canonical blocks at
	// heights 1, 2, 3 (no height 0), sync_state advanced to height 2 (three
	// canonical rows == indexed_height(2)+1 == 3, so the count check alone
	// cannot catch this — only min(height) != 0 can).
	h1, h2, h3 := hash64("bfg1"), hash64("bfg2"), hash64("bfg3")
	insertRawSupplyBlock(t, ctxBg, pool, h1, 1, "")
	insertRawSupplyBlock(t, ctxBg, pool, h2, 2, h1)
	insertRawSupplyBlock(t, ctxBg, pool, h3, 3, h2)
	insertRawSupplyAccounting(t, ctxBg, pool, h1, era0Subsidy)
	insertRawSupplyAccounting(t, ctxBg, pool, h2, era0Subsidy)
	insertRawSupplyAccounting(t, ctxBg, pool, h3, era0Subsidy)
	if _, err := pool.Exec(ctx, `UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'`, h2); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}

	_, err := BackfillSupplyRollup(ctx, pool)
	if err == nil {
		t.Fatal("expected BackfillSupplyRollup to reject a canonical height gap")
	}
	if !errors.Is(err, ErrSupplyRollupCanonicalShapeInvalid) {
		t.Errorf("error = %v, want ErrSupplyRollupCanonicalShapeInvalid", err)
	}
}

// ─── source incomplete: a canonical block missing block_accounting ───────

func TestBackfillSupplyRollup_SourceIncomplete(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfsi0"), 0, "", coinbaseTx(hash64("bfsi0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	block1 := testBlock(hash64("bfsi1"), 1, g.Hash, coinbaseTx(hash64("bfsi1tx"), out(0, era0Subsidy, "qMiner1")))
	mustApply(t, ctx, s, block1)

	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting WHERE block_hash = $1`, block1.Hash); err != nil {
		t.Fatalf("delete block_accounting: %v", err)
	}

	_, err := BackfillSupplyRollup(ctx, pool)
	if err == nil {
		t.Fatal("expected BackfillSupplyRollup to reject a canonical block missing block_accounting")
	}
	if !errors.Is(err, ErrSupplyRollupSourceIncomplete) {
		t.Errorf("error = %v, want ErrSupplyRollupSourceIncomplete", err)
	}
}

// ─── cross-check failure: independently computed UTXO scan disagrees ─────

func TestBackfillSupplyRollup_UTXOCrossCheckFailure(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfx0"), 0, "", coinbaseTx(hash64("bfx0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	block1 := testBlock(hash64("bfx1"), 1, g.Hash, coinbaseTx(hash64("bfx1tx"), out(0, era0Subsidy, "qMiner1")))
	mustApply(t, ctx, s, block1)

	// Delete existing rollups so a fresh computation runs, then corrupt
	// utxo_state directly (simulate a desync between the accounting-derived
	// view and the actual live UTXO set) without touching block_accounting
	// — the two sources now disagree.
	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup`); err != nil {
		t.Fatalf("clear rollups: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM utxo_state WHERE txid = $1`, hash64("bfx1tx")); err != nil {
		t.Fatalf("corrupt utxo_state: %v", err)
	}

	_, err := BackfillSupplyRollup(ctx, pool)
	if err == nil {
		t.Fatal("expected BackfillSupplyRollup to reject a UTXO cross-check mismatch")
	}
	if !errors.Is(err, ErrSupplyRollupCrossCheckFailed) {
		t.Errorf("error = %v, want ErrSupplyRollupCrossCheckFailed", err)
	}

	// Nothing must have been published.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_supply_rollup`).Scan(&count); err != nil {
		t.Fatalf("count block_supply_rollup: %v", err)
	}
	if count != 0 {
		t.Errorf("rows published despite failed cross-check: count = %d", count)
	}
}

// ─── uninitialized store: nothing to do ───────────────────────────────────

func TestBackfillSupplyRollup_UninitializedStoreIsNoOp(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	result, err := BackfillSupplyRollup(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillSupplyRollup on uninitialized store: %v", err)
	}
	if result.CanonicalBlocks != 0 {
		t.Errorf("CanonicalBlocks = %d, want 0", result.CanonicalBlocks)
	}
}

// ─── concurrency: same schema excluded, different schema independent ─────

func TestBackfillSupplyRollup_ConcurrentRunRejected(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestStore(t)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	defer conn.Release()

	schemaKey, _, err := schemaScopedLockKey(ctx, conn)
	if err != nil {
		t.Fatalf("schemaScopedLockKey: %v", err)
	}

	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, backfillSupplyRollupAdvisoryLockNamespace, schemaKey).Scan(&acquired); err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}
	if !acquired {
		t.Fatal("fixture bug: could not acquire the advisory lock to simulate a concurrent run")
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, backfillSupplyRollupAdvisoryLockNamespace, schemaKey)
	}()

	_, err = BackfillSupplyRollup(ctx, pool)
	if !errors.Is(err, ErrSupplyRollupBackfillAlreadyRunning) {
		t.Fatalf("error = %v, want ErrSupplyRollupBackfillAlreadyRunning", err)
	}
}

func TestBackfillSupplyRollup_DifferentSchemaNotBlocked(t *testing.T) {
	ctx := context.Background()
	sA, poolA := newTestStore(t)
	sB, poolB := newTestStore(t)

	seed := func(s *Store, label string) {
		g := testBlock(hash64(label+"genesis"), 0, "", coinbaseTx(hash64(label+"cb"), out(0, era0Subsidy, "q"+label)))
		mustApply(t, ctx, s, g)
	}
	seed(sA, "bfdsA")
	seed(sB, "bfdsB")

	connA, err := poolA.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire schema A lock connection: %v", err)
	}
	defer connA.Release()

	schemaKeyA, schemaNameA, err := schemaScopedLockKey(ctx, connA)
	if err != nil {
		t.Fatalf("schemaScopedLockKey(A): %v", err)
	}

	var lockedA bool
	if err := connA.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, backfillSupplyRollupAdvisoryLockNamespace, schemaKeyA).Scan(&lockedA); err != nil {
		t.Fatalf("acquire schema A advisory lock: %v", err)
	}
	if !lockedA {
		t.Fatal("fixture bug: could not acquire schema A's advisory lock")
	}
	defer func() {
		_, _ = connA.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, backfillSupplyRollupAdvisoryLockNamespace, schemaKeyA)
	}()

	connB, err := poolB.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire schema B connection: %v", err)
	}
	schemaKeyB, schemaNameB, err := schemaScopedLockKey(ctx, connB)
	connB.Release()
	if err != nil {
		t.Fatalf("schemaScopedLockKey(B): %v", err)
	}
	if schemaNameA == schemaNameB {
		t.Fatalf("fixture bug: schema A and schema B are the same schema (%s)", schemaNameA)
	}
	if schemaKeyA == schemaKeyB {
		t.Fatalf("fixture bug: schema A and schema B hashed to the same lock key (%d)", schemaKeyA)
	}

	if _, err := BackfillSupplyRollup(ctx, poolB); err != nil {
		t.Fatalf("BackfillSupplyRollup on independent schema B failed while schema A's lock was held: %v", err)
	}
}

// insertRawSupplyBlock inserts a minimal canonical block directly via SQL,
// bypassing Store's write path entirely — used to construct "impossible"
// canonical shapes that ApplyBlock's own continuity rule would never
// produce (see checkCanonicalContinuity), so BackfillSupplyRollup's
// independent shape-scan preflight can be proven against them.
func insertRawSupplyBlock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hash string, height int64, prevHash string) {
	t.Helper()
	var prev *string
	if prevHash != "" {
		prev = &prevHash
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, $2, $3, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, hash, height, prev); err != nil {
		t.Fatalf("insert raw block %s: %v", hash, err)
	}
}

func insertRawSupplyAccounting(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hash string, subsidy int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO block_accounting (block_hash, subsidy_satoshis, fee_satoshis, coinbase_output_satoshis, unclaimed_reward_satoshis)
		VALUES ($1, $2, 0, $2, 0)
	`, hash, subsidy); err != nil {
		t.Fatalf("insert raw block_accounting %s: %v", hash, err)
	}
}
