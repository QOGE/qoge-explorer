package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func loadTestMigrations(t *testing.T) []Migration {
	t.Helper()
	migrations, err := LoadMigrations(os.DirFS(migrationsDirForTests(t)))
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("expected at least one migration to be loaded")
	}
	return migrations
}

// tableExists checks for a table in whatever schema pool's connections
// currently resolve unqualified names against (current_schema()) — i.e.
// each test's own isolated schema (see newTestSchema), never a hardcoded
// 'public'.
func tableExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
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

// TestMigrations_ApplyToEmptyDB is invariant test A: migrations apply
// cleanly to an empty database.
func TestMigrations_ApplyToEmptyDB(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	applied, err := Up(ctx, pool, migrations)
	if err != nil {
		t.Fatalf("Up on empty DB: %v", err)
	}
	if len(applied) != len(migrations) {
		t.Errorf("applied %d migrations, want %d", len(applied), len(migrations))
	}

	for _, table := range []string{
		"sync_state", "blocks", "transactions", "block_transactions",
		"transaction_inputs", "transaction_input_witness", "transaction_outputs",
		"output_addresses", "output_participants", "utxo_state", "addresses",
		"chain_deployments",
	} {
		if !tableExists(t, ctx, pool, table) {
			t.Errorf("expected table %s to exist after migrating up", table)
		}
	}

	version, err := CurrentVersion(ctx, pool)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if version != migrations[len(migrations)-1].Version {
		t.Errorf("CurrentVersion = %d, want %d", version, migrations[len(migrations)-1].Version)
	}
}

// TestMigrations_RollbackCleanly is invariant test B: migrations roll back
// cleanly where supported.
func TestMigrations_RollbackCleanly(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	if _, err := Up(ctx, pool, migrations); err != nil {
		t.Fatalf("Up: %v", err)
	}

	rolledBack, err := Down(ctx, pool, migrations, len(migrations))
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if len(rolledBack) != len(migrations) {
		t.Errorf("rolled back %d migrations, want %d", len(rolledBack), len(migrations))
	}

	for _, table := range []string{"blocks", "transactions", "transaction_outputs", "utxo_state"} {
		if tableExists(t, ctx, pool, table) {
			t.Errorf("expected table %s to be gone after rolling back every migration", table)
		}
	}

	version, err := CurrentVersion(ctx, pool)
	if err != nil {
		t.Fatalf("CurrentVersion after full rollback: %v", err)
	}
	if version != 0 {
		t.Errorf("CurrentVersion after full rollback = %d, want 0", version)
	}
}

// TestMigrations_ReapplyAfterRollback is invariant test C: migrations can
// be reapplied after rollback.
func TestMigrations_ReapplyAfterRollback(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	if _, err := Up(ctx, pool, migrations); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if _, err := Down(ctx, pool, migrations, len(migrations)); err != nil {
		t.Fatalf("Down: %v", err)
	}
	applied, err := Up(ctx, pool, migrations)
	if err != nil {
		t.Fatalf("second Up (reapply): %v", err)
	}
	if len(applied) != len(migrations) {
		t.Errorf("reapply: applied %d migrations, want %d", len(applied), len(migrations))
	}

	if !tableExists(t, ctx, pool, "blocks") {
		t.Error("expected blocks table to exist after reapplying migrations")
	}

	// The reapplied schema must be fully functional, not just present:
	// confirm the sync_state bootstrap row from the migration's INSERT
	// exists exactly once (proves the up migration's data statements, not
	// just its DDL, ran again cleanly).
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sync_state").Scan(&count); err != nil {
		t.Fatalf("count sync_state rows: %v", err)
	}
	if count != 1 {
		t.Errorf("sync_state row count after reapply = %d, want 1", count)
	}
}

// TestLoadMigrations_RequiresBothDirections confirms LoadMigrations refuses
// a migration that has an .up.sql but no matching .down.sql, per the "every
// migration must be reversible" requirement (task item 10/11).
func TestLoadMigrations_RequiresBothDirections(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/0001_only_up.up.sql", []byte("CREATE TABLE t (id int);"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadMigrations(os.DirFS(dir))
	if err == nil {
		t.Fatal("expected an error for a migration missing its .down.sql, got nil")
	}
}

// TestMigrations_ChecksumDriftDetected proves the task-5 scenario directly:
// apply a migration, then simulate someone editing the already-applied
// .up.sql file afterward (by mutating the in-memory UpSQL of an otherwise
// identically-versioned/named Migration), and confirm both VerifyChecksums
// and Up itself reject it loudly rather than silently treating the
// (already-recorded) version as still up to date.
func TestMigrations_ChecksumDriftDetected(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	if _, err := Up(ctx, pool, migrations); err != nil {
		t.Fatalf("initial Up: %v", err)
	}

	drifted := make([]Migration, len(migrations))
	copy(drifted, migrations)
	drifted[0].UpSQL = drifted[0].UpSQL + "\n-- edited after being applied\n"

	if err := VerifyChecksums(ctx, pool, drifted); err == nil {
		t.Fatal("expected VerifyChecksums to reject a migration whose UpSQL changed after being applied, got nil")
	} else {
		t.Logf("VerifyChecksums correctly rejected drift: %v", err)
	}

	// Up() itself must refuse to proceed too — not just the standalone
	// checker — even though every migration is already applied and Up
	// would otherwise have nothing to do.
	if _, err := Up(ctx, pool, drifted); err == nil {
		t.Fatal("expected Up to reject checksum drift before doing anything, got nil")
	}

	// Down() must refuse for the same reason: its DownSQL might no longer
	// be the correct inverse of whatever is actually applied.
	if _, err := Down(ctx, pool, drifted, 1); err == nil {
		t.Fatal("expected Down to reject checksum drift before rolling anything back, got nil")
	}
}

// TestMigrations_NameDriftDetected covers the same-version/different-name
// case explicitly.
func TestMigrations_NameDriftDetected(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	if _, err := Up(ctx, pool, migrations); err != nil {
		t.Fatalf("initial Up: %v", err)
	}

	renamed := make([]Migration, len(migrations))
	copy(renamed, migrations)
	renamed[0].Name = renamed[0].Name + "_renamed"

	if err := VerifyChecksums(ctx, pool, renamed); err == nil {
		t.Fatal("expected VerifyChecksums to reject a migration renamed after being applied, got nil")
	}
}

// TestMigrations_DownSQLDriftDetected covers the DownSQL half of checksum
// drift protection explicitly: UpSQL unchanged, only DownSQL edited after
// the migration was applied. A future rollback would otherwise silently
// run something that's no longer the correct inverse of what was actually
// applied.
func TestMigrations_DownSQLDriftDetected(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	if _, err := Up(ctx, pool, migrations); err != nil {
		t.Fatalf("initial Up: %v", err)
	}

	drifted := make([]Migration, len(migrations))
	copy(drifted, migrations)
	drifted[0].DownSQL = drifted[0].DownSQL + "\n-- edited after being applied\n"

	if err := VerifyChecksums(ctx, pool, drifted); err == nil {
		t.Fatal("expected VerifyChecksums to reject a migration whose DownSQL changed after being applied, got nil")
	} else {
		t.Logf("VerifyChecksums correctly rejected DownSQL drift: %v", err)
	}

	if _, err := Down(ctx, pool, drifted, 1); err == nil {
		t.Fatal("expected Down to reject DownSQL checksum drift before rolling anything back, got nil")
	}
	if _, err := Up(ctx, pool, drifted); err == nil {
		t.Fatal("expected Up to reject DownSQL checksum drift too (it verifies both directions), got nil")
	}
}

// TestMigrations_AppliedMigrationMissingLocalDefinitionRejected covers the
// "migration history is append-only" requirement: a version recorded as
// applied in the database, but with no corresponding entry in the loaded
// migrations slice (as if its .sql files had been deleted from the
// repository), must be rejected rather than silently ignored.
func TestMigrations_AppliedMigrationMissingLocalDefinitionRejected(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := loadTestMigrations(t)

	if _, err := Up(ctx, pool, migrations); err != nil {
		t.Fatalf("initial Up: %v", err)
	}

	// Simulate deleting migration 0001's .sql files from the repository:
	// load an empty migrations slice against a database that still has it
	// recorded as applied.
	if err := VerifyChecksums(ctx, pool, nil); err == nil {
		t.Fatal("expected VerifyChecksums to reject an applied migration with no local definition, got nil")
	}
	if _, err := Up(ctx, pool, nil); err == nil {
		t.Fatal("expected Up to reject an applied migration with no local definition, got nil")
	}
	if _, err := Down(ctx, pool, nil, 1); err == nil {
		t.Fatal("expected Down to reject an applied migration with no local definition, got nil")
	}
}
