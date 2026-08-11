package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/query"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestPool mirrors internal/api's, internal/query's, and internal/web's
// dbtest_test.go pattern — duplicated per-package on purpose. This is the
// one test file allowed to exercise the ACTUAL Phase 2E.1 route
// composition (newRootHandler in main.go), since that composition itself
// lives in package main.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	baseURL := os.Getenv("QOGE_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("QOGE_TEST_DATABASE_URL not set; skipping PostgreSQL-backed test")
	}

	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random schema suffix: %v", err)
	}
	schemaName := "qoge_main_test_" + hex.EncodeToString(b)
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

	migrations, err := store.LoadMigrations(os.DirFS("../../migrations"))
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

// AE: existing internal/api JSON routes remain reachable, and return JSON
// (not HTML), under the composite root handler built by newRootHandler.
func TestRootHandler_APIRoutesStayJSON(t *testing.T) {
	pool := newTestPool(t)
	handler := newRootHandler(query.New(pool), nil)

	for _, path := range []string{
		"/api/v1/status",
		"/api/v1/blocks",
		"/api/v1/block/1",
		"/api/v1/tx/" + strings.Repeat("a", 64),
		"/api/v1/address/qSomeAddress",
		"/api/v1/address/qSomeAddress/transactions",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		ct := rec.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s: Content-Type = %q, want application/json (body=%s)", path, ct, rec.Body.String())
		}
		var js any
		if err := json.Unmarshal(rec.Body.Bytes(), &js); err != nil {
			t.Fatalf("%s: response is not valid JSON: %v (body=%s)", path, err, rec.Body.String())
		}
	}
}

// AE (continued): the HTML explorer's own routes are also reachable under
// the same composite handler and return HTML, not JSON.
func TestRootHandler_WebRoutesStayHTML(t *testing.T) {
	pool := newTestPool(t)
	handler := newRootHandler(query.New(pool), nil)

	for _, path := range []string{"/", "/blocks", "/block/1", "/search?q=1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code >= 300 && rec.Code < 400 {
			continue // /search redirects; that's expected, not a failure
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s: Content-Type = %q, want text/html", path, ct)
		}
	}
}

// AF: /healthz and /readyz behavior is unchanged by composing the web
// layer in — both are still served by internal/api, with its existing JSON
// contract, never re-implemented by internal/web.
func TestRootHandler_HealthzReadyzUnchanged(t *testing.T) {
	pool := newTestPool(t)
	handler := newRootHandler(query.New(pool), nil)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body=%s", path, rec.Code, rec.Body.String())
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s: Content-Type = %q, want application/json", path, ct)
		}
		var body struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if body.Status == "" {
			t.Fatalf("%s: empty status field", path)
		}
	}
}
