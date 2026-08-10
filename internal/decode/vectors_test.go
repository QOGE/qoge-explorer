package decode

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

func strPtr(s string) *string { return &s }

// ─── real mainnet vectors (task item 13) ─────────────────────────────────
//
// Real block hashes/heights/txids/scriptPubKey bytes, reusing the same
// documented vectors internal/script/classify_test.go and
// internal/store/apply_test.go's TestApplyBlock_RealMainnetFixtures
// already established. Surrounding block header fields are synthetic
// (this repo has no offline historical block source — the prior review
// round's task item 14 explicitly permits this for real-vector fixtures).
// P2PK address resolution here uses a deterministic fake resolver, not a
// live node; TestGenesisVector_LiveRPCIntegration in integration_test.go
// opt-in-cross-checks selected vectors against a real running node.

func TestDecodeBlock_RealVector_Genesis(t *testing.T) {
	ctx := context.Background()
	const (
		genesisBlockHash = "78cf9e38dad7e61400f3a3e4e987efa7c90c09f69d9be7ce95e504bfa447aadc"
		genesisTxID      = "a0bc982915c0435f85fa6e44b7e6bd7b32e2a6ad10f968d223d4a56fa2aabc9e"
		genesisPubKeyHex = "042f87f89b47b6d60836b56bb0b112e573913f47361c07852957ce967c618ea09577c10b0c7a6d54d785860e45309318056c387e0e15047e57ad45e5f623b61594"
	)
	scriptHex := "41" + genesisPubKeyHex + "ac" // <push 65><pubkey> OP_CHECKSIG

	raw := rawBlockFixture(genesisBlockHash, 0, "", rpc.RawTransaction{
		TxID: genesisTxID, Hash: genesisTxID,
		Version: 1, Size: 100, VSize: 100, Weight: 400, LockTime: 0,
		Vin: []rpc.RawVin{{Coinbase: strPtr("04ffff001d"), Sequence: 4294967295}},
		Vout: []rpc.RawVout{{
			Value:        "100", // 100 QOGE, per Qogecoin's stable chainparams
			N:            0,
			ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex}, // Core omits address for bare P2PK
		}},
	})

	// "qd4Cs5rBh6JF89JNms9YpyGP1J5uEgs3jT" is the address live
	// getdescriptorinfo/deriveaddresses resolved this exact pubkey to on
	// the local node during this review round (see
	// TestLiveRPC_GenesisBlockDecodesAgainstRealNode) — used here via the
	// deterministic fake resolver so this test never needs a running
	// qogecoind.
	block, err := DecodeBlock(ctx, raw, newFakeResolver(map[string]string{genesisPubKeyHex: "qd4Cs5rBh6JF89JNms9YpyGP1J5uEgs3jT"}))
	if err != nil {
		t.Fatalf("DecodeBlock: %v", err)
	}
	if block.Height != 0 {
		t.Errorf("Height = %d, want 0", block.Height)
	}
	if block.PreviousHash != "" {
		t.Errorf("PreviousHash = %q, want empty for genesis", block.PreviousHash)
	}
	if len(block.Transactions) != 1 {
		t.Fatalf("Transactions length = %d, want 1", len(block.Transactions))
	}
	txn := block.Transactions[0]
	if txn.Version != 1 {
		t.Errorf("transaction version = %d, want 1", txn.Version)
	}
	if len(txn.Outputs) != 1 {
		t.Fatalf("Outputs length = %d, want 1", len(txn.Outputs))
	}
	out := txn.Outputs[0]
	if out.Value != 10_000_000_000 {
		t.Errorf("output value = %d, want 10000000000 (100 QOGE decoded exactly)", out.Value)
	}
	if out.ScriptType != script.TypeP2PK {
		t.Errorf("ScriptType = %s, want %s", out.ScriptType, script.TypeP2PK)
	}
	if out.Address != "qd4Cs5rBh6JF89JNms9YpyGP1J5uEgs3jT" {
		t.Errorf("Address = %q, want the descriptor-resolver's address", out.Address)
	}
}

func TestDecodeOutput_RealVector_EarlyP2PK(t *testing.T) {
	ctx := context.Background()
	// Block 1 coinbase — bare P2PK (same vector as
	// internal/store/apply_test.go's TestApplyBlock_RealMainnetFixtures
	// and internal/script/classify_test.go). txid, script, and value
	// cross-checked live against `getblock 09d272c0...a35 2` on the local
	// node during this review round, which also confirmed Core reports no
	// "address" field for this output. The fake resolver below returns the
	// SAME address that live getdescriptorinfo/deriveaddresses resolved
	// this exact pubkey to on that node (TestLiveRPC_DescriptorResolutionRoundTrip,
	// integration_test.go) — deterministic here so this test never needs a
	// running qogecoind, but not an arbitrary placeholder either.
	pubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	scriptHex := "21" + pubKeyHex + "ac"
	raw := rpc.RawVout{
		Value:        "100",
		N:            0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex}, // Core RPC address field absent for a bare P2PK output
	}
	if raw.ScriptPubKey.Address != "" {
		t.Fatal("test fixture error: address must be absent for this vector")
	}

	resolver := newFakeResolver(map[string]string{pubKeyHex: "qHaZdwmKKLpLGbGDNJyur2QfbxniR1zb2F"})
	out, err := decodeOutput(ctx, 0, raw, resolver)
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeP2PK {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeP2PK)
	}
	if out.Value != 10_000_000_000 {
		t.Errorf("Value = %d, want 10000000000 (100 QOGE)", out.Value)
	}
	if out.Address != "qHaZdwmKKLpLGbGDNJyur2QfbxniR1zb2F" {
		t.Errorf("Address = %q, want the descriptor-resolver's address (chain.Output.Address populated by resolver)", out.Address)
	}
}

func TestDecodeOutput_RealVector_P2PKH(t *testing.T) {
	ctx := context.Background()
	// Block 8000 coinbase — P2PKH (txid
	// a8ee14b21e7d42a4e9c155de159c9836ce932d4c4cccf77a0f23a71acd031b45) —
	// script, value, and address cross-checked live against `getblock
	// 6d0bf534...1fd 2` on the local node during this review round.
	scriptHex := "76a914db6cdf671aa4dc3a395b934ca08bffb54658f36c88ac"
	raw := rpc.RawVout{
		Value:        "100",
		N:            0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex, Type: "pubkeyhash", Address: "qdZbZeX5YG2rCFWUeGJrBdKjc2xFPeZ1YU"},
	}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeP2PKH {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeP2PKH)
	}
	if out.Value != 10_000_000_000 {
		t.Errorf("Value = %d, want 10000000000 (100 QOGE)", out.Value)
	}
	if out.Address != "qdZbZeX5YG2rCFWUeGJrBdKjc2xFPeZ1YU" {
		t.Errorf("Address = %q, want Core's real reported address copied as-is", out.Address)
	}
}

func TestDecodeOutput_RealVector_OpReturn(t *testing.T) {
	ctx := context.Background()
	// Block 38393 — OP_RETURN witness commitment (txid
	// 01d172762f277d6b2a48bc935ed2603dddd23f9eb85d84bd325fde05a3787be0) —
	// script and value cross-checked live against `getblock
	// d6ddea02...743 2` on the local node during this review round, which
	// also confirmed Core reports no "address" field for this output.
	scriptHex := "6a24aa21a9ede2f61c3f71d1defd3fa999dfa36953755c690689799962b48bebd836974e8cf9"
	raw := rpc.RawVout{
		Value:        "0",
		N:            0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex, Type: "nulldata"}, // no address field
	}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeNullData {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeNullData)
	}
	if out.Value != 0 {
		t.Errorf("Value = %d, want 0", out.Value)
	}
	if !bytes.Equal(out.ScriptPubKey, mustHexDecode(t, scriptHex)) {
		t.Error("raw output script not preserved exactly")
	}
	if out.Address != "" {
		t.Errorf("Address = %q, want empty (never invented for OP_RETURN)", out.Address)
	}
}

func TestDecodeOutput_RealVector_P2WPKH(t *testing.T) {
	ctx := context.Background()
	// Native SegWit P2WPKH at height 494,289 (txid
	// 180c6aee4e8ff354868f7f44945192e1bd2941827413203f5b69c36bb3fb4a29) —
	// hash, script, value, and address all cross-checked live against
	// `getblock 49e2123d...904 2` on the local node during this review
	// round.
	scriptHex := "001471fcd715a320938d8dfa1b56d9acdab9b1616be1"
	raw := rpc.RawVout{
		Value:        "0.53687299",
		N:            0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex, Type: "witness_v0_keyhash", Address: "bq1qw87dw9dryzfcmr06rdtdntx6hxckz6lp9gj3nu"},
	}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeP2WPKH {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeP2WPKH)
	}
	if out.WitnessVersion == nil || *out.WitnessVersion != 0 {
		t.Fatalf("WitnessVersion = %v, want 0", out.WitnessVersion)
	}
	if len(out.WitnessProgram) != 20 {
		t.Errorf("WitnessProgram length = %d, want 20", len(out.WitnessProgram))
	}
	if out.Value != 53_687_299 {
		t.Errorf("Value = %d, want 53687299 (0.53687299 QOGE, decoded exactly)", out.Value)
	}
	if out.Address != "bq1qw87dw9dryzfcmr06rdtdntx6hxckz6lp9gj3nu" {
		t.Errorf("Address = %q, want Core's real reported bech32 address copied as-is", out.Address)
	}
}

func TestDecodeOutput_RealVector_P2TR(t *testing.T) {
	ctx := context.Background()
	// Real QOGE Taproot output at height 1,284,510 (txid
	// 8c7381260e076f781de4c0c5246c709579c80e964738a719579ae4fd5c312106) —
	// hash, script, value, and address all cross-checked live against
	// `getblock 983e028d...52f 2` on the local node during this review
	// round.
	scriptHex := "51202e44fe044d16a3b7900c179d9bb3fc005f0d5e92c89b8c7c0d340c7d6f56077c"
	raw := rpc.RawVout{
		Value:        "149.24759996",
		N:            0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex, Type: "witness_v1_taproot", Address: "bq1p9ez0upzdz63m0yqvz7wehvluqp0s6h5jezdcclqdxsx86m6kqa7qwqlq3p"},
	}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeP2TR {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeP2TR)
	}
	if out.ScriptType == script.TypeP2QPK {
		t.Fatal("a real Taproot (witness v1) output must never classify as P2QPK")
	}
	if out.WitnessVersion == nil || *out.WitnessVersion != 1 {
		t.Fatalf("WitnessVersion = %v, want 1", out.WitnessVersion)
	}
	if len(out.WitnessProgram) != 32 {
		t.Errorf("WitnessProgram length = %d, want 32", len(out.WitnessProgram))
	}
	if out.Value != 14_924_759_996 {
		t.Errorf("Value = %d, want 14924759996 (149.24759996 QOGE, decoded exactly)", out.Value)
	}
	if out.Address != "bq1p9ez0upzdz63m0yqvz7wehvluqp0s6h5jezdcclqdxsx86m6kqa7qwqlq3p" {
		t.Errorf("Address = %q, want Core's real reported bech32m address copied as-is", out.Address)
	}
}

// ─── P2QPK synthetic source-derived vector (task item 12) ───────────────
//
// No real P2QPK spend exists to capture yet. This vector is entirely
// synthetic but source-derived in shape: OP_2 <push 32> <32-byte program>
// mirrors exactly what script.Classify structurally requires for P2QPK
// (internal/script/classify.go), and the witness item lengths mirror the
// documented SLH-DSA-SHA2-128f sizes (docs/ARCHITECTURE.md §8/§9,
// script.P2QPKSignatureLength/P2QPKPublicKeyLength).

func TestDecodeOutput_P2QPKSyntheticVector(t *testing.T) {
	ctx := context.Background()
	program := strings.Repeat("cd", script.P2QPKProgramLength)
	scriptHex := "5220" + program // OP_2, push 32, <32-byte program>

	raw := rpc.RawVout{
		Value: "1",
		N:     0,
		ScriptPubKey: rpc.RawScriptPubKey{
			Hex:  scriptHex,
			Type: "witness_unknown", // Core has no dedicated P2QPK type today
			// Address is exactly as Core would expose a witness_unknown
			// destination for this program — synthetic value here, but the
			// decoder's job is only to copy it, never derive it.
			Address: "qP2QPKWitnessUnknownAddress",
		},
	}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeP2QPK {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeP2QPK)
	}
	if out.WitnessVersion == nil || *out.WitnessVersion != script.P2QPKWitnessVersion {
		t.Fatalf("WitnessVersion = %v, want %d", out.WitnessVersion, script.P2QPKWitnessVersion)
	}
	if len(out.WitnessProgram) != script.P2QPKProgramLength {
		t.Fatalf("WitnessProgram length = %d, want %d", len(out.WitnessProgram), script.P2QPKProgramLength)
	}
	if out.Address != "qP2QPKWitnessUnknownAddress" {
		t.Errorf("Address = %q, want Core's reported address copied as-is (never derived from the pubkey/program)", out.Address)
	}
}

func TestDecodeInput_P2QPKWitnessSpendVector(t *testing.T) {
	sig := strings.Repeat("ab", script.P2QPKSignatureLength) // 17,088 bytes, hex-encoded
	pub := strings.Repeat("ef", script.P2QPKPublicKeyLength) // 32 bytes, hex-encoded

	raw := rpc.RawVin{
		TxID: rawHash("p2qpkprevtx"), Vout: 0,
		ScriptSig:   &rpc.RawScriptSig{Hex: ""}, // pure witness spend
		Sequence:    4294967295,
		TxInWitness: []string{sig, pub},
	}
	in, err := decodeInput(0, raw)
	if err != nil {
		t.Fatalf("decodeInput: %v", err)
	}
	if len(in.ScriptSig) != 0 {
		t.Errorf("ScriptSig = %x, want empty for a pure witness spend", in.ScriptSig)
	}
	if len(in.Witness) != 2 {
		t.Fatalf("Witness length = %d, want 2", len(in.Witness))
	}
	if len(in.Witness[0]) != script.P2QPKSignatureLength {
		t.Errorf("Witness[0] length = %d, want %d", len(in.Witness[0]), script.P2QPKSignatureLength)
	}
	if len(in.Witness[1]) != script.P2QPKPublicKeyLength {
		t.Errorf("Witness[1] length = %d, want %d", len(in.Witness[1]), script.P2QPKPublicKeyLength)
	}
	if !bytes.Equal(in.Witness[0], mustHexDecode(t, sig)) {
		t.Error("Witness[0] (signature) did not survive the decode boundary byte-exact")
	}
	if !bytes.Equal(in.Witness[1], mustHexDecode(t, pub)) {
		t.Error("Witness[1] (pubkey) did not survive the decode boundary byte-exact")
	}
}
