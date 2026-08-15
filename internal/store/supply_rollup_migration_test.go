package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrations_SupplyRollupRoundTrip is task item 38: 0004 -> 0005 -> 0004
// -> 0005, proving existing data (from 0004 and earlier) survives the round
// trip and 0005's down migration removes ONLY block_supply_rollup. Bounded
// explicitly to the 0001-0005 prefix (through0005), not the full loaded
// migration set: Phase 2H.3 added migration 0006 on top of 0005, and this
// test's own "Up to 0005" / "Down one step (0005)" steps must stay pinned
// to 0005 regardless of what's layered on top later.
func TestMigrations_SupplyRollupRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	idx0005 := -1
	for i, m := range migrations {
		if m.Version == 5 {
			idx0005 = i
		}
	}
	if idx0005 == -1 {
		t.Fatal("migration 0005 not found among loaded migrations")
	}
	through0004 := migrations[:idx0005]
	through0005 := migrations[:idx0005+1]

	if _, err := Up(ctx, pool, through0004); err != nil {
		t.Fatalf("Up through 0004: %v", err)
	}
	if !tableExists(t, ctx, pool, "block_accounting") {
		t.Fatal("expected block_accounting to exist after migrating through 0004")
	}
	if tableExists(t, ctx, pool, "block_supply_rollup") {
		t.Fatal("block_supply_rollup must not exist before 0005 is applied")
	}

	// Seed data through 0004's tables, to prove it survives the 0005
	// up/down/up round trip below.
	blockHash := hash64("migRT0")
	if _, err := pool.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, 0, NULL, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, blockHash); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO block_accounting (block_hash, subsidy_satoshis, fee_satoshis, coinbase_output_satoshis, unclaimed_reward_satoshis)
		VALUES ($1, 100, 0, 100, 0)
	`, blockHash); err != nil {
		t.Fatalf("seed block_accounting: %v", err)
	}

	// 0004 -> 0005
	if _, err := Up(ctx, pool, through0005); err != nil {
		t.Fatalf("Up to 0005: %v", err)
	}
	if !tableExists(t, ctx, pool, "block_supply_rollup") {
		t.Fatal("expected block_supply_rollup to exist after 0005")
	}

	// 0005 -> 0004 (down one step)
	if _, err := Down(ctx, pool, through0005, 1); err != nil {
		t.Fatalf("Down 1 (0005): %v", err)
	}
	if tableExists(t, ctx, pool, "block_supply_rollup") {
		t.Error("block_supply_rollup must be gone after rolling back 0005")
	}
	if !tableExists(t, ctx, pool, "block_accounting") {
		t.Fatal("block_accounting must survive rolling back 0005 (0005's down must remove ONLY block_supply_rollup)")
	}
	requireCount(t, ctx, pool, "block_accounting", blockHash, 1)
	requireCount(t, ctx, pool, "blocks", blockHash, 1)

	// 0004 -> 0005 again
	if _, err := Up(ctx, pool, through0005); err != nil {
		t.Fatalf("re-Up to 0005: %v", err)
	}
	if !tableExists(t, ctx, pool, "block_supply_rollup") {
		t.Fatal("expected block_supply_rollup to exist after re-applying 0005")
	}
	requireCount(t, ctx, pool, "block_accounting", blockHash, 1)
	requireCount(t, ctx, pool, "blocks", blockHash, 1)
}

func requireCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, hash string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+pkColumn(table)+" = $1", hash).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != want {
		t.Errorf("%s row count for %s = %d, want %d", table, hash, count, want)
	}
}

func pkColumn(table string) string {
	switch table {
	case "blocks":
		return "hash"
	case "addresses":
		return "address"
	default:
		return "block_hash"
	}
}
