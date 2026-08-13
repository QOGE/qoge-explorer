package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/config"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── PHASE 2H.1 FINAL BACKFILL NETWORK-SAFETY CORRECTION ──────────────────
//
// backfill-accounting previously trusted QOGE_NETWORK without verifying
// that the already-indexed PostgreSQL database actually belonged to that
// network — a real smoke test demonstrated that a regtest database
// combined with QOGE_NETWORK=main produced plausible-but-false immutable
// accounting (actual Core subsidy 50 QOGE, wrong backfill subsidy 100 QOGE,
// false 50 QOGE "unclaimed reward"). These tests exercise the ACTUAL CLI
// entry point (runBackfillAccounting), through a real PostgreSQL database,
// proving that misconfiguration is now mechanically impossible: a
// mismatched or missing genesis exits nonzero BEFORE any block_accounting
// row is written, and a correctly-configured network still backfills
// exactly as before. See store.VerifyNetworkIdentity and
// chain.ExpectedGenesisHash.

// testDBURLWithSchema creates a fresh, uniquely-named schema in the disposable
// QOGE_TEST_DATABASE_URL database, migrates it, and returns both a pool
// pinned to that schema (for seeding fixtures / assertions) and a
// standalone connection string — WITH the schema baked in via the
// "search_path" runtime parameter pgx sends in its startup packet — for
// runBackfillAccounting's own independent store.Connect call to use. This
// mirrors newTestPool's schema-isolation pattern (serve_test.go) but also
// hands back a connectable URL, since runBackfillAccounting opens its own
// pool from cfg.DatabaseURL rather than accepting an existing pool.
func testDBURLWithSchema(t *testing.T) (dbURL string, verifyPool *pgxpool.Pool) {
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
	schemaName := "qoge_backfillcli_test_" + hex.EncodeToString(b)
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

	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	q := parsed.Query()
	q.Set("search_path", schemaName)
	parsed.RawQuery = q.Encode()

	return parsed.String(), pool
}

// fixtureHash64 turns a short readable label into a syntactically valid
// 64-lowercase-hex-char hash, matching internal/store's hash64 test helper
// (invariants_test.go) — duplicated here since that helper is unexported in
// a different package.
func fixtureHash64(label string) string {
	h := hex.EncodeToString([]byte(label))
	if len(h) >= 64 {
		return h[:64]
	}
	return h + strings.Repeat("0", 64-len(h))
}

// genesisBlockWithHash builds a minimal, schema-valid height-0 block whose
// hash is exactly hash — used to simulate an already-indexed database whose
// canonical genesis either does or doesn't match a given network's expected
// genesis hash. The coinbase output value (1 satoshi) is deliberately far
// under any network's actual era-0 subsidy: an underclaimed coinbase is
// always valid chain state (docs/ARCHITECTURE.md §26), so this fixture
// never trips ErrCoinbaseOverclaim regardless of which network's schedule a
// Store happens to be bound to.
func genesisBlockWithHash(hash string) chain.Block {
	coinbaseTxid := fixtureHash64("cliGenesisTx")
	return chain.Block{
		Hash:         hash,
		Height:       0,
		PreviousHash: "",
		MerkleRoot:   coinbaseTxid,
		Time:         1630500000,
		Bits:         "1d00ffff",
		Difficulty:   1.0,
		Nonce:        1,
		Size:         100,
		Weight:       400,
		TxCount:      1,
		Transactions: []chain.Transaction{
			{
				TxID:       coinbaseTxid,
				WTxID:      coinbaseTxid,
				Version:    1,
				LockTime:   0,
				Size:       100,
				VSize:      100,
				Weight:     400,
				IsCoinbase: true,
				Inputs: []chain.Input{
					{Index: 0, Coinbase: []byte{0x51}, Sequence: 4294967295},
				},
				Outputs: []chain.Output{
					{Index: 0, Value: chain.Amount(1), ScriptPubKey: []byte{0x00}, ScriptType: script.TypeP2PKH, Address: "qGenesisCLI"},
				},
			},
		},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func accountingRowCount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	completeness, err := store.CheckAccountingCompleteness(context.Background(), pool)
	if err != nil {
		t.Fatalf("CheckAccountingCompleteness: %v", err)
	}
	return completeness.AccountingCount
}

// TestRunBackfillAccounting_WrongNetworkRejectedBeforeWrites reproduces the
// exact previously-demonstrated misconfiguration — a regtest-genesis
// database with QOGE_NETWORK=main — and requires the CLI to exit nonzero
// with ZERO block_accounting rows written, rather than silently computing
// mainnet-schedule accounting against regtest data.
func TestRunBackfillAccounting_WrongNetworkRejectedBeforeWrites(t *testing.T) {
	dbURL, pool := testDBURLWithSchema(t)
	ctx := context.Background()

	regtestGenesis, err := chain.ExpectedGenesisHash("regtest")
	if err != nil {
		t.Fatalf("ExpectedGenesisHash(regtest): %v", err)
	}
	seedStore, err := store.NewForNetwork(pool, "regtest")
	if err != nil {
		t.Fatalf("NewForNetwork(regtest): %v", err)
	}
	if err := seedStore.ApplyBlock(ctx, genesisBlockWithHash(regtestGenesis)); err != nil {
		t.Fatalf("seed regtest genesis: %v", err)
	}
	if got := accountingRowCount(t, pool); got != 1 {
		t.Fatalf("accounting rows after seeding = %d, want 1 (live ApplyBlock always writes accounting)", got)
	}
	// Simulate the legacy pre-0004 scenario backfill-accounting exists for:
	// blocks indexed, but block_accounting not yet populated.
	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting`); err != nil {
		t.Fatalf("clear accounting rows: %v", err)
	}
	if got := accountingRowCount(t, pool); got != 0 {
		t.Fatalf("accounting rows after clearing = %d, want 0", got)
	}

	cfg := config.Config{DatabaseURL: dbURL, Network: "main"}
	code := runBackfillAccounting(cfg, discardLogger())
	if code == 0 {
		t.Fatal("runBackfillAccounting(regtest DB, QOGE_NETWORK=main) exit code = 0, want nonzero")
	}
	if got := accountingRowCount(t, pool); got != 0 {
		t.Fatalf("accounting rows after rejected wrong-network run = %d, want 0 (no write must occur before the genesis preflight passes)", got)
	}
}

// TestRunBackfillAccounting_CorrectNetworkSucceeds proves the SAME regtest
// database backfills correctly once QOGE_NETWORK actually matches what was
// indexed: the preflight passes, and every block gets its accounting row.
func TestRunBackfillAccounting_CorrectNetworkSucceeds(t *testing.T) {
	dbURL, pool := testDBURLWithSchema(t)
	ctx := context.Background()

	regtestGenesis, err := chain.ExpectedGenesisHash("regtest")
	if err != nil {
		t.Fatalf("ExpectedGenesisHash(regtest): %v", err)
	}
	seedStore, err := store.NewForNetwork(pool, "regtest")
	if err != nil {
		t.Fatalf("NewForNetwork(regtest): %v", err)
	}
	if err := seedStore.ApplyBlock(ctx, genesisBlockWithHash(regtestGenesis)); err != nil {
		t.Fatalf("seed regtest genesis: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting`); err != nil {
		t.Fatalf("clear accounting rows: %v", err)
	}

	cfg := config.Config{DatabaseURL: dbURL, Network: "regtest"}
	code := runBackfillAccounting(cfg, discardLogger())
	if code != 0 {
		t.Fatalf("runBackfillAccounting(regtest DB, QOGE_NETWORK=regtest) exit code = %d, want 0", code)
	}
	if got := accountingRowCount(t, pool); got != 1 {
		t.Fatalf("accounting rows after correct-network backfill = %d, want 1", got)
	}
}

// TestRunBackfillAccounting_MainnetGenesisRejectsOtherNetworks seeds a
// mainnet-genesis database and requires QOGE_NETWORK=main to succeed while
// QOGE_NETWORK=test and QOGE_NETWORK=regtest are both rejected before any
// write.
func TestRunBackfillAccounting_MainnetGenesisRejectsOtherNetworks(t *testing.T) {
	mainGenesis, err := chain.ExpectedGenesisHash("main")
	if err != nil {
		t.Fatalf("ExpectedGenesisHash(main): %v", err)
	}

	for _, tc := range []struct {
		network   string
		wantCode0 bool
	}{
		{"main", true},
		{"test", false},
		{"regtest", false},
	} {
		t.Run(tc.network, func(t *testing.T) {
			dbURL, pool := testDBURLWithSchema(t)
			ctx := context.Background()

			seedStore, err := store.NewForNetwork(pool, "main")
			if err != nil {
				t.Fatalf("NewForNetwork(main): %v", err)
			}
			if err := seedStore.ApplyBlock(ctx, genesisBlockWithHash(mainGenesis)); err != nil {
				t.Fatalf("seed main genesis: %v", err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM block_accounting`); err != nil {
				t.Fatalf("clear accounting rows: %v", err)
			}

			cfg := config.Config{DatabaseURL: dbURL, Network: tc.network}
			code := runBackfillAccounting(cfg, discardLogger())
			if tc.wantCode0 && code != 0 {
				t.Fatalf("QOGE_NETWORK=%s against mainnet-genesis DB: exit code = %d, want 0", tc.network, code)
			}
			if !tc.wantCode0 && code == 0 {
				t.Fatalf("QOGE_NETWORK=%s against mainnet-genesis DB: exit code = 0, want nonzero", tc.network)
			}
			gotRows := accountingRowCount(t, pool)
			if tc.wantCode0 && gotRows != 1 {
				t.Fatalf("QOGE_NETWORK=%s: accounting rows = %d, want 1", tc.network, gotRows)
			}
			if !tc.wantCode0 && gotRows != 0 {
				t.Fatalf("QOGE_NETWORK=%s: accounting rows = %d, want 0 (rejected before any write)", tc.network, gotRows)
			}
		})
	}
}

// TestRunBackfillAccounting_TestnetGenesisRejectsOtherNetworks mirrors
// TestRunBackfillAccounting_MainnetGenesisRejectsOtherNetworks for a
// testnet-genesis database: QOGE_NETWORK=test succeeds; main and regtest
// are both rejected before any write.
func TestRunBackfillAccounting_TestnetGenesisRejectsOtherNetworks(t *testing.T) {
	testGenesis, err := chain.ExpectedGenesisHash("test")
	if err != nil {
		t.Fatalf("ExpectedGenesisHash(test): %v", err)
	}

	for _, tc := range []struct {
		network   string
		wantCode0 bool
	}{
		{"test", true},
		{"main", false},
		{"regtest", false},
	} {
		t.Run(tc.network, func(t *testing.T) {
			dbURL, pool := testDBURLWithSchema(t)
			ctx := context.Background()

			seedStore, err := store.NewForNetwork(pool, "test")
			if err != nil {
				t.Fatalf("NewForNetwork(test): %v", err)
			}
			if err := seedStore.ApplyBlock(ctx, genesisBlockWithHash(testGenesis)); err != nil {
				t.Fatalf("seed testnet genesis: %v", err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM block_accounting`); err != nil {
				t.Fatalf("clear accounting rows: %v", err)
			}

			cfg := config.Config{DatabaseURL: dbURL, Network: tc.network}
			code := runBackfillAccounting(cfg, discardLogger())
			if tc.wantCode0 && code != 0 {
				t.Fatalf("QOGE_NETWORK=%s against testnet-genesis DB: exit code = %d, want 0", tc.network, code)
			}
			if !tc.wantCode0 && code == 0 {
				t.Fatalf("QOGE_NETWORK=%s against testnet-genesis DB: exit code = 0, want nonzero", tc.network)
			}
			gotRows := accountingRowCount(t, pool)
			if tc.wantCode0 && gotRows != 1 {
				t.Fatalf("QOGE_NETWORK=%s: accounting rows = %d, want 1", tc.network, gotRows)
			}
			if !tc.wantCode0 && gotRows != 0 {
				t.Fatalf("QOGE_NETWORK=%s: accounting rows = %d, want 0 (rejected before any write)", tc.network, gotRows)
			}
		})
	}
}

// TestRunBackfillAccounting_MissingGenesisFailsClosed proves an
// already-migrated but never-indexed database (no blocks at all) is
// rejected rather than trusting QOGE_NETWORK alone — see
// store.ErrGenesisMissing.
func TestRunBackfillAccounting_MissingGenesisFailsClosed(t *testing.T) {
	dbURL, pool := testDBURLWithSchema(t)

	cfg := config.Config{DatabaseURL: dbURL, Network: "main"}
	code := runBackfillAccounting(cfg, discardLogger())
	if code == 0 {
		t.Fatal("runBackfillAccounting against a genesis-less database: exit code = 0, want nonzero")
	}
	if got := accountingRowCount(t, pool); got != 0 {
		t.Fatalf("accounting rows after missing-genesis rejection = %d, want 0", got)
	}
}
