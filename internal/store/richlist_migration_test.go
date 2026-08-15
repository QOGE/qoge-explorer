package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// indexExists checks for an index in whatever schema pool's connections
// currently resolve unqualified names against (current_schema()) — mirrors
// tableExists exactly, just against pg_indexes instead of
// information_schema.tables.
func indexExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = current_schema() AND indexname=$1)`,
		name,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check index %s exists: %v", name, err)
	}
	return exists
}

// TestMigrations_RichListIndexRoundTrip is Phase 2H.3 spec item 42: 0005 ->
// 0006 -> 0005 -> 0006, proving migration 0006 creates ONLY the rich-list
// index and its down migration removes ONLY that index, leaving existing
// data (the addresses table and its rows) completely untouched.
func TestMigrations_RichListIndexRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	idx0005 := -1
	idx0006 := -1
	for i, m := range migrations {
		switch m.Version {
		case 5:
			idx0005 = i
		case 6:
			idx0006 = i
		}
	}
	if idx0005 == -1 {
		t.Fatal("migration 0005 not found among loaded migrations")
	}
	if idx0006 == -1 {
		t.Fatal("migration 0006 not found among loaded migrations")
	}
	through0005 := migrations[:idx0005+1]
	through0006 := migrations[:idx0006+1]

	if _, err := Up(ctx, pool, through0005); err != nil {
		t.Fatalf("Up through 0005: %v", err)
	}
	if !tableExists(t, ctx, pool, "addresses") {
		t.Fatal("expected addresses table to exist after 0005")
	}
	if indexExists(t, ctx, pool, "addresses_richlist_positive_balance_idx") {
		t.Fatal("richlist index must not exist before 0006 is applied")
	}

	addr := "qRichlistMigrationTestAddr"
	if _, err := pool.Exec(ctx, `
		INSERT INTO addresses (address, total_received_satoshis, total_sent_satoshis, balance_satoshis, tx_count)
		VALUES ($1, 100, 0, 100, 1)
	`, addr); err != nil {
		t.Fatalf("seed addresses row: %v", err)
	}

	// 0005 -> 0006
	if _, err := Up(ctx, pool, through0006); err != nil {
		t.Fatalf("Up to 0006: %v", err)
	}
	if !indexExists(t, ctx, pool, "addresses_richlist_positive_balance_idx") {
		t.Fatal("expected richlist index to exist after 0006")
	}
	requireCount(t, ctx, pool, "addresses", addr, 1)

	// 0006 -> 0005 (down one step)
	if _, err := Down(ctx, pool, through0006, 1); err != nil {
		t.Fatalf("Down 1 (0006): %v", err)
	}
	if indexExists(t, ctx, pool, "addresses_richlist_positive_balance_idx") {
		t.Error("richlist index must be gone after rolling back 0006")
	}
	if !tableExists(t, ctx, pool, "addresses") {
		t.Fatal("addresses table must survive rolling back 0006 (0006's down must remove ONLY the index)")
	}
	requireCount(t, ctx, pool, "addresses", addr, 1)

	// 0005 -> 0006 again
	if _, err := Up(ctx, pool, through0006); err != nil {
		t.Fatalf("re-Up to 0006: %v", err)
	}
	if !indexExists(t, ctx, pool, "addresses_richlist_positive_balance_idx") {
		t.Fatal("expected richlist index to exist after re-applying 0006")
	}
	requireCount(t, ctx, pool, "addresses", addr, 1)
}
