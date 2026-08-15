package store

import (
	"context"
	"errors"
	"testing"
)

// TestVerifyDistributionCoverage_FreshDatabase covers §14's first outcome:
// sync_state uninitialized, always nil regardless of distribution state.
func TestVerifyDistributionCoverage_FreshDatabase(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	if err := VerifyDistributionCoverage(ctx, pool); err != nil {
		t.Fatalf("VerifyDistributionCoverage on a fresh database: %v", err)
	}
}

// TestVerifyDistributionCoverage_MigrationNotApplied covers §14's second
// outcome: migration 0007 not applied at all (table doesn't exist) is nil,
// even with an initialized sync_state.
func TestVerifyDistributionCoverage_MigrationNotApplied(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	var through0006 []Migration
	for i, m := range migrations {
		if m.Version == 6 {
			through0006 = migrations[:i+1]
			break
		}
	}
	if through0006 == nil {
		t.Fatal("migration 0006 not found among loaded migrations")
	}
	if _, err := Up(ctx, pool, through0006); err != nil {
		t.Fatalf("Up through 0006: %v", err)
	}

	// Simulate an OLD database indexed by a pre-2H.4a binary: real chain
	// data and an advanced checkpoint, via raw SQL — never ApplyBlock,
	// which now unconditionally requires address_balance_distribution to
	// exist (the whole point of this test is the state BEFORE migration
	// 0007 has ever been applied).
	genesisHash := hash64("distPreflightNoMig")
	if _, err := pool.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, 0, NULL, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, genesisHash); err != nil {
		t.Fatalf("insert raw genesis block: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'`, genesisHash); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}

	if err := VerifyDistributionCoverage(ctx, pool); err != nil {
		t.Fatalf("VerifyDistributionCoverage with migration 0007 not applied: %v", err)
	}
}

// TestVerifyDistributionCoverage_UpToDate covers §14's third outcome
// (matching case): after a normal ApplyBlock, the anchor is kept in
// lockstep by the live incremental maintenance, so coverage must pass.
func TestVerifyDistributionCoverage_UpToDate(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("distPreflightOK"), 100, "",
		coinbaseTx(hash64("distPreflightOKtx"), out(0, 1*SatoshisPerQOGE, "qDistPreflightOK")),
	)
	mustApply(t, ctx, s, block)

	if err := VerifyDistributionCoverage(ctx, pool); err != nil {
		t.Fatalf("VerifyDistributionCoverage after normal indexing: %v", err)
	}
}

// TestVerifyDistributionCoverage_LegacyGapDetected covers §14's mismatch
// case: sync_state has an indexed tip but address_balance_distribution_state
// was never brought forward — simulating "migration 0007 applied to an
// already-indexed legacy database, backfill-address-distribution never
// run": sync_state is advanced directly via raw SQL (never ApplyBlock,
// which would keep address_balance_distribution_state in lockstep — the
// whole point of this test is the gap that exists BEFORE a backfill ever
// runs), leaving address_balance_distribution_state at migration 0007's
// seeded -1/NULL. Index startup must refuse.
func TestVerifyDistributionCoverage_LegacyGapDetected(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	genesisHash := hash64("distPreflightGap")
	if _, err := pool.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, 0, NULL, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, genesisHash); err != nil {
		t.Fatalf("insert raw genesis block: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'`, genesisHash); err != nil {
		t.Fatalf("set checkpoint: %v", err)
	}
	requireDistributionStateUninitialized(t, ctx, pool)

	err := VerifyDistributionCoverage(ctx, pool)
	if !errors.Is(err, ErrDistributionCoverageMissing) {
		t.Fatalf("expected ErrDistributionCoverageMissing, got: %v", err)
	}
}
