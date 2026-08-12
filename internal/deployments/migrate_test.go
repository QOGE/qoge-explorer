package deployments

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/mempool"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrationRoundTrip_PreservesExistingData is spec item 7: schema
// v2 -> v3 -> v2 -> v3, with REAL pre-existing confirmed-chain, address,
// and mempool state seeded before the migration exercise. All
// pre-existing data must survive unchanged; DROP SCHEMA public and any
// destruction of unrelated tables are never used (newTestSchema's
// isolated, uniquely-named schema — see dbtest_test.go).
func TestMigrationRoundTrip_PreservesExistingData(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := migrationsFS(t)

	// Start at v2 only (every migration except 0003), seed real data,
	// THEN exercise 0003 up/down/up — proving 0003 itself round-trips
	// cleanly around genuinely pre-existing v2 data, not just an empty
	// schema.
	v2Migrations := migrations[:len(migrations)-1]
	if migrations[len(migrations)-1].Version != 3 {
		t.Fatalf("fixture assumption violated: last migration version = %d, want 3", migrations[len(migrations)-1].Version)
	}
	if _, err := store.Up(ctx, pool, v2Migrations); err != nil {
		t.Fatalf("Up to v2: %v", err)
	}

	cstore := store.New(pool)
	mstore := mempool.NewStore(pool)

	g := block("migrate-roundtrip-genesis", 0, "", coinbaseTx("migrate-roundtrip-genesis", 100_00000000, "qMigrateRoundTripAddr"))
	if err := cstore.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock genesis: %v", err)
	}
	// Genesis's coinbase output is intentionally excluded from address
	// tracking (Core's ConnectBlock skips connecting it — see
	// internal/store/apply.go's isGenesis comment), so a second block is
	// needed to actually produce an addresses row to assert against.
	b1 := block("migrate-roundtrip-block1", 1, g.Hash, coinbaseTx("migrate-roundtrip-block1", 50_00000000, "qMigrateRoundTripAddr"))
	if err := cstore.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("ApplyBlock block1: %v", err)
	}

	mtx := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("migrate-roundtrip-mempool"), 500, 1_700_000_000)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, b1.Hash, mtx)); err != nil {
		t.Fatalf("mempool ReplaceSnapshot: %v", err)
	}

	confirmedBefore := fingerprintTables(t, ctx, pool, confirmedTables)
	mempoolBefore := fingerprintTables(t, ctx, pool, mempoolTables)
	var addressCountBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM addresses`).Scan(&addressCountBefore); err != nil {
		t.Fatalf("count addresses (before): %v", err)
	}
	if addressCountBefore == 0 {
		t.Fatal("fixture bug: expected at least one address row before the migration exercise")
	}

	// v2 -> v3
	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("Up to v3: %v", err)
	}
	if !tableExistsIn(t, ctx, pool, "deployment_state") {
		t.Fatal("deployment_state table missing after migrating up to v3")
	}
	requireTablesUnchanged(t, ctx, pool, confirmedTables, confirmedBefore)
	requireTablesUnchanged(t, ctx, pool, mempoolTables, mempoolBefore)

	// v3 -> v2
	if _, err := store.Down(ctx, pool, migrations, 1); err != nil {
		t.Fatalf("Down to v2: %v", err)
	}
	if tableExistsIn(t, ctx, pool, "deployment_state") {
		t.Fatal("deployment_state table still present after rolling back to v2")
	}
	if !tableExistsIn(t, ctx, pool, "chain_deployments") {
		t.Fatal("chain_deployments table (owned by migration 0001) must survive rolling back migration 0003")
	}
	requireTablesUnchanged(t, ctx, pool, confirmedTables, confirmedBefore)
	requireTablesUnchanged(t, ctx, pool, mempoolTables, mempoolBefore)

	// v2 -> v3 again
	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("second Up to v3: %v", err)
	}
	if !tableExistsIn(t, ctx, pool, "deployment_state") {
		t.Fatal("deployment_state table missing after reapplying migration 0003")
	}
	requireTablesUnchanged(t, ctx, pool, confirmedTables, confirmedBefore)
	requireTablesUnchanged(t, ctx, pool, mempoolTables, mempoolBefore)

	var addressCountAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM addresses`).Scan(&addressCountAfter); err != nil {
		t.Fatalf("count addresses (after): %v", err)
	}
	if addressCountAfter != addressCountBefore {
		t.Errorf("address count changed across migration round trip: %d -> %d", addressCountBefore, addressCountAfter)
	}

	// deployment_state must be back to its freshly-migrated bootstrap
	// row, not some stale leftover.
	dstore := NewStore(pool)
	state, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Initialized {
		t.Error("deployment_state.initialized = true after a fresh 0003 reapplication, want false")
	}
}

type tableFP struct {
	count  int
	digest string
}

func fingerprintTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tables []string) map[string]tableFP {
	t.Helper()
	out := make(map[string]tableFP, len(tables))
	for _, table := range tables {
		var c int
		var d string
		if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(md5(string_agg(t::text, '|' ORDER BY t::text)), '') FROM `+table+` t`).Scan(&c, &d); err != nil {
			t.Fatalf("fingerprint table %s: %v", table, err)
		}
		out[table] = tableFP{c, d}
	}
	return out
}

func requireTablesUnchanged(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tables []string, before map[string]tableFP) {
	t.Helper()
	after := fingerprintTables(t, ctx, pool, tables)
	for _, table := range tables {
		if before[table] != after[table] {
			t.Errorf("table %s changed: before=%+v after=%+v", table, before[table], after[table])
		}
	}
}

func tableExistsIn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name=$1)`,
		name,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s exists: %v", name, err)
	}
	return exists
}
