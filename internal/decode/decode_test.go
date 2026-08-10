package decode

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// ─── test fixtures / helpers ─────────────────────────────────────────────

// fakeResolver is a deterministic AddressResolver test double — decoder
// unit tests must never require a running qogecoind (task item 9).
type fakeResolver struct {
	mu    sync.Mutex
	addrs map[string]string
	calls map[string]int
}

func newFakeResolver(pairs map[string]string) *fakeResolver {
	return &fakeResolver{addrs: pairs, calls: map[string]int{}}
}

func (f *fakeResolver) ResolvePubKeyAddress(_ context.Context, pubKeyHex string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[pubKeyHex]++
	addr, ok := f.addrs[pubKeyHex]
	if !ok {
		return "", errors.New("fakeResolver: no address configured for pubkey " + pubKeyHex)
	}
	return addr, nil
}

func (f *fakeResolver) callCount(pubKeyHex string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[pubKeyHex]
}

// rawHash turns a short readable label into a syntactically valid
// 64-lowercase-hex-char hash, mirroring internal/store's own hash64 test
// helper so fixtures stay easy to read.
func rawHash(label string) string {
	h := hex.EncodeToString([]byte(label))
	if len(h) >= 64 {
		return h[:64]
	}
	return h + strings.Repeat("0", 64-len(h))
}

func rawBlockFixture(hash string, height int64, prevHash string, txs ...rpc.RawTransaction) rpc.RawBlock {
	return rpc.RawBlock{
		Hash:              hash,
		Height:            height,
		PreviousBlockHash: prevHash,
		MerkleRoot:        hash,
		Time:              1_700_000_000 + height,
		Bits:              "1d00ffff",
		Difficulty:        1.0,
		Nonce:             uint32(height),
		Size:              100,
		Weight:            400,
		NTx:               len(txs),
		Tx:                txs,
	}
}

func rawCoinbaseTx(txid string, vout ...rpc.RawVout) rpc.RawTransaction {
	cb := "51"
	return rpc.RawTransaction{
		TxID: txid, Hash: txid, // no witness data: hash == txid
		Version: 2, Size: 100, VSize: 100, Weight: 400, LockTime: 0,
		Vin:  []rpc.RawVin{{Coinbase: &cb, Sequence: 4294967295}},
		Vout: vout,
	}
}

func rawSpendTx(txid, wtxid string, vin []rpc.RawVin, vout []rpc.RawVout) rpc.RawTransaction {
	return rpc.RawTransaction{
		TxID: txid, Hash: wtxid,
		Version: 2, Size: 200, VSize: 200, Weight: 800, LockTime: 0,
		Vin: vin, Vout: vout,
	}
}

func rawSpendVin(prevTxid string, prevVout uint32, scriptSigHex string, witness ...string) rpc.RawVin {
	return rpc.RawVin{
		TxID: prevTxid, Vout: prevVout,
		ScriptSig:   &rpc.RawScriptSig{Hex: scriptSigHex},
		Sequence:    4294967295,
		TxInWitness: witness,
	}
}

func rawP2PKHVout(n uint32, valueQOGE, address string) rpc.RawVout {
	// OP_DUP OP_HASH160 <20 bytes> OP_EQUALVERIFY OP_CHECKSIG
	scriptHex := "76a914" + strings.Repeat("ab", 20) + "88ac"
	return rpc.RawVout{
		Value:        json.Number(valueQOGE),
		N:            n,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex, Type: "pubkeyhash", Address: address},
	}
}

func mustHexDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test hex %q: %v", s, err)
	}
	return b
}

// ─── block-level decoding ────────────────────────────────────────────────

func TestDecodeBlock_MapsAllFields(t *testing.T) {
	ctx := context.Background()
	raw := rpc.RawBlock{
		Hash:              rawHash("blockA"),
		Height:            12345,
		PreviousBlockHash: rawHash("blockAparent"),
		MerkleRoot:        rawHash("blockAmerkle"),
		Time:              1_700_000_123,
		Bits:              "1d00ffff",
		Difficulty:        1.5,
		Nonce:             999,
		Size:              321,
		Weight:            1284,
		NTx:               1,
		Tx:                []rpc.RawTransaction{rawCoinbaseTx(rawHash("blockAtx"), rawP2PKHVout(0, "5", "qAlice"))},
	}
	block, err := DecodeBlock(ctx, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("DecodeBlock: %v", err)
	}
	if block.Hash != raw.Hash || block.Height != raw.Height || block.PreviousHash != raw.PreviousBlockHash ||
		block.MerkleRoot != raw.MerkleRoot || block.Time != raw.Time || block.Bits != raw.Bits ||
		block.Difficulty != raw.Difficulty || block.Nonce != raw.Nonce || block.Size != raw.Size ||
		block.Weight != raw.Weight || block.TxCount != raw.NTx {
		t.Errorf("field mapping mismatch: got %+v from raw %+v", block, raw)
	}
	if len(block.Transactions) != 1 {
		t.Fatalf("Transactions length = %d, want 1", len(block.Transactions))
	}
}

func TestDecodeBlock_GenesisPreviousHashEmpty(t *testing.T) {
	ctx := context.Background()
	raw := rawBlockFixture(rawHash("genesis"), 0, "", rawCoinbaseTx(rawHash("genesistx"), rawP2PKHVout(0, "100", "qGenesis")))
	block, err := DecodeBlock(ctx, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("DecodeBlock: %v", err)
	}
	if block.PreviousHash != "" {
		t.Errorf("PreviousHash = %q, want empty for genesis", block.PreviousHash)
	}
}

func TestDecodeBlock_NTxMismatchRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawBlockFixture(rawHash("blockB"), 100, rawHash("blockBparent"), rawCoinbaseTx(rawHash("blockBtx"), rawP2PKHVout(0, "5", "qAlice")))
	raw.NTx = 2 // claims 2 transactions but only 1 supplied
	_, err := DecodeBlock(ctx, raw, newFakeResolver(nil))
	if err == nil {
		t.Fatal("expected an nTx/tx-list mismatch to be rejected")
	}
}

func TestDecodeBlock_MalformedHashRejected(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		hash string
	}{
		{"too short", "abcd"},
		{"uppercase", strings.ToUpper(rawHash("blockC"))},
		{"non-hex characters", strings.Repeat("z", 64)},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := rawBlockFixture(tt.hash, 100, "", rawCoinbaseTx(rawHash("blockCtx"), rawP2PKHVout(0, "5", "qAlice")))
			if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
				t.Fatalf("expected a malformed block hash (%s) to be rejected", tt.name)
			}
		})
	}
}

func TestDecodeBlock_MalformedMerkleRootRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawBlockFixture(rawHash("blockD"), 100, "", rawCoinbaseTx(rawHash("blockDtx"), rawP2PKHVout(0, "5", "qAlice")))
	raw.MerkleRoot = "not-a-hash"
	if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a malformed merkleroot to be rejected")
	}
}

func TestDecodeBlock_MalformedPreviousBlockHashRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawBlockFixture(rawHash("blockE"), 101, "not-a-hash", rawCoinbaseTx(rawHash("blockEtx"), rawP2PKHVout(0, "5", "qAlice")))
	if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a malformed (non-empty) previousblockhash to be rejected")
	}
}

// ─── transaction-level decoding: txid/wtxid, version, size fields ───────

func TestDecodeTransaction_TxIDWTxIDPreservedNotDerived(t *testing.T) {
	ctx := context.Background()
	txid := rawHash("txid1")
	wtxid := rawHash("wtxid1")
	raw := rawSpendTx(txid, wtxid,
		[]rpc.RawVin{rawSpendVin(rawHash("prevtx"), 0, "", "aa", "bb")},
		[]rpc.RawVout{rawP2PKHVout(0, "1", "qBob")},
	)
	txn, err := DecodeTransaction(ctx, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("DecodeTransaction: %v", err)
	}
	if txn.TxID != txid {
		t.Errorf("TxID = %s, want %s (Core's txid field, verbatim)", txn.TxID, txid)
	}
	if txn.WTxID != wtxid {
		t.Errorf("WTxID = %s, want %s (Core's hash field, verbatim, never derived)", txn.WTxID, wtxid)
	}
}

func TestDecodeTransaction_VersionLockTimeSizeFields(t *testing.T) {
	ctx := context.Background()
	raw := rawSpendTx(rawHash("txv"), rawHash("txv"),
		[]rpc.RawVin{rawSpendVin(rawHash("prevtxv"), 3, "aabb")},
		[]rpc.RawVout{rawP2PKHVout(0, "1", "qBob")},
	)
	raw.Version = 7
	raw.LockTime = 500000
	raw.Size = 250
	raw.VSize = 200
	raw.Weight = 998
	txn, err := DecodeTransaction(ctx, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("DecodeTransaction: %v", err)
	}
	if txn.Version != 7 || txn.LockTime != 500000 || txn.Size != 250 || txn.VSize != 200 || txn.Weight != 998 {
		t.Errorf("field mismatch: %+v", txn)
	}
}

func TestDecodeTransaction_MissingTxIDRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawSpendTx("", rawHash("wtxidx"),
		[]rpc.RawVin{rawSpendVin(rawHash("prevtxx"), 0, "aabb")},
		[]rpc.RawVout{rawP2PKHVout(0, "1", "qBob")},
	)
	if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a missing txid to be rejected")
	}
}

func TestDecodeTransaction_MissingHashRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawSpendTx(rawHash("txmh"), "",
		[]rpc.RawVin{rawSpendVin(rawHash("prevtxmh"), 0, "aabb")},
		[]rpc.RawVout{rawP2PKHVout(0, "1", "qBob")},
	)
	if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a missing hash (wtxid) to be rejected")
	}
}

func TestDecodeTransaction_NoInputsRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawSpendTx(rawHash("txni"), rawHash("txni"), nil, []rpc.RawVout{rawP2PKHVout(0, "1", "qBob")})
	if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a transaction with zero inputs to be rejected")
	}
}

func TestDecodeTransaction_NoOutputsRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawSpendTx(rawHash("txno"), rawHash("txno"), []rpc.RawVin{rawSpendVin(rawHash("prevtxno"), 0, "aabb")}, nil)
	if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a transaction with zero outputs to be rejected")
	}
}

func TestDecodeTransaction_MultipleCoinbaseShapedInputsRejected(t *testing.T) {
	ctx := context.Background()
	cb1, cb2 := "51", "52"
	raw := rpc.RawTransaction{
		TxID: rawHash("txmc"), Hash: rawHash("txmc"),
		Version: 2, Size: 100, VSize: 100, Weight: 400,
		Vin: []rpc.RawVin{
			{Coinbase: &cb1, Sequence: 4294967295},
			{Coinbase: &cb2, Sequence: 4294967295},
		},
		Vout: []rpc.RawVout{rawP2PKHVout(0, "1", "qBob")},
	}
	if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected two coinbase-shaped inputs to be rejected")
	}
}

func TestDecodeTransaction_CoinbaseMixedWithRealInputRejected(t *testing.T) {
	ctx := context.Background()
	cb := "51"
	raw := rpc.RawTransaction{
		TxID: rawHash("txcm"), Hash: rawHash("txcm"),
		Version: 2, Size: 100, VSize: 100, Weight: 400,
		Vin: []rpc.RawVin{
			{Coinbase: &cb, Sequence: 4294967295},
			rawSpendVin(rawHash("prevtxcm"), 0, "aabb"),
		},
		Vout: []rpc.RawVout{rawP2PKHVout(0, "1", "qBob")},
	}
	if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a coinbase input mixed with a real input to be rejected")
	}
}

// ─── input decoding: coinbase, ordinary, witness ─────────────────────────

func TestDecodeInput_Coinbase(t *testing.T) {
	cb := "0301020304ffff"
	raw := rpc.RawVin{Coinbase: &cb, Sequence: 4294967295}
	in, err := decodeInput(0, raw)
	if err != nil {
		t.Fatalf("decodeInput: %v", err)
	}
	if in.PreviousOut != nil {
		t.Errorf("PreviousOut = %+v, want nil for coinbase", in.PreviousOut)
	}
	want := mustHexDecode(t, cb)
	if !bytes.Equal(in.Coinbase, want) {
		t.Errorf("Coinbase = %x, want %x", in.Coinbase, want)
	}
	if len(in.ScriptSig) != 0 {
		t.Errorf("ScriptSig = %x, want empty for coinbase (coinbase bytes must never land in ScriptSig)", in.ScriptSig)
	}
	if in.Sequence != 4294967295 {
		t.Errorf("Sequence = %d, want 4294967295", in.Sequence)
	}
}

func TestDecodeInput_CoinbaseInvalidHexRejected(t *testing.T) {
	cb := "not-hex"
	_, err := decodeInput(0, rpc.RawVin{Coinbase: &cb, Sequence: 0})
	if err == nil {
		t.Fatal("expected invalid coinbase hex to be rejected")
	}
}

func TestDecodeInput_CoinbaseEmptyBytesRejected(t *testing.T) {
	cb := ""
	_, err := decodeInput(0, rpc.RawVin{Coinbase: &cb, Sequence: 0})
	if err == nil {
		t.Fatal("expected empty coinbase script bytes to be rejected")
	}
}

func TestDecodeInput_Ordinary(t *testing.T) {
	prevTxid := rawHash("prevord")
	raw := rpc.RawVin{
		TxID: prevTxid, Vout: 3,
		ScriptSig: &rpc.RawScriptSig{Hex: "483045022100"},
		Sequence:  4294967294,
	}
	in, err := decodeInput(2, raw)
	if err != nil {
		t.Fatalf("decodeInput: %v", err)
	}
	if in.Index != 2 {
		t.Errorf("Index = %d, want 2", in.Index)
	}
	if in.PreviousOut == nil || in.PreviousOut.TxID != prevTxid || in.PreviousOut.Index != 3 {
		t.Errorf("PreviousOut = %+v, want {%s 3}", in.PreviousOut, prevTxid)
	}
	if in.Coinbase != nil {
		t.Errorf("Coinbase = %x, want nil for an ordinary input", in.Coinbase)
	}
	want := mustHexDecode(t, "483045022100")
	if !bytes.Equal(in.ScriptSig, want) {
		t.Errorf("ScriptSig = %x, want %x", in.ScriptSig, want)
	}
}

func TestDecodeInput_OrdinaryPureWitnessEmptyScriptSigPreserved(t *testing.T) {
	raw := rpc.RawVin{
		TxID: rawHash("prevpw"), Vout: 0,
		ScriptSig:   &rpc.RawScriptSig{Hex: ""},
		Sequence:    4294967295,
		TxInWitness: []string{"aabb"},
	}
	in, err := decodeInput(0, raw)
	if err != nil {
		t.Fatalf("decodeInput: %v", err)
	}
	if len(in.ScriptSig) != 0 {
		t.Errorf("ScriptSig = %x, want empty (pure witness spend)", in.ScriptSig)
	}
	if in.PreviousOut == nil {
		t.Fatal("PreviousOut is nil, want set for a non-coinbase input")
	}
	if len(in.Witness) != 1 {
		t.Fatalf("Witness length = %d, want 1", len(in.Witness))
	}
}

func TestDecodeInput_OrdinaryMissingTxIDRejected(t *testing.T) {
	raw := rpc.RawVin{Vout: 0, ScriptSig: &rpc.RawScriptSig{Hex: "aa"}, Sequence: 0}
	if _, err := decodeInput(0, raw); err == nil {
		t.Fatal("expected an ordinary input with no txid and no coinbase field to be rejected")
	}
}

func TestDecodeInput_InvalidScriptSigHexRejected(t *testing.T) {
	raw := rpc.RawVin{
		TxID: rawHash("prevbadsig"), Vout: 0,
		ScriptSig: &rpc.RawScriptSig{Hex: "zzzz"},
		Sequence:  0,
	}
	if _, err := decodeInput(0, raw); err == nil {
		t.Fatal("expected invalid scriptSig hex to be rejected")
	}
}

func TestDecodeInput_InvalidPrevTxIDRejected(t *testing.T) {
	raw := rpc.RawVin{
		TxID: "not-a-hash", Vout: 0,
		ScriptSig: &rpc.RawScriptSig{Hex: "aa"},
		Sequence:  0,
	}
	if _, err := decodeInput(0, raw); err == nil {
		t.Fatal("expected a malformed prevout txid to be rejected")
	}
}

func TestDecodeWitness_PreservesOrderZeroLengthAndArbitraryBytes(t *testing.T) {
	items := []string{"", "deadbeef", "00", strings.Repeat("ff", 100)}
	stack, err := decodeWitness(items)
	if err != nil {
		t.Fatalf("decodeWitness: %v", err)
	}
	if len(stack) != 4 {
		t.Fatalf("stack length = %d, want 4", len(stack))
	}
	if len(stack[0]) != 0 {
		t.Errorf("item 0 = %x, want zero-length", stack[0])
	}
	if !bytes.Equal(stack[1], mustHexDecode(t, "deadbeef")) {
		t.Errorf("item 1 mismatch: %x", stack[1])
	}
	if !bytes.Equal(stack[2], mustHexDecode(t, "00")) {
		t.Errorf("item 2 mismatch: %x", stack[2])
	}
	if !bytes.Equal(stack[3], mustHexDecode(t, strings.Repeat("ff", 100))) {
		t.Errorf("item 3 mismatch (100-byte item)")
	}
}

func TestDecodeWitness_InvalidHexRejected(t *testing.T) {
	if _, err := decodeWitness([]string{"aa", "not-hex"}); err == nil {
		t.Fatal("expected an invalid witness item hex to be rejected")
	}
}

func TestDecodeWitness_EmptyOrNilInputIsEmptyWitnessStack(t *testing.T) {
	stack, err := decodeWitness(nil)
	if err != nil || len(stack) != 0 {
		t.Errorf("decodeWitness(nil) = %v, %v; want empty, nil", stack, err)
	}
}

// ─── output decoding: value, script classification, address rules ──────

func TestDecodeOutput_ValueAndPositionalIndex(t *testing.T) {
	ctx := context.Background()
	raw := rawP2PKHVout(3, "6.25", "qCarol")
	out, err := decodeOutput(ctx, 3, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.Value != 625_000_000 {
		t.Errorf("Value = %d, want 625000000", out.Value)
	}
	if out.Index != 3 {
		t.Errorf("Index = %d, want 3", out.Index)
	}
}

func TestDecodeOutput_NMismatchRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawP2PKHVout(5, "1", "qDave") // n=5, but decoded at position 2
	if _, err := decodeOutput(ctx, 2, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected an n/position mismatch to be rejected")
	}
}

func TestDecodeOutput_InvalidScriptHexRejected(t *testing.T) {
	ctx := context.Background()
	raw := rpc.RawVout{Value: "1", N: 0, ScriptPubKey: rpc.RawScriptPubKey{Hex: "zz"}}
	if _, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected invalid scriptPubKey hex to be rejected")
	}
}

func TestDecodeOutput_MissingScriptRejected(t *testing.T) {
	ctx := context.Background()
	raw := rpc.RawVout{Value: "1", N: 0, ScriptPubKey: rpc.RawScriptPubKey{Hex: ""}}
	if _, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a missing scriptPubKey to be rejected")
	}
}

func TestDecodeOutput_MalformedAmountRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawP2PKHVout(0, "not-a-number", "qEve")
	if _, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a malformed output amount to be rejected")
	}
}

func TestDecodeOutput_NegativeAmountRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawP2PKHVout(0, "-1", "qEve")
	if _, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a negative output amount to be rejected")
	}
}

func TestDecodeOutput_RPCTypeNeverDrivesClassification(t *testing.T) {
	ctx := context.Background()
	// scriptPubKey.type deliberately LIES (claims "pubkeyhash") but the raw
	// script bytes are a real P2WPKH witness program — classification must
	// follow the bytes, never the RPC-reported type string.
	scriptHex := "0014" + strings.Repeat("ab", 20)
	raw := rpc.RawVout{
		Value: "1", N: 0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex, Type: "pubkeyhash", Address: "qLies"},
	}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeP2WPKH {
		t.Errorf("ScriptType = %s, want %s (structural classification must win over RPC type)", out.ScriptType, script.TypeP2WPKH)
	}
}

func TestDecodeOutput_OrdinaryAddressCopiedFromCore(t *testing.T) {
	ctx := context.Background()
	raw := rawP2PKHVout(0, "1", "qRealAddress")
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.Address != "qRealAddress" {
		t.Errorf("Address = %s, want qRealAddress (copied as-is from Core)", out.Address)
	}
}

func TestDecodeOutput_NullDataNeverInventsAddress(t *testing.T) {
	ctx := context.Background()
	raw := rpc.RawVout{
		Value: "0", N: 0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: "6a04deadbeef", Type: "nulldata"}, // no Address field
	}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeNullData {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeNullData)
	}
	if out.Address != "" {
		t.Errorf("Address = %q, want empty for OP_RETURN (never invented)", out.Address)
	}
	if out.Value != 0 {
		t.Errorf("Value = %d, want 0", out.Value)
	}
}

func TestDecodeOutput_UnknownWitnessNeverUpgradedToP2QPK(t *testing.T) {
	ctx := context.Background()
	// witness version 3 (not 0/1/2), 32-byte program: structurally
	// TypeUnknownWitness, must NOT be upgraded to P2QPK just because it's
	// witness_unknown-shaped in some generic sense.
	prog := strings.Repeat("cd", 32)
	scriptHex := "6120" + prog // OP_16? no: version 3 push opcode is 0x53
	scriptHex = "5320" + prog
	raw := rpc.RawVout{
		Value: "1", N: 0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex, Type: "witness_unknown", Address: "qWitnessUnknown"},
	}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeUnknownWitness {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeUnknownWitness)
	}
	if out.ScriptType == script.TypeP2QPK {
		t.Fatal("a version-3 witness program must never classify as P2QPK")
	}
	if out.Address != "qWitnessUnknown" {
		t.Errorf("Address = %q, want Core's reported address preserved as RPC metadata", out.Address)
	}
}

// ─── P2PK address resolution (task item 9) ───────────────────────────────

func TestDecodeOutput_P2PKResolvesAddressViaResolver(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	scriptHex := "21" + pubKeyHex + "ac" // <push 33><pubkey> OP_CHECKSIG
	raw := rpc.RawVout{
		Value: "1", N: 0,
		ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex}, // no Address: Core omits it for bare P2PK
	}
	resolver := newFakeResolver(map[string]string{pubKeyHex: "qResolvedP2PKAddress"})
	out, err := decodeOutput(ctx, 0, raw, resolver)
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeP2PK {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeP2PK)
	}
	if out.Address != "qResolvedP2PKAddress" {
		t.Errorf("Address = %q, want the resolver's address", out.Address)
	}
	if resolver.callCount(pubKeyHex) != 1 {
		t.Errorf("resolver called %d times, want 1", resolver.callCount(pubKeyHex))
	}
}

func TestDecodeOutput_P2PKResolutionFailureRejected(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	scriptHex := "21" + pubKeyHex + "ac"
	raw := rpc.RawVout{Value: "1", N: 0, ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex}}
	resolver := newFakeResolver(nil) // no addresses configured -> always errors
	if _, err := decodeOutput(ctx, 0, raw, resolver); err == nil {
		t.Fatal("expected a P2PK resolution failure to reject the whole output")
	}
}

// ─── bare multisig participant resolution (task item 10) ────────────────

func TestDecodeOutput_MultisigResolvesEveryParticipant(t *testing.T) {
	ctx := context.Background()
	pub1 := strings.Repeat("02", 1) + strings.Repeat("11", 32)
	pub2 := strings.Repeat("03", 1) + strings.Repeat("22", 32)
	// 1-of-2 bare multisig: OP_1 <pub1> <pub2> OP_2 OP_CHECKMULTISIG
	scriptHex := "51" + "21" + pub1 + "21" + pub2 + "52ae"
	raw := rpc.RawVout{Value: "1", N: 0, ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex}}
	resolver := newFakeResolver(map[string]string{
		pub1: "qParticipant1",
		pub2: "qParticipant2",
	})
	out, err := decodeOutput(ctx, 0, raw, resolver)
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.ScriptType != script.TypeMultisig {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeMultisig)
	}
	if out.Address != "" {
		t.Errorf("Address = %q, want empty for multisig (never set)", out.Address)
	}
	if len(out.ParticipantAddresses) != 2 || out.ParticipantAddresses[0] != "qParticipant1" || out.ParticipantAddresses[1] != "qParticipant2" {
		t.Errorf("ParticipantAddresses = %v, want [qParticipant1 qParticipant2] (parallel to PubKeys)", out.ParticipantAddresses)
	}
	if len(out.PubKeys) != 2 {
		t.Fatalf("PubKeys length = %d, want 2", len(out.PubKeys))
	}
}

func TestDecodeOutput_MultisigDuplicatePubkeyPreservedPositionally(t *testing.T) {
	ctx := context.Background()
	pub1 := strings.Repeat("02", 1) + strings.Repeat("33", 32)
	// 2-of-2 with the SAME pubkey twice — structurally legal, and the raw
	// script preserves the duplication; the decoder must NOT deduplicate
	// (Store applies identity-set deduplication at persistence instead).
	scriptHex := "52" + "21" + pub1 + "21" + pub1 + "52ae"
	raw := rpc.RawVout{Value: "1", N: 0, ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex}}
	resolver := newFakeResolver(map[string]string{pub1: "qDupParticipant"})
	out, err := decodeOutput(ctx, 0, raw, resolver)
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if len(out.PubKeys) != 2 || len(out.ParticipantAddresses) != 2 {
		t.Fatalf("expected both duplicate entries preserved positionally: PubKeys=%d ParticipantAddresses=%d", len(out.PubKeys), len(out.ParticipantAddresses))
	}
	if out.ParticipantAddresses[0] != "qDupParticipant" || out.ParticipantAddresses[1] != "qDupParticipant" {
		t.Errorf("ParticipantAddresses = %v, want both entries resolved", out.ParticipantAddresses)
	}
}

func TestDecodeOutput_MultisigOneParticipantUnresolvableRejected(t *testing.T) {
	ctx := context.Background()
	pub1 := strings.Repeat("02", 1) + strings.Repeat("44", 32)
	pub2 := strings.Repeat("03", 1) + strings.Repeat("55", 32)
	scriptHex := "51" + "21" + pub1 + "21" + pub2 + "52ae"
	raw := rpc.RawVout{Value: "1", N: 0, ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex}}
	// Only pub1 resolvable — pub2 is not, so the whole output must fail
	// rather than silently shortening ParticipantAddresses.
	resolver := newFakeResolver(map[string]string{pub1: "qOnlyOne"})
	if _, err := decodeOutput(ctx, 0, raw, resolver); err == nil {
		t.Fatal("expected an unresolvable multisig participant to reject the whole output")
	}
}

// ─── resolver memoization ────────────────────────────────────────────────

func TestCoreAddressResolver_MemoizesPerPubKey(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "02" + strings.Repeat("66", 32)

	var mu sync.Mutex
	descriptorCalls, deriveCalls := 0, 0
	srv := newFakeRPCServer(t, func(method string, params []json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "getdescriptorinfo":
			descriptorCalls++
			return rpc.DescriptorInfo{Descriptor: "pkh(" + pubKeyHex + ")#checksum"}, nil
		case "deriveaddresses":
			deriveCalls++
			return []string{"qMemoAddress"}, nil
		default:
			return nil, errors.New("unexpected method " + method)
		}
	})
	defer srv.Close()

	client := rpc.New(rpc.Config{Host: srv.host, Port: srv.port, User: "u", Password: "p", Timeout: 5_000_000_000})
	resolver := NewCoreAddressResolver(client)

	for i := 0; i < 3; i++ {
		addr, err := resolver.ResolvePubKeyAddress(ctx, pubKeyHex)
		if err != nil {
			t.Fatalf("ResolvePubKeyAddress call %d: %v", i, err)
		}
		if addr != "qMemoAddress" {
			t.Errorf("call %d: addr = %s, want qMemoAddress", i, addr)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if descriptorCalls != 1 || deriveCalls != 1 {
		t.Errorf("descriptorCalls=%d deriveCalls=%d, want exactly 1 each (memoized across 3 resolutions of the same pubkey)", descriptorCalls, deriveCalls)
	}
}

func TestCoreAddressResolver_DeriveAddressesWrongCountRejected(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "02" + strings.Repeat("77", 32)
	srv := newFakeRPCServer(t, func(method string, params []json.RawMessage) (any, error) {
		switch method {
		case "getdescriptorinfo":
			return rpc.DescriptorInfo{Descriptor: "pkh(" + pubKeyHex + ")#checksum"}, nil
		case "deriveaddresses":
			return []string{"qOne", "qTwo"}, nil // malformed: want exactly 1
		default:
			return nil, errors.New("unexpected method " + method)
		}
	})
	defer srv.Close()

	client := rpc.New(rpc.Config{Host: srv.host, Port: srv.port, User: "u", Password: "p", Timeout: 5_000_000_000})
	resolver := NewCoreAddressResolver(client)
	if _, err := resolver.ResolvePubKeyAddress(ctx, pubKeyHex); err == nil {
		t.Fatal("expected a malformed (non-1-address) deriveaddresses result to be rejected")
	}
}

func TestCoreAddressResolver_DescriptorFailureRejected(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "02" + strings.Repeat("88", 32)
	srv := newFakeRPCServer(t, func(method string, params []json.RawMessage) (any, error) {
		return nil, errors.New("boom")
	})
	defer srv.Close()

	client := rpc.New(rpc.Config{Host: srv.host, Port: srv.port, User: "u", Password: "p", Timeout: 5_000_000_000})
	resolver := NewCoreAddressResolver(client)
	if _, err := resolver.ResolvePubKeyAddress(ctx, pubKeyHex); err == nil {
		t.Fatal("expected a getdescriptorinfo RPC failure to be rejected")
	}
}

// ─── chain type sanity (guards against silent chain.* drift) ─────────────

func TestDecodeOutput_WitnessProgramPopulatedForWitnessTypes(t *testing.T) {
	ctx := context.Background()
	prog := strings.Repeat("ab", 20)
	scriptHex := "0014" + prog
	raw := rpc.RawVout{Value: "1", N: 0, ScriptPubKey: rpc.RawScriptPubKey{Hex: scriptHex, Address: "qP2WPKH"}}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("decodeOutput: %v", err)
	}
	if out.WitnessVersion == nil || *out.WitnessVersion != 0 {
		t.Errorf("WitnessVersion = %v, want 0", out.WitnessVersion)
	}
	if !bytes.Equal(out.WitnessProgram, mustHexDecode(t, prog)) {
		t.Errorf("WitnessProgram mismatch: %x", out.WitnessProgram)
	}
	var _ chain.Output = out
}
