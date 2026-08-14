package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertOrphanBlock inserts a minimal, valid ORPHANED (canonical=false)
// block directly via SQL — used as the "wrong" prev_hash target for the
// broken-ancestry regression test below. Its own prev_hash is NULL so it
// carries no FK dependency on any other fixture block.
func insertOrphanBlock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hash string, height int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count, canonical, orphaned_at)
		VALUES ($1, $2, NULL, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1, false, now())
	`, hash, height); err != nil {
		t.Fatalf("insert orphan block %s: %v", hash, err)
	}
}

// ─── 5: broken parent link — the critical regression ─────────────────────

func TestBackfillSupplyRollup_BrokenAncestryDetected(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfa0"), 0, "", coinbaseTx(hash64("bfa0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	a := testBlock(hash64("bfaA"), 1, g.Hash, coinbaseTx(hash64("bfaAtx"), out(0, era0Subsidy, "qMinerA")))
	mustApply(t, ctx, s, a)
	b := testBlock(hash64("bfaB"), 2, a.Hash, coinbaseTx(hash64("bfaBtx"), out(0, era0Subsidy, "qMinerB")))
	mustApply(t, ctx, s, b)

	// An unrelated orphaned block X, structurally valid, but NOT b's real
	// parent.
	x := hash64("bfaOrphanX")
	insertOrphanBlock(t, ctx, pool, x, 1)

	// Mutate b's prev_hash to point at X instead of a — canonical flags,
	// heights, block_accounting, transaction_outputs, and utxo_state are
	// all left exactly as ApplyBlock wrote them. The canonical shape
	// (count/min/max, accounting completeness, monetary/UTXO totals) is
	// UNCHANGED by this mutation — only ancestry is broken.
	if _, err := pool.Exec(ctx, `UPDATE blocks SET prev_hash = $1 WHERE hash = $2`, x, b.Hash); err != nil {
		t.Fatalf("mutate b.prev_hash: %v", err)
	}

	// Simulate a legacy database: no rollups yet.
	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup`); err != nil {
		t.Fatalf("clear rollups: %v", err)
	}

	_, err := BackfillSupplyRollup(ctx, pool)
	if err == nil {
		t.Fatal("expected BackfillSupplyRollup to reject a broken canonical ancestry link")
	}
	if !errors.Is(err, ErrSupplyRollupCanonicalAncestryInvalid) {
		t.Errorf("error = %v, want ErrSupplyRollupCanonicalAncestryInvalid", err)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_supply_rollup`).Scan(&rowCount); err != nil {
		t.Fatalf("count block_supply_rollup: %v", err)
	}
	if rowCount != 0 {
		t.Errorf("rows published despite broken ancestry: count = %d", rowCount)
	}
}

// ─── 6: genesis prev_hash must be NULL ────────────────────────────────────

func TestBackfillSupplyRollup_GenesisPrevHashMustBeNull(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfg0"), 0, "", coinbaseTx(hash64("bfg0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	a := testBlock(hash64("bfgA"), 1, g.Hash, coinbaseTx(hash64("bfgAtx"), out(0, era0Subsidy, "qMinerA")))
	mustApply(t, ctx, s, a)

	// Mutate genesis's prev_hash to a non-NULL value (pointing at an
	// otherwise-unrelated existing block, to satisfy the prev_hash FK).
	// count/min/max shape remains valid; only genesis's own ancestry claim
	// is now nonsensical.
	if _, err := pool.Exec(ctx, `UPDATE blocks SET prev_hash = $1 WHERE hash = $2`, a.Hash, g.Hash); err != nil {
		t.Fatalf("mutate genesis.prev_hash: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup`); err != nil {
		t.Fatalf("clear rollups: %v", err)
	}

	_, err := BackfillSupplyRollup(ctx, pool)
	if err == nil {
		t.Fatal("expected BackfillSupplyRollup to reject a non-NULL genesis prev_hash")
	}
	if !errors.Is(err, ErrSupplyRollupCanonicalAncestryInvalid) {
		t.Errorf("error = %v, want ErrSupplyRollupCanonicalAncestryInvalid", err)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_supply_rollup`).Scan(&rowCount); err != nil {
		t.Fatalf("count block_supply_rollup: %v", err)
	}
	if rowCount != 0 {
		t.Errorf("rows published despite invalid genesis ancestry: count = %d", rowCount)
	}
}

// ─── 7: a normal valid chain passes ancestry validation unchanged ────────

func TestBackfillSupplyRollup_NormalChainAncestryValid(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfn0"), 0, "", coinbaseTx(hash64("bfn0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	a := testBlock(hash64("bfnA"), 1, g.Hash, coinbaseTx(hash64("bfnAtx"), out(0, era0Subsidy, "qMinerA")))
	mustApply(t, ctx, s, a)
	b := testBlock(hash64("bfnB"), 2, a.Hash, coinbaseTx(hash64("bfnBtx"), out(0, era0Subsidy, "qMinerB")))
	mustApply(t, ctx, s, b)
	c := testBlock(hash64("bfnC"), 3, b.Hash, coinbaseTx(hash64("bfnCtx"), out(0, era0Subsidy, "qMinerC")))
	mustApply(t, ctx, s, c)

	want, err := s.GetBlockSupplyRollup(ctx, c.Hash)
	if err != nil || want == nil {
		t.Fatalf("GetBlockSupplyRollup(c) before: r=%+v err=%v", want, err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup`); err != nil {
		t.Fatalf("clear rollups: %v", err)
	}

	result, err := BackfillSupplyRollup(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillSupplyRollup on a valid chain: %v", err)
	}
	if result.CanonicalBlocks != 4 || result.Inserted != 4 {
		t.Errorf("result = %+v, want CanonicalBlocks=4 Inserted=4", result)
	}

	got, err := s.GetBlockSupplyRollup(ctx, c.Hash)
	if err != nil || got == nil {
		t.Fatalf("GetBlockSupplyRollup(c) after: r=%+v err=%v", got, err)
	}
	if *got != *want {
		t.Errorf("backfilled rollup differs from live-computed rollup: got=%+v want=%+v", got, want)
	}
}

// ─── 8: reorged canonical chain — backfill follows the CURRENT chain ─────

func TestBackfillSupplyRollup_ReorgedChainAncestryValid(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("bfr0"), 0, "", coinbaseTx(hash64("bfr0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	a := testBlock(hash64("bfrA"), 1, g.Hash, coinbaseTx(hash64("bfrAtx"), out(0, era0Subsidy, "qMinerA")))
	mustApply(t, ctx, s, a)
	b := testBlock(hash64("bfrB"), 2, a.Hash, coinbaseTx(hash64("bfrBtx"), out(0, era0Subsidy, "qMinerB")))
	mustApply(t, ctx, s, b)

	if err := s.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("RollbackTo(genesis): %v", err)
	}

	c := testBlock(hash64("bfrC"), 1, g.Hash, coinbaseTx(hash64("bfrCtx"), out(0, era0Subsidy, "qMinerC")))
	mustApply(t, ctx, s, c)
	d := testBlock(hash64("bfrD"), 2, c.Hash, coinbaseTx(hash64("bfrDtx"), out(0, era0Subsidy, "qMinerD")))
	mustApply(t, ctx, s, d)

	wantTip, err := s.GetBlockSupplyRollup(ctx, d.Hash)
	if err != nil || wantTip == nil {
		t.Fatalf("GetBlockSupplyRollup(d) before: r=%+v err=%v", wantTip, err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup`); err != nil {
		t.Fatalf("clear rollups: %v", err)
	}

	result, err := BackfillSupplyRollup(ctx, pool)
	if err != nil {
		t.Fatalf("BackfillSupplyRollup on a reorged chain: %v", err)
	}
	// Only the CURRENT canonical chain (g, c, d) is considered — a and b
	// are orphaned and never participate.
	if result.CanonicalBlocks != 3 {
		t.Errorf("CanonicalBlocks = %d, want 3 (genesis, c, d — a/b are orphaned)", result.CanonicalBlocks)
	}
	if result.TipHash != d.Hash {
		t.Errorf("TipHash = %s, want %s (d)", result.TipHash, d.Hash)
	}

	gotTip, err := s.GetBlockSupplyRollup(ctx, d.Hash)
	if err != nil || gotTip == nil {
		t.Fatalf("GetBlockSupplyRollup(d) after: r=%+v err=%v", gotTip, err)
	}
	if *gotTip != *wantTip {
		t.Errorf("backfilled tip rollup differs from live-computed rollup: got=%+v want=%+v", gotTip, wantTip)
	}

	// The orphaned branch (a, b) must never gain a rollup via this
	// canonical-only backfill.
	orphanRollup, err := s.GetBlockSupplyRollup(ctx, b.Hash)
	if err != nil {
		t.Fatalf("GetBlockSupplyRollup(b, orphaned): %v", err)
	}
	if orphanRollup != nil {
		t.Errorf("expected no rollup for orphaned block b, got %+v", orphanRollup)
	}
}
