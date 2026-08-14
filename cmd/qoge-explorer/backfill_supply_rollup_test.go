package main

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/config"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── PHASE 2H.2a: backfill-supply-rollup network-safety preflight ────────
//
// Mirrors backfill-accounting's wrong-network tests exactly (task item
// 52): runBackfillSupplyRollup must reject a QOGE_NETWORK that doesn't
// match the already-indexed database's canonical genesis BEFORE writing
// any block_supply_rollup row, reusing the same store.VerifyNetworkIdentity
// preflight backfill-accounting itself uses.

func supplyRollupRowCount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM block_supply_rollup`).Scan(&count); err != nil {
		t.Fatalf("count block_supply_rollup: %v", err)
	}
	return count
}

// TestRunBackfillSupplyRollup_WrongNetworkRejectedBeforeWrites reproduces
// the same regtest-genesis-database + QOGE_NETWORK=main misconfiguration
// backfill-accounting's tests exercise, for backfill-supply-rollup: the CLI
// must exit nonzero with ZERO block_supply_rollup rows written.
func TestRunBackfillSupplyRollup_WrongNetworkRejectedBeforeWrites(t *testing.T) {
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
	// Simulate the legacy pre-0005 scenario: blocks/accounting indexed
	// (live ApplyBlock always writes both), but block_supply_rollup cleared
	// to prove backfill-supply-rollup itself is what's being gated, not
	// merely finding nothing to do.
	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup`); err != nil {
		t.Fatalf("clear rollup rows: %v", err)
	}

	cfg := config.Config{DatabaseURL: dbURL, Network: "main"}
	code := runBackfillSupplyRollup(cfg, discardLogger())
	if code == 0 {
		t.Fatal("runBackfillSupplyRollup(regtest DB, QOGE_NETWORK=main) exit code = 0, want nonzero")
	}
	if got := supplyRollupRowCount(t, pool); got != 0 {
		t.Fatalf("block_supply_rollup rows after rejected wrong-network run = %d, want 0 (no write must occur before the genesis preflight passes)", got)
	}
}

// TestRunBackfillSupplyRollup_CorrectNetworkSucceeds proves the SAME
// regtest database backfills correctly once QOGE_NETWORK actually matches
// what was indexed.
func TestRunBackfillSupplyRollup_CorrectNetworkSucceeds(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup`); err != nil {
		t.Fatalf("clear rollup rows: %v", err)
	}

	cfg := config.Config{DatabaseURL: dbURL, Network: "regtest"}
	code := runBackfillSupplyRollup(cfg, discardLogger())
	if code != 0 {
		t.Fatalf("runBackfillSupplyRollup(regtest DB, QOGE_NETWORK=regtest) exit code = %d, want 0", code)
	}
	if got := supplyRollupRowCount(t, pool); got != 1 {
		t.Fatalf("block_supply_rollup rows after correct-network backfill = %d, want 1", got)
	}
}
