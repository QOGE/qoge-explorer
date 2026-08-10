package decode

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/QOGE/qoge-explorer/internal/config"
	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// This file is entirely opt-in: it exercises a real, currently-running
// Qogecoin Core node over live RPC. Every test here calls
// requireRPCIntegration first, which skips unless QOGE_RPC_INTEGRATION is
// set — so a normal `go test ./...` run never needs a running qogecoind,
// RPC credentials, or network access (task item 14).
//
// Connection parameters come from the same QOGE_RPC_* environment
// variables the rest of this codebase already uses (internal/config). The
// RPC username/password/Authorization header are never printed anywhere in
// this file — only config.RPCConfig.Redacted() (which replaces the
// password with a fixed placeholder) is ever logged.
func requireRPCIntegration(t *testing.T) *rpc.Client {
	t.Helper()
	if os.Getenv("QOGE_RPC_INTEGRATION") == "" {
		t.Skip("QOGE_RPC_INTEGRATION not set; skipping live Qogecoin Core RPC integration test")
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

func TestLiveRPC_GenesisBlockDecodesAgainstRealNode(t *testing.T) {
	client := requireRPCIntegration(t)
	ctx := context.Background()

	hash, err := client.GetBlockHash(ctx, 0)
	if err != nil {
		t.Fatalf("GetBlockHash(0): %v", err)
	}
	const wantGenesisHash = "78cf9e38dad7e61400f3a3e4e987efa7c90c09f69d9be7ce95e504bfa447aadc"
	if hash != wantGenesisHash {
		t.Fatalf("genesis block hash = %s, want %s", hash, wantGenesisHash)
	}

	raw, err := client.GetBlockVerbose2(ctx, hash)
	if err != nil {
		t.Fatalf("GetBlockVerbose2: %v", err)
	}

	resolver := NewCoreAddressResolver(client)
	block, err := DecodeBlock(ctx, raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock: %v", err)
	}
	if block.Height != 0 {
		t.Errorf("Height = %d, want 0", block.Height)
	}
	if block.PreviousHash != "" {
		t.Errorf("PreviousHash = %q, want empty", block.PreviousHash)
	}
	if len(block.Transactions) != 1 {
		t.Fatalf("Transactions length = %d, want 1", len(block.Transactions))
	}
	txn := block.Transactions[0]
	const wantGenesisTxID = "a0bc982915c0435f85fa6e44b7e6bd7b32e2a6ad10f968d223d4a56fa2aabc9e"
	if txn.TxID != wantGenesisTxID {
		t.Errorf("genesis txid = %s, want %s", txn.TxID, wantGenesisTxID)
	}
	if txn.Version != 1 {
		t.Errorf("genesis transaction version = %d, want 1", txn.Version)
	}
	if len(txn.Outputs) != 1 {
		t.Fatalf("genesis Outputs length = %d, want 1", len(txn.Outputs))
	}
	if txn.Outputs[0].Value != 10_000_000_000 {
		t.Errorf("genesis output value = %d, want 10000000000 (100 QOGE)", txn.Outputs[0].Value)
	}
	t.Logf("genesis output classified as %s; resolved address (if any): %q", txn.Outputs[0].ScriptType, txn.Outputs[0].Address)
}

func TestLiveRPC_DescriptorResolutionRoundTrip(t *testing.T) {
	client := requireRPCIntegration(t)
	ctx := context.Background()
	resolver := NewCoreAddressResolver(client)

	// A real, previously-observed QOGE bare-P2PK pubkey (block 1 coinbase;
	// same vector as internal/script/classify_test.go).
	pubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	addr, err := resolver.ResolvePubKeyAddress(ctx, pubKeyHex)
	if err != nil {
		t.Fatalf("ResolvePubKeyAddress: %v", err)
	}
	if addr == "" {
		t.Fatal("resolver returned an empty address")
	}
	t.Logf("live-node-resolved P2PKH address for block 1's coinbase pubkey: %s", addr)
}

// TestLiveRPC_KnownVectorsMatchLiveNode re-verifies, against a real
// running node, that every "real mainnet vector" hardcoded into
// vectors_test.go (txid, scriptPubKey hex, decimal value) still matches
// chain reality — repeatable evidence for task item 13, not a one-off
// manual check.
func TestLiveRPC_KnownVectorsMatchLiveNode(t *testing.T) {
	client := requireRPCIntegration(t)
	ctx := context.Background()

	tests := []struct {
		name          string
		height        int64
		txid          string
		scriptHex     string
		valueSatoshis int64
	}{
		{"block 1 — P2PK", 1, "1d4bdd70951bae6dd62b265a1877b7b140ea86b6ff0cd51102eec801c3947a1d",
			"21029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cdac", 10_000_000_000},
		{"block 8000 — P2PKH", 8000, "a8ee14b21e7d42a4e9c155de159c9836ce932d4c4cccf77a0f23a71acd031b45",
			"76a914db6cdf671aa4dc3a395b934ca08bffb54658f36c88ac", 10_000_000_000},
		{"block 38393 — OP_RETURN", 38393, "01d172762f277d6b2a48bc935ed2603dddd23f9eb85d84bd325fde05a3787be0",
			"6a24aa21a9ede2f61c3f71d1defd3fa999dfa36953755c690689799962b48bebd836974e8cf9", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := client.GetBlockHash(ctx, tt.height)
			if err != nil {
				t.Fatalf("GetBlockHash(%d): %v", tt.height, err)
			}
			raw, err := client.GetBlockVerbose2(ctx, hash)
			if err != nil {
				t.Fatalf("GetBlockVerbose2: %v", err)
			}

			var found *rpc.RawVout
			var foundTx *rpc.RawTransaction
			for i := range raw.Tx {
				if raw.Tx[i].TxID != nil && *raw.Tx[i].TxID == tt.txid {
					foundTx = &raw.Tx[i]
					for j := range raw.Tx[i].Vout {
						spk := raw.Tx[i].Vout[j].ScriptPubKey
						if spk != nil && spk.Hex != nil && *spk.Hex == tt.scriptHex {
							found = &raw.Tx[i].Vout[j]
						}
					}
				}
			}
			if foundTx == nil {
				t.Fatalf("txid %s not found in live block %d (%s)", tt.txid, tt.height, hash)
			}
			if found == nil {
				t.Fatalf("scriptPubKey %s not found on any output of live txid %s", tt.scriptHex, tt.txid)
			}
			value, err := DecodeAmount(found.Value)
			if err != nil {
				t.Fatalf("decode live value %s: %v", found.Value, err)
			}
			if int64(value) != tt.valueSatoshis {
				t.Errorf("live value = %d satoshis, want %d", value, tt.valueSatoshis)
			}
		})
	}
}

func TestLiveRPC_DecodeRecentBlockRoundTrip(t *testing.T) {
	client := requireRPCIntegration(t)
	ctx := context.Background()

	info, err := client.GetBlockchainInfo(ctx)
	if err != nil {
		t.Fatalf("GetBlockchainInfo: %v", err)
	}
	if info.Blocks == 0 {
		t.Skip("node reports height 0; nothing beyond genesis to decode")
	}

	hash, err := client.GetBlockHash(ctx, info.Blocks)
	if err != nil {
		t.Fatalf("GetBlockHash(%d): %v", info.Blocks, err)
	}
	raw, err := client.GetBlockVerbose2(ctx, hash)
	if err != nil {
		t.Fatalf("GetBlockVerbose2: %v", err)
	}

	resolver := NewCoreAddressResolver(client)
	block, err := DecodeBlock(ctx, raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(tip block %d): %v", info.Blocks, err)
	}
	if block.TxCount != len(block.Transactions) {
		t.Errorf("TxCount=%d but decoded %d transactions", block.TxCount, len(block.Transactions))
	}
	t.Logf("decoded live tip block %d (%s): %d transactions", block.Height, block.Hash, len(block.Transactions))
}
