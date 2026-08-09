package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDatabaseURL returns the connection string for the disposable local
// test database from QOGE_TEST_DATABASE_URL, or skips the calling test if
// it isn't set — these tests need a real PostgreSQL instance (task item
// 11: "tests that can run against a disposable local test database") and
// must not fail a `go test ./...` run in an environment without one.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("QOGE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QOGE_TEST_DATABASE_URL not set; skipping PostgreSQL-backed test")
	}
	return url
}

// resetSchema drops and recreates the public schema, giving each caller a
// guaranteed-empty database regardless of what earlier test runs left
// behind.
func resetSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}

// migratedPool connects to the test database, resets it to empty, applies
// every migration, and returns the pool with a Cleanup that closes it. Used
// by tests that exercise schema invariants rather than migration mechanics
// themselves.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pool, err := Connect(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	resetSchema(t, pool)

	migrations, err := LoadMigrations(os.DirFS(migrationsDirForTests(t)))
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := Up(ctx, pool, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

// migrationsDirForTests locates the repository's top-level migrations/
// directory from this test file's package directory
// (internal/store -> ../../migrations).
func migrationsDirForTests(t *testing.T) string {
	t.Helper()
	return "../../migrations"
}
