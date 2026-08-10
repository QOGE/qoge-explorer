package indexer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/QOGE/qoge-explorer/internal/config"
	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// This file is entirely opt-in: it exercises a real, currently-running
// Qogecoin Core node over live RPC (task items 23-24). Every test here
// calls requireIndexerIntegration first, which skips unless
// QOGE_INDEXER_INTEGRATION is set — a normal `go test ./...` run never
// needs a running qogecoind, RPC credentials, or network access.
//
// Neither test here ever attempts to index the real chain's full ~2.4M
// blocks: TestLiveIndexer_CappedHistoricalSync caps GetBlockCount to a
// small height via cappedRPCClient (test-only; production code never caps
// Core's reported height), and TestLiveIndexer_CurrentTipFetchOnly fetches
// and decodes the current tip WITHOUT applying it to any database.

const integrationCapHeight = 10

// cappedRPCClient forwards GetBlockHash/GetBlockVerbose2 straight to a
// real *rpc.Client, but reports at most cap for GetBlockCount — the only
// way this suite can safely run the REAL Indexer against a REAL node
// without attempting a multi-million-block historical sync.
type cappedRPCClient struct {
	client *rpc.Client
	cap    int64
}

func (c *cappedRPCClient) GetBlockCount(ctx context.Context) (int64, error) {
	real, err := c.client.GetBlockCount(ctx)
	if err != nil {
		return 0, err
	}
	if real > c.cap {
		return c.cap, nil
	}
	return real, nil
}

func (c *cappedRPCClient) GetBlockHash(ctx context.Context, height int64) (string, error) {
	return c.client.GetBlockHash(ctx, height)
}

func (c *cappedRPCClient) GetBlockVerbose2(ctx context.Context, hash string) (rpc.RawBlock, error) {
	return c.client.GetBlockVerbose2(ctx, hash)
}

var _ RPCClient = (*cappedRPCClient)(nil)

// requireIndexerIntegration mirrors internal/decode/integration_test.go's
// requireRPCIntegration: connection parameters come from the same
// QOGE_RPC_* environment variables the rest of this codebase uses, and the
// RPC username/password/Authorization header are never printed — only
// config.RPCConfig.Redacted() (password replaced with a fixed placeholder)
// is ever logged.
func requireIndexerIntegration(t *testing.T) *rpc.Client {
	t.Helper()
	if os.Getenv("QOGE_INDEXER_INTEGRATION") == "" {
		t.Skip("QOGE_INDEXER_INTEGRATION not set; skipping live Qogecoin Core indexer integration test")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfg.RPC.Validate(); err != nil {
		t.Fatalf("RPC config: %v", err)
	}
	t.Logf("connecting to live node: %s", cfg.RPC.Redacted())
	return rpc.New(rpc.Config{
		Host:     cfg.RPC.Host,
		Port:     cfg.RPC.Port,
		User:     cfg.RPC.User,
		Password: cfg.RPC.Password,
		UseTLS:   cfg.RPC.UseTLS,
		Timeout:  time.Duration(cfg.RPC.Timeout) * time.Second,
	})
}

// TestLiveIndexer_CappedHistoricalSync runs the REAL Indexer — real Core
// RPC, real DecodeBlock, real CoreAddressResolver, real Store — against a
// fresh, isolated PostgreSQL schema, capped to genesis..integrationCapHeight.
func TestLiveIndexer_CappedHistoricalSync(t *testing.T) {
	client := requireIndexerIntegration(t)
	ctx := context.Background()

	st, pool := newTestStore(t)
	resolver := decode.NewCoreAddressResolver(client)
	capped := &cappedRPCClient{client: client, cap: integrationCapHeight}

	idx := New(capped, st, resolver, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("SyncToTip: %v", err)
	}

	tip, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != integrationCapHeight {
		t.Fatalf("tip height = %d, want %d", tip.Height, integrationCapHeight)
	}

	realHash, err := client.GetBlockHash(ctx, integrationCapHeight)
	if err != nil {
		t.Fatalf("GetBlockHash(%d): %v", integrationCapHeight, err)
	}
	if tip.Hash != realHash {
		t.Errorf("tip hash = %s, want %s (real Core's block at height %d)", tip.Hash, realHash, integrationCapHeight)
	}

	var blockCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE canonical`).Scan(&blockCount); err != nil {
		t.Fatalf("count canonical blocks: %v", err)
	}
	if blockCount != integrationCapHeight+1 {
		t.Errorf("canonical block count = %d, want %d (genesis through height %d)", blockCount, integrationCapHeight+1, integrationCapHeight)
	}

	// Re-running SyncToTip against the same (still-capped) target must be
	// a clean no-op — proves the pipeline is idempotent against real Core
	// wire data too, not just synthetic fixtures.
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("second (no-op) SyncToTip: %v", err)
	}
	tip2, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip after no-op pass: %v", err)
	}
	if tip2 != tip {
		t.Errorf("tip changed on a no-op pass: before=%+v after=%+v", tip, tip2)
	}

	t.Logf("indexed genesis..%d from real Core into an isolated PostgreSQL schema successfully", integrationCapHeight)
}

// TestLiveIndexer_CurrentTipFetchOnly verifies the CURRENT live Core tip's
// wire format still decodes successfully, without ever applying it to any
// database — exercises today's real format without a multi-million-block
// bootstrap (task item 24).
func TestLiveIndexer_CurrentTipFetchOnly(t *testing.T) {
	client := requireIndexerIntegration(t)
	ctx := context.Background()
	resolver := decode.NewCoreAddressResolver(client)

	tipHeight, err := client.GetBlockCount(ctx)
	if err != nil {
		t.Fatalf("GetBlockCount: %v", err)
	}
	hash, err := client.GetBlockHash(ctx, tipHeight)
	if err != nil {
		t.Fatalf("GetBlockHash(%d): %v", tipHeight, err)
	}
	raw, err := client.GetBlockVerbose2(ctx, hash)
	if err != nil {
		t.Fatalf("GetBlockVerbose2: %v", err)
	}
	block, err := decode.DecodeBlock(ctx, raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(current tip %d): %v", tipHeight, err)
	}
	if block.Height != tipHeight {
		t.Errorf("decoded height = %d, want %d", block.Height, tipHeight)
	}
	t.Logf("current live Core tip %d (%s) decoded successfully; %d transactions", block.Height, block.Hash, len(block.Transactions))
}
