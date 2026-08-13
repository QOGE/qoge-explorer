package deployments

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/mempool"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrationRoundTrip_PreservesExistingData is spec item 7: schema
// v4 -> v2 -> v4 (0003 and 0004 both roll back and reapply together — see
// the in-function comment for why), with REAL pre-existing confirmed-chain,
// address, and mempool state seeded before the migration exercise. All
// pre-existing data must survive unchanged; DROP SCHEMA public and any
// destruction of unrelated tables are never used (newTestSchema's
// isolated, uniquely-named schema — see dbtest_test.go).
func TestMigrationRoundTrip_PreservesExistingData(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := migrationsFS(t)

	// Every migration is applied up front — including 0004
	// (block_accounting), added by Phase 2H.1 — because
	// internal/store.ApplyBlock (used below to seed real confirmed-chain
	// fixtures) unconditionally writes a block_accounting row for every
	// block it applies; 0004 must exist for that to succeed at all.
	//
	// store.Down/Up always operate by strict descending/ascending VERSION
	// NUMBER against whatever is actually applied in the database, not by
	// which single migration a test cares about — once 0004 sits on top
	// of 0003, Down has no way to roll back 0003 alone while leaving 0004
	// in place (Down(1) would target 0004, the higher version number,
	// every time). This test's round trip below therefore cycles 0003 AND
	// 0004 together (both come off, then both go back on); what it
	// actually asserts — deployment_state's own presence/bootstrap state,
	// and zero mutation of confirmed/mempool data — is unaffected by 0004
	// also cycling alongside it. block_accounting itself is deliberately
	// excluded from the "must remain byte-identical" table lists below
	// for exactly this reason: its own table is dropped and recreated
	// empty as a mechanical side effect of this test toggling 0004, which
	// is not what this test exists to verify (that belongs to
	// internal/store's own migration round-trip coverage).
	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("Up (all migrations): %v", err)
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

	// v4 -> v2: roll back 0004 (block_accounting) then 0003
	// (deployment_state), newest version first — see the comment above for
	// why both must come off together.
	if _, err := store.Down(ctx, pool, migrations, 2); err != nil {
		t.Fatalf("Down to v2: %v", err)
	}
	if tableExistsIn(t, ctx, pool, "deployment_state") {
		t.Fatal("deployment_state table still present after rolling back to v2")
	}
	if tableExistsIn(t, ctx, pool, "block_accounting") {
		t.Fatal("block_accounting table still present after rolling back to v2")
	}
	if !tableExistsIn(t, ctx, pool, "chain_deployments") {
		t.Fatal("chain_deployments table (owned by migration 0001) must survive rolling back migrations 0003/0004")
	}
	requireTablesUnchanged(t, ctx, pool, confirmedTables, confirmedBefore)
	requireTablesUnchanged(t, ctx, pool, mempoolTables, mempoolBefore)

	// v2 -> v4 again: reapply both 0003 and 0004.
	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("second Up to v4: %v", err)
	}
	if !tableExistsIn(t, ctx, pool, "deployment_state") {
		t.Fatal("deployment_state table missing after reapplying migration 0003")
	}
	if !tableExistsIn(t, ctx, pool, "block_accounting") {
		t.Fatal("block_accounting table missing after reapplying migration 0004")
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
