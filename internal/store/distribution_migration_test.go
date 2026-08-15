package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrations_DistributionRoundTrip is Phase 2H.4a spec item 17: 0006 ->
// 0007 -> 0006 -> 0007, proving migration 0007 creates ONLY the two
// distribution tables (and their triggers) and its down migration removes
// ONLY those, leaving addresses and its existing rows completely
// untouched, and migrations 0001-0006 unaffected.
func TestMigrations_DistributionRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	idx0006 := -1
	idx0007 := -1
	for i, m := range migrations {
		switch m.Version {
		case 6:
			idx0006 = i
		case 7:
			idx0007 = i
		}
	}
	if idx0006 == -1 {
		t.Fatal("migration 0006 not found among loaded migrations")
	}
	if idx0007 == -1 {
		t.Fatal("migration 0007 not found among loaded migrations")
	}
	through0006 := migrations[:idx0006+1]
	through0007 := migrations[:idx0007+1]

	if _, err := Up(ctx, pool, through0006); err != nil {
		t.Fatalf("Up through 0006: %v", err)
	}
	if !tableExists(t, ctx, pool, "addresses") {
		t.Fatal("expected addresses table to exist after 0006")
	}
	if tableExists(t, ctx, pool, "address_balance_distribution") {
		t.Fatal("address_balance_distribution must not exist before 0007 is applied")
	}
	if tableExists(t, ctx, pool, "address_balance_distribution_state") {
		t.Fatal("address_balance_distribution_state must not exist before 0007 is applied")
	}

	addr := "qDistributionMigrationTestAddr"
	if _, err := pool.Exec(ctx, `
		INSERT INTO addresses (address, total_received_satoshis, total_sent_satoshis, balance_satoshis, tx_count)
		VALUES ($1, 100, 0, 100, 1)
	`, addr); err != nil {
		t.Fatalf("seed addresses row: %v", err)
	}

	// 0006 -> 0007
	if _, err := Up(ctx, pool, through0007); err != nil {
		t.Fatalf("Up to 0007: %v", err)
	}
	if !tableExists(t, ctx, pool, "address_balance_distribution") {
		t.Fatal("expected address_balance_distribution to exist after 0007")
	}
	if !tableExists(t, ctx, pool, "address_balance_distribution_state") {
		t.Fatal("expected address_balance_distribution_state to exist after 0007")
	}
	requireCount(t, ctx, pool, "addresses", addr, 1)
	requireDistributionBucketCount(t, ctx, pool, 8)
	requireDistributionStateUninitialized(t, ctx, pool)

	// 0007 -> 0006 (down one step)
	if _, err := Down(ctx, pool, through0007, 1); err != nil {
		t.Fatalf("Down 1 (0007): %v", err)
	}
	if tableExists(t, ctx, pool, "address_balance_distribution") {
		t.Error("address_balance_distribution must be gone after rolling back 0007")
	}
	if tableExists(t, ctx, pool, "address_balance_distribution_state") {
		t.Error("address_balance_distribution_state must be gone after rolling back 0007")
	}
	if !tableExists(t, ctx, pool, "addresses") {
		t.Fatal("addresses table must survive rolling back 0007 (0007's down must remove ONLY its own tables)")
	}
	requireCount(t, ctx, pool, "addresses", addr, 1)

	// 0006 -> 0007 again
	if _, err := Up(ctx, pool, through0007); err != nil {
		t.Fatalf("re-Up to 0007: %v", err)
	}
	if !tableExists(t, ctx, pool, "address_balance_distribution") {
		t.Fatal("expected address_balance_distribution to exist after re-applying 0007")
	}
	requireCount(t, ctx, pool, "addresses", addr, 1)
	requireDistributionBucketCount(t, ctx, pool, 8)
	requireDistributionStateUninitialized(t, ctx, pool)
}

func requireDistributionBucketCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM address_balance_distribution`).Scan(&count); err != nil {
		t.Fatalf("count address_balance_distribution: %v", err)
	}
	if count != want {
		t.Errorf("address_balance_distribution row count = %d, want %d", count, want)
	}
}

func requireDistributionStateUninitialized(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var height int64
	var hash *string
	if err := pool.QueryRow(ctx, `SELECT indexed_height, indexed_block_hash FROM address_balance_distribution_state WHERE name = 'main'`).Scan(&height, &hash); err != nil {
		t.Fatalf("read address_balance_distribution_state: %v", err)
	}
	if height != -1 || hash != nil {
		t.Errorf("address_balance_distribution_state = (height=%d, hash=%v), want uninitialized (-1, NULL)", height, hash)
	}
}
