package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
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

// newTestSchema creates a uniquely-named schema in the test database (never
// `public`, never any pre-existing schema) and returns a pool whose every
// connection has search_path pinned to that schema, plus a Cleanup that
// drops ONLY that generated schema.
//
// This exists specifically so a misconfigured QOGE_TEST_DATABASE_URL
// pointing at a real, populated database cannot be destroyed by the test
// suite: nothing here ever runs `DROP SCHEMA public` or touches any object
// this function didn't itself create. Worst case on a misconfigured URL is
// an extra, harmless, uniquely-named empty schema.
func newTestSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	baseURL := testDatabaseURL(t)

	schemaName := "qoge_test_" + randomHex(t, 12)
	identifier := pgx.Identifier{schemaName}.Sanitize()

	// Create the schema via a single throwaway connection, before the pool
	// (whose every connection needs search_path pointed at a schema that
	// must already exist) is constructed.
	setupConn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect to create test schema: %v", err)
	}
	_, err = setupConn.Exec(ctx, "CREATE SCHEMA "+identifier)
	closeErr := setupConn.Close(ctx)
	if err != nil {
		t.Fatalf("create test schema %s: %v", schemaName, err)
	}
	if closeErr != nil {
		t.Fatalf("close schema-setup connection: %v", closeErr)
	}

	poolCfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	// Every physical connection the pool ever opens — not just the first —
	// gets search_path pinned to this test's dedicated schema, so
	// unqualified DDL/DML in migrations and test fixtures always lands
	// there rather than in `public`.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+identifier)
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("open pool for test schema %s: %v", schemaName, err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropCtx := context.Background()
		dropConn, err := pgx.Connect(dropCtx, baseURL)
		if err != nil {
			t.Logf("warning: could not connect to drop test schema %s: %v", schemaName, err)
			return
		}
		defer dropConn.Close(dropCtx)
		if _, err := dropConn.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Logf("warning: could not drop test schema %s: %v", schemaName, err)
		}
	})

	return pool
}

// randomHex returns n random bytes hex-encoded, used to make each test
// schema's name unique even across concurrent test binaries on the same
// database.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random schema suffix: %v", err)
	}
	return hex.EncodeToString(b)
}

// migratedPool creates a fresh, isolated test schema and applies every
// migration to it. Used by tests that exercise schema invariants rather
// than migration mechanics themselves.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pool := newTestSchema(t)

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
