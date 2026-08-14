package store

import (
	"context"
	"errors"
	"testing"
)

func TestVerifySupplyRollupCoverage_FreshDatabase(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	if err := VerifySupplyRollupCoverage(ctx, pool); err != nil {
		t.Errorf("VerifySupplyRollupCoverage on a fresh (pre-genesis) database: %v, want nil", err)
	}
}

func TestVerifySupplyRollupCoverage_TableNotYetMigrated(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)

	// Migrate through 0004 only — block_supply_rollup does not exist yet.
	migrations := loadTestMigrations(t)
	idx0005 := -1
	for i, m := range migrations {
		if m.Version == 5 {
			idx0005 = i
		}
	}
	if idx0005 == -1 {
		t.Fatal("migration 0005 not found")
	}
	if _, err := Up(ctx, pool, migrations[:idx0005]); err != nil {
		t.Fatalf("Up through 0004: %v", err)
	}

	// Simulate an OLD database indexed by a pre-2H.2a binary: real chain
	// data and a real advanced checkpoint, entirely via raw SQL (never
	// ApplyBlock, which now unconditionally requires block_supply_rollup to
	// exist — the whole point of this test is the state BEFORE migration
	// 0005 has ever been applied).
	genesisHash := hash64("pfl0")
	if _, err := pool.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, 0, NULL, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, genesisHash); err != nil {
		t.Fatalf("insert raw genesis block: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'`, genesisHash); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}

	if err := VerifySupplyRollupCoverage(ctx, pool); err != nil {
		t.Errorf("VerifySupplyRollupCoverage before migration 0005 is applied: %v, want nil", err)
	}
}

func TestVerifySupplyRollupCoverage_TipCovered(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("pflc0"), 0, "", coinbaseTx(hash64("pflc0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	if err := VerifySupplyRollupCoverage(ctx, pool); err != nil {
		t.Errorf("VerifySupplyRollupCoverage with a fully covered tip: %v, want nil", err)
	}
}

func TestVerifySupplyRollupCoverage_TipMissing(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("pflm0"), 0, "", coinbaseTx(hash64("pflm0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup WHERE block_hash = $1`, g.Hash); err != nil {
		t.Fatalf("delete tip rollup: %v", err)
	}

	err := VerifySupplyRollupCoverage(ctx, pool)
	if err == nil {
		t.Fatal("expected VerifySupplyRollupCoverage to fail when the tip has no rollup row")
	}
	if !errors.Is(err, ErrSupplyRollupCoverageMissing) {
		t.Errorf("error = %v, want ErrSupplyRollupCoverageMissing", err)
	}
}

// TestVerifySupplyRollupCoverage_ArbitraryBootstrapChainFlagged proves the
// preflight correctly flags an arbitrary-bootstrap chain too — it has no
// rollup coverage at all (task item 18), and this preflight has no way to
// distinguish "legitimately never had genesis" from "migrated 0005 but
// forgot to backfill" at the tip alone; both need an operator's attention
// (backfill-supply-rollup is a safe no-op if genuinely nothing can be
// backfilled — see BackfillSupplyRollup's canonical shape preflight, which
// WOULD catch a true arbitrary-bootstrap chain and report
// ErrSupplyRollupCanonicalShapeInvalid, a clearer signal for that specific
// case).
func TestVerifySupplyRollupCoverage_ArbitraryBootstrapChainFlagged(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	boot := testBlock(hash64("pflab100"), 100, "", coinbaseTx(hash64("pflab100tx"), out(0, era0Subsidy, "qAlice")))
	if err := s.ApplyBlock(ctx, boot); err != nil {
		t.Fatalf("bootstrap ApplyBlock: %v", err)
	}

	err := VerifySupplyRollupCoverage(ctx, pool)
	if err == nil {
		t.Fatal("expected VerifySupplyRollupCoverage to flag an arbitrary-bootstrap chain (no rollup coverage at all)")
	}
	if !errors.Is(err, ErrSupplyRollupCoverageMissing) {
		t.Errorf("error = %v, want ErrSupplyRollupCoverageMissing", err)
	}
}
