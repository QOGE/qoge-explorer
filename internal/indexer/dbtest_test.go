package indexer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDatabaseURL returns the connection string for the disposable local
// test database from QOGE_TEST_DATABASE_URL, or skips the calling test if
// it isn't set — these tests need a real PostgreSQL instance and must not
// fail a `go test ./...` run in an environment without one. Mirrors
// internal/store/dbtest_test.go's helper of the same name; duplicated here
// (rather than exported from internal/store) because indexer must not
// reach into store's SQL/test internals — only its public API.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("QOGE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QOGE_TEST_DATABASE_URL not set; skipping PostgreSQL-backed test")
	}
	return url
}

// newTestSchema creates a uniquely-named schema in the test database and
// returns a pool pinned to it, plus automatic teardown — see
// internal/store/dbtest_test.go for the identical rationale (never touches
// `public` or any pre-existing schema/object).
func newTestSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	baseURL := testDatabaseURL(t)

	schemaName := "qoge_idx_test_" + randomHex(t, 12)
	identifier := pgx.Identifier{schemaName}.Sanitize()

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

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random schema suffix: %v", err)
	}
	return hex.EncodeToString(b)
}

// newTestStore returns a fully-migrated, isolated Store for one test, plus
// the underlying pool for tests that need to assert on rows directly
// (e.g. confirming a rejected block was never persisted, or a rolled-back
// block's audit-history row still exists).
func newTestStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := newTestSchema(t)

	migrations, err := store.LoadMigrations(os.DirFS("../../migrations"))
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return store.New(pool), pool
}
