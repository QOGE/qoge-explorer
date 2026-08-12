package deployments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/mempool"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDatabaseURL returns the connection string for the disposable local
// test database from QOGE_TEST_DATABASE_URL, or skips the calling test if
// it isn't set. Mirrors internal/store/dbtest_test.go, internal/query/
// dbtest_test.go, etc. — duplicated rather than exported because this
// package must only reach into store's/mempool's public API.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("QOGE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QOGE_TEST_DATABASE_URL not set; skipping PostgreSQL-backed test")
	}
	return url
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random schema suffix: %v", err)
	}
	return hex.EncodeToString(b)
}

// newTestSchema creates a fresh, uniquely-named, empty PostgreSQL schema
// and returns a pool whose every connection has search_path pinned to it
// — never migrations/public. Cleaned up (pool closed, schema dropped) via
// t.Cleanup. Never runs DROP SCHEMA public or touches any object this
// function didn't itself create (mirrors internal/store/dbtest_test.go's
// safety rationale exactly).
func newTestSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	baseURL := testDatabaseURL(t)

	schemaName := "qoge_deployments_test_" + randomHex(t, 12)
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

func migrationsFS(t *testing.T) []store.Migration {
	t.Helper()
	migrations, err := store.LoadMigrations(os.DirFS("../../migrations"))
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	return migrations
}

// newTestStores returns a fully-migrated, isolated schema plus a write
// internal/store.Store (confirmed fixtures via ApplyBlock — never ad-hoc
// SQL), a write internal/mempool.Store (mempool fixtures via
// ReplaceSnapshot), and the deployments.Store under test, all sharing one
// pool.
func newTestStores(t *testing.T) (deploymentStore *Store, confirmedStore *store.Store, mempoolStore *mempool.Store, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool = newTestSchema(t)

	migrations := migrationsFS(t)
	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	return NewStore(pool), store.New(pool), mempool.NewStore(pool), pool
}
