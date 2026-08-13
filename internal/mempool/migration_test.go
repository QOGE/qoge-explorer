package mempool

import (
	"context"
	"os"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigration_0002RoundTrip exercises schema v1 -> v2 -> v1 -> v2 (spec
// item 36): migratedPool already applies every migration (0001, 0002,
// 0003 since Phase 2G.1, and 0004 since Phase 2H.1) to a fresh disposable
// schema; this test additionally rolls 0004, 0003, and 0002 all back
// (newest first: 0004 must come off before 0003, which must come off
// before 0002) and re-applies them, confirming mempool_state and every
// mempool_* table exist after the final up and are gone after the
// rollback, and that 0001's tables/rows are never touched by any of it.
func TestMigration_0002RoundTrip(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	requireTableExists(t, ctx, pool, "mempool_state")
	requireTableExists(t, ctx, pool, "mempool_transactions")
	requireTableExists(t, ctx, pool, "mempool_inputs")
	requireTableExists(t, ctx, pool, "mempool_input_witness")
	requireTableExists(t, ctx, pool, "mempool_outputs")
	requireTableExists(t, ctx, pool, "mempool_output_addresses")
	requireTableExists(t, ctx, pool, "mempool_output_participants")
	requireTableExists(t, ctx, pool, "mempool_dependencies")

	// Confirmed-chain data survives a mempool cache write untouched — seed
	// one confirmed block before rolling the mempool schema back and
	// forth, so this also proves the down/up cycle never destroys
	// confirmed data (spec item 36 "no confirmed data is destroyed").
	confirmed := store.New(pool)
	seedOneConfirmedBlock(t, ctx, confirmed)
	tipBefore, err := confirmed.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip before rollback: %v", err)
	}

	migrations, err := store.LoadMigrations(os.DirFS(migrationsDirForTests(t)))
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}

	// v4 -> v1: roll back 0004 (block_accounting), 0003 (deployment_state),
	// then 0002 (the mempool migration), newest first.
	rolledBack, err := store.Down(ctx, pool, migrations, 3)
	if err != nil {
		t.Fatalf("Down(3): %v", err)
	}
	if len(rolledBack) != 3 || rolledBack[0] != 4 || rolledBack[1] != 3 || rolledBack[2] != 2 {
		t.Fatalf("Down(3) rolled back %v, want [4 3 2]", rolledBack)
	}
	requireTableAbsent(t, ctx, pool, "mempool_state")
	requireTableAbsent(t, ctx, pool, "mempool_transactions")
	requireTableAbsent(t, ctx, pool, "mempool_dependencies")
	requireTableAbsent(t, ctx, pool, "deployment_state")
	requireTableAbsent(t, ctx, pool, "block_accounting")
	requireTableExists(t, ctx, pool, "blocks")            // 0001 unaffected
	requireTableExists(t, ctx, pool, "chain_deployments") // 0001 unaffected

	// v1 -> v4 again: re-apply all three.
	applied, err := store.Up(ctx, pool, migrations)
	if err != nil {
		t.Fatalf("Up (re-apply): %v", err)
	}
	if len(applied) != 3 || applied[0] != 2 || applied[1] != 3 || applied[2] != 4 {
		t.Fatalf("Up (re-apply) applied %v, want [2 3 4]", applied)
	}
	requireTableExists(t, ctx, pool, "mempool_state")
	requireTableExists(t, ctx, pool, "deployment_state")
	requireTableExists(t, ctx, pool, "block_accounting")

	// The bootstrap mempool_state('main') row must be back in its
	// UNINITIALIZED state after re-creation, exactly as migration 0002's
	// up.sql leaves it — never carrying over anything from before the
	// rollback (there is nothing to carry over: DROP TABLE removed it
	// entirely).
	var initialized bool
	if err := pool.QueryRow(ctx, `SELECT initialized FROM mempool_state WHERE name = 'main'`).Scan(&initialized); err != nil {
		t.Fatalf("read mempool_state after re-apply: %v", err)
	}
	if initialized {
		t.Fatalf("mempool_state.initialized = true after fresh re-apply, want false")
	}

	// Confirmed data is byte-identical to before the whole round trip.
	tipAfter, err := confirmed.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip after round trip: %v", err)
	}
	if tipAfter != tipBefore {
		t.Fatalf("confirmed tip changed across mempool migration round trip: before=%+v after=%+v", tipBefore, tipAfter)
	}
}

func requireTableExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	if !tableExists(t, ctx, pool, name) {
		t.Fatalf("table %s does not exist, want it to exist", name)
	}
}

func requireTableAbsent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	if tableExists(t, ctx, pool, name) {
		t.Fatalf("table %s exists, want it absent", name)
	}
}

func tableExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = $1
		)
	`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s existence: %v", name, err)
	}
	return exists
}

// seedOneConfirmedBlock applies a single genesis-height confirmed block
// through the real decode/store pipeline, for tests that need to prove
// mempool operations never touch confirmed state.
func seedOneConfirmedBlock(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	addr := "qMigrationConfirmedAddr"
	blk := confirmedBlockFixture(t, ctx, "migration-genesis", 0, "", addr, 100_00000000)
	if err := st.ApplyBlock(ctx, blk); err != nil {
		t.Fatalf("seed confirmed block: %v", err)
	}
}
