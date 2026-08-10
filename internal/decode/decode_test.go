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
// unit tests must never require a running qogecoind (task item 9, prior
// round).
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

// emptyResolver always resolves to ("", nil) — used to prove the decoder
// rejects that result itself rather than trusting a resolver
// implementation that doesn't already treat it as an error (task item 6).
type emptyResolver struct{}

func (emptyResolver) ResolvePubKeyAddress(_ context.Context, _ string) (string, error) {
	return "", nil
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

// ─── pointer helpers ──────────────────────────────────────────────────────
//
// rpc's raw DTOs use pointers for every field Core always emits, so a
// missing key (nil) is distinguishable from a legitimate Go zero value
// (0, "") that Core actually reported (task item 1, this review round).
// These convert plain test-fixture literals into that pointer shape;
// optStrPtr specifically preserves this test suite's existing convention
// of using "" to mean "field intentionally absent" in fixture builders
// (distinct from a hex field that is legitimately present-but-empty,
// which individual tests construct explicitly rather than through these
// helpers).

func strPtr(s string) *string       { return &s }
func intPtr(i int) *int             { return &i }
func int64Ptr(i int64) *int64       { return &i }
func uint32Ptr(u uint32) *uint32    { return &u }
func float64Ptr(f float64) *float64 { return &f }

func optStrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return strPtr(s)
}

func rawBlockFixture(hash string, height int64, prevHash string, txs ...rpc.RawTransaction) rpc.RawBlock {
	return rpc.RawBlock{
		Hash:              strPtr(hash),
		Height:            int64Ptr(height),
		PreviousBlockHash: optStrPtr(prevHash),
		MerkleRoot:        strPtr(hash),
		Time:              int64Ptr(1_700_000_000 + height),
		Bits:              strPtr("1d00ffff"),
		Difficulty:        float64Ptr(1.0),
		Nonce:             uint32Ptr(uint32(height)),
		Size:              intPtr(100),
		Weight:            intPtr(400),
		NTx:               intPtr(len(txs)),
		Tx:                txs,
	}
}

func rawCoinbaseTx(txid string, vout ...rpc.RawVout) rpc.RawTransaction {
	cb := "51"
	return rpc.RawTransaction{
		TxID: optStrPtr(txid), Hash: optStrPtr(txid), // no witness data: hash == txid
		Version: uint32Ptr(2), Size: intPtr(100), VSize: intPtr(100), Weight: intPtr(400), LockTime: uint32Ptr(0),
		Vin:  []rpc.RawVin{{Coinbase: &cb, Sequence: uint32Ptr(4294967295)}},
		Vout: vout,
	}
}

func rawSpendTx(txid, wtxid string, vin []rpc.RawVin, vout []rpc.RawVout) rpc.RawTransaction {
	return rpc.RawTransaction{
		TxID: optStrPtr(txid), Hash: optStrPtr(wtxid),
		Version: uint32Ptr(2), Size: intPtr(200), VSize: intPtr(200), Weight: intPtr(800), LockTime: uint32Ptr(0),
		Vin: vin, Vout: vout,
	}
}

// rawSpendVin always supplies every required ordinary-vin field —
// scriptSigHex is passed through as the (always-present) scriptSig.hex
// value, so "" here legitimately means "present but empty" (a pure
// witness spend), never "absent".
func rawSpendVin(prevTxid string, prevVout uint32, scriptSigHex string, witness ...string) rpc.RawVin {
	return rpc.RawVin{
		TxID: strPtr(prevTxid), Vout: uint32Ptr(prevVout),
		ScriptSig:   &rpc.RawScriptSig{Hex: strPtr(scriptSigHex)},
		Sequence:    uint32Ptr(4294967295),
		TxInWitness: witness,
	}
}

func rawCoinbaseVin(coinbaseHex string) rpc.RawVin {
	return rpc.RawVin{Coinbase: strPtr(coinbaseHex), Sequence: uint32Ptr(4294967295)}
}

func rawP2PKHVout(n uint32, valueQOGE, address string) rpc.RawVout {
	// OP_DUP OP_HASH160 <20 bytes> OP_EQUALVERIFY OP_CHECKSIG
	scriptHex := "76a914" + strings.Repeat("ab", 20) + "88ac"
	return rpc.RawVout{
		Value:        json.Number(valueQOGE),
		N:            uint32Ptr(n),
		ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex), Type: "pubkeyhash", Address: optStrPtr(address)},
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
		Hash:              strPtr(rawHash("blockA")),
		Height:            int64Ptr(12345),
		PreviousBlockHash: strPtr(rawHash("blockAparent")),
		MerkleRoot:        strPtr(rawHash("blockAmerkle")),
		Time:              int64Ptr(1_700_000_123),
		Bits:              strPtr("1d00ffff"),
		Difficulty:        float64Ptr(1.5),
		Nonce:             uint32Ptr(999),
		Size:              intPtr(321),
		Weight:            intPtr(1284),
		NTx:               intPtr(1),
		Tx:                []rpc.RawTransaction{rawCoinbaseTx(rawHash("blockAtx"), rawP2PKHVout(0, "5", "qAlice"))},
	}
	block, err := DecodeBlock(ctx, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("DecodeBlock: %v", err)
	}
	if block.Hash != *raw.Hash || block.Height != *raw.Height || block.PreviousHash != *raw.PreviousBlockHash ||
		block.MerkleRoot != *raw.MerkleRoot || block.Time != *raw.Time || block.Bits != *raw.Bits ||
		block.Difficulty != *raw.Difficulty || block.Nonce != *raw.Nonce || block.Size != *raw.Size ||
		block.Weight != *raw.Weight || block.TxCount != *raw.NTx {
		t.Errorf("field mapping mismatch: got %+v", block)
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
	raw.NTx = intPtr(2) // claims 2 transactions but only 1 supplied
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := rawBlockFixture(tt.hash, 100, rawHash("blockCparent"), rawCoinbaseTx(rawHash("blockCtx"), rawP2PKHVout(0, "5", "qAlice")))
			if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
				t.Fatalf("expected a malformed block hash (%s) to be rejected", tt.name)
			}
		})
	}
}

func TestDecodeBlock_MalformedMerkleRootRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawBlockFixture(rawHash("blockD"), 100, rawHash("blockDparent"), rawCoinbaseTx(rawHash("blockDtx"), rawP2PKHVout(0, "5", "qAlice")))
	raw.MerkleRoot = strPtr("not-a-hash")
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

// ─── review round: required-field presence, not zero-value inference ────
// (task item 1)

func TestDecodeBlock_MissingRequiredFieldsRejected(t *testing.T) {
	ctx := context.Background()
	base := func() rpc.RawBlock {
		return rawBlockFixture(rawHash("reqblock"), 100, rawHash("reqblockparent"), rawCoinbaseTx(rawHash("reqblocktx"), rawP2PKHVout(0, "5", "qAlice")))
	}

	tests := []struct {
		name   string
		mutate func(*rpc.RawBlock)
	}{
		{"missing hash", func(b *rpc.RawBlock) { b.Hash = nil }},
		{"missing height (would otherwise look like legitimate genesis 0)", func(b *rpc.RawBlock) { b.Height = nil }},
		{"missing merkleroot", func(b *rpc.RawBlock) { b.MerkleRoot = nil }},
		{"missing time", func(b *rpc.RawBlock) { b.Time = nil }},
		{"missing bits", func(b *rpc.RawBlock) { b.Bits = nil }},
		{"missing difficulty", func(b *rpc.RawBlock) { b.Difficulty = nil }},
		{"missing nonce (would otherwise look like a legitimate zero nonce)", func(b *rpc.RawBlock) { b.Nonce = nil }},
		{"missing size", func(b *rpc.RawBlock) { b.Size = nil }},
		{"missing weight", func(b *rpc.RawBlock) { b.Weight = nil }},
		{"missing nTx", func(b *rpc.RawBlock) { b.NTx = nil }},
		{"missing tx", func(b *rpc.RawBlock) { b.Tx = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := base()
			tt.mutate(&raw)
			if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
				t.Fatalf("expected %s to be rejected", tt.name)
			}
		})
	}
}

func TestDecodeTransaction_MissingRequiredFieldsRejected(t *testing.T) {
	ctx := context.Background()
	base := func() rpc.RawTransaction {
		return rawSpendTx(rawHash("reqtx"), rawHash("reqtx"),
			[]rpc.RawVin{rawSpendVin(rawHash("reqtxprev"), 0, "aabb")},
			[]rpc.RawVout{rawP2PKHVout(0, "1", "qBob")},
		)
	}
	tests := []struct {
		name   string
		mutate func(*rpc.RawTransaction)
	}{
		{"missing txid", func(tx *rpc.RawTransaction) { tx.TxID = nil }},
		{"missing hash", func(tx *rpc.RawTransaction) { tx.Hash = nil }},
		{"missing version (would otherwise look like a legitimate version 0)", func(tx *rpc.RawTransaction) { tx.Version = nil }},
		{"missing size", func(tx *rpc.RawTransaction) { tx.Size = nil }},
		{"missing vsize", func(tx *rpc.RawTransaction) { tx.VSize = nil }},
		{"missing weight", func(tx *rpc.RawTransaction) { tx.Weight = nil }},
		{"missing locktime (would otherwise look like a legitimate locktime 0)", func(tx *rpc.RawTransaction) { tx.LockTime = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := base()
			tt.mutate(&raw)
			if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
				t.Fatalf("expected %s to be rejected", tt.name)
			}
		})
	}
}

func TestDecodeInput_MissingRequiredFieldsRejected(t *testing.T) {
	tests := []struct {
		name string
		vin  rpc.RawVin
	}{
		{"ordinary vin missing vout (would otherwise look like a legitimate vout 0)",
			rpc.RawVin{TxID: strPtr(rawHash("reqvinprev")), ScriptSig: &rpc.RawScriptSig{Hex: strPtr("aa")}, Sequence: uint32Ptr(0)}},
		{"ordinary vin missing sequence (would otherwise look like a legitimate sequence 0)",
			rpc.RawVin{TxID: strPtr(rawHash("reqvinprev")), Vout: uint32Ptr(0), ScriptSig: &rpc.RawScriptSig{Hex: strPtr("aa")}}},
		{"coinbase vin missing sequence",
			rpc.RawVin{Coinbase: strPtr("51")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeInput(0, tt.vin); err == nil {
				t.Fatalf("expected %s to be rejected", tt.name)
			}
		})
	}
}

func TestDecodeOutput_MissingNRejected(t *testing.T) {
	ctx := context.Background()
	raw := rpc.RawVout{
		Value:        "1",
		ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr("76a914" + strings.Repeat("ab", 20) + "88ac"), Address: strPtr("qAlice")},
	}
	if _, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a vout missing n (at output index 0, where n=0 would otherwise look legitimate) to be rejected")
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
	raw.Version = uint32Ptr(7)
	raw.LockTime = uint32Ptr(500000)
	raw.Size = intPtr(250)
	raw.VSize = intPtr(200)
	raw.Weight = intPtr(998)
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
	raw := rawSpendTx(rawHash("txmc"), rawHash("txmc"), nil, []rpc.RawVout{rawP2PKHVout(0, "1", "qBob")})
	raw.Vin = []rpc.RawVin{rawCoinbaseVin("51"), rawCoinbaseVin("52")}
	if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected two coinbase-shaped inputs to be rejected")
	}
}

func TestDecodeTransaction_CoinbaseMixedWithRealInputRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawSpendTx(rawHash("txcm"), rawHash("txcm"), nil, []rpc.RawVout{rawP2PKHVout(0, "1", "qBob")})
	raw.Vin = []rpc.RawVin{rawCoinbaseVin("51"), rawSpendVin(rawHash("prevtxcm"), 0, "aabb")}
	if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a coinbase input mixed with a real input to be rejected")
	}
}

// ─── input decoding: coinbase, ordinary, witness ─────────────────────────

func TestDecodeInput_Coinbase(t *testing.T) {
	cb := "0301020304ffff"
	raw := rpc.RawVin{Coinbase: &cb, Sequence: uint32Ptr(4294967295)}
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
	_, err := decodeInput(0, rpc.RawVin{Coinbase: &cb, Sequence: uint32Ptr(0)})
	if err == nil {
		t.Fatal("expected invalid coinbase hex to be rejected")
	}
}

func TestDecodeInput_CoinbaseEmptyBytesRejected(t *testing.T) {
	cb := ""
	_, err := decodeInput(0, rpc.RawVin{Coinbase: &cb, Sequence: uint32Ptr(0)})
	if err == nil {
		t.Fatal("expected empty coinbase script bytes to be rejected")
	}
}

func TestDecodeInput_Ordinary(t *testing.T) {
	prevTxid := rawHash("prevord")
	raw := rpc.RawVin{
		TxID: strPtr(prevTxid), Vout: uint32Ptr(3),
		ScriptSig: &rpc.RawScriptSig{Hex: strPtr("483045022100")},
		Sequence:  uint32Ptr(4294967294),
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
		TxID: strPtr(rawHash("prevpw")), Vout: uint32Ptr(0),
		ScriptSig:   &rpc.RawScriptSig{Hex: strPtr("")}, // present, legitimately empty
		Sequence:    uint32Ptr(4294967295),
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
	raw := rpc.RawVin{Vout: uint32Ptr(0), ScriptSig: &rpc.RawScriptSig{Hex: strPtr("aa")}, Sequence: uint32Ptr(0)}
	if _, err := decodeInput(0, raw); err == nil {
		t.Fatal("expected an ordinary input with no txid and no coinbase field to be rejected")
	}
}

func TestDecodeInput_InvalidScriptSigHexRejected(t *testing.T) {
	raw := rpc.RawVin{
		TxID: strPtr(rawHash("prevbadsig")), Vout: uint32Ptr(0),
		ScriptSig: &rpc.RawScriptSig{Hex: strPtr("zzzz")},
		Sequence:  uint32Ptr(0),
	}
	if _, err := decodeInput(0, raw); err == nil {
		t.Fatal("expected invalid scriptSig hex to be rejected")
	}
}

func TestDecodeInput_InvalidPrevTxIDRejected(t *testing.T) {
	raw := rpc.RawVin{
		TxID: strPtr("not-a-hash"), Vout: uint32Ptr(0),
		ScriptSig: &rpc.RawScriptSig{Hex: strPtr("aa")},
		Sequence:  uint32Ptr(0),
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

// ─── review round: raw vin wire-shape exclusivity (task item 3) ─────────

func TestDecodeInput_ContradictoryWireShapesRejected(t *testing.T) {
	tests := []struct {
		name string
		vin  rpc.RawVin
	}{
		{"coinbase + txid", rpc.RawVin{
			Coinbase: strPtr("51"), TxID: strPtr(rawHash("contraprev")), Sequence: uint32Ptr(0),
		}},
		{"coinbase + vout", rpc.RawVin{
			Coinbase: strPtr("51"), Vout: uint32Ptr(0), Sequence: uint32Ptr(0),
		}},
		{"coinbase + scriptSig", rpc.RawVin{
			Coinbase: strPtr("51"), ScriptSig: &rpc.RawScriptSig{Hex: strPtr("")}, Sequence: uint32Ptr(0),
		}},
		{"ordinary txid present but vout missing", rpc.RawVin{
			TxID: strPtr(rawHash("contraprev")), ScriptSig: &rpc.RawScriptSig{Hex: strPtr("")}, Sequence: uint32Ptr(0),
		}},
		{"ordinary vin missing scriptSig entirely", rpc.RawVin{
			TxID: strPtr(rawHash("contraprev")), Vout: uint32Ptr(0), Sequence: uint32Ptr(0),
		}},
		{"scriptSig object present but hex member missing", rpc.RawVin{
			TxID: strPtr(rawHash("contraprev")), Vout: uint32Ptr(0), ScriptSig: &rpc.RawScriptSig{}, Sequence: uint32Ptr(0),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeInput(0, tt.vin); err == nil {
				t.Fatalf("expected %s to be rejected", tt.name)
			}
		})
	}
}

// ─── review round: genesis / previousblockhash relation (task item 4) ───

func TestDecodeBlock_GenesisRelation(t *testing.T) {
	ctx := context.Background()
	coinbase := func(label string) rpc.RawTransaction {
		return rawCoinbaseTx(rawHash(label), rawP2PKHVout(0, "5", "qAlice"))
	}

	t.Run("height 0 with previousblockhash present rejected", func(t *testing.T) {
		raw := rawBlockFixture(rawHash("genrelA"), 0, "", coinbase("genrelAtx"))
		raw.PreviousBlockHash = strPtr(rawHash("genrelAparent"))
		if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
			t.Fatal("expected height 0 with a present previousblockhash to be rejected")
		}
	})

	t.Run("height > 0 without previousblockhash rejected", func(t *testing.T) {
		raw := rawBlockFixture(rawHash("genrelB"), 1, "", coinbase("genrelBtx"))
		if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
			t.Fatal("expected a non-genesis height missing previousblockhash to be rejected")
		}
	})

	t.Run("negative height rejected", func(t *testing.T) {
		raw := rawBlockFixture(rawHash("genrelC"), 1, rawHash("genrelCparent"), coinbase("genrelCtx"))
		raw.Height = int64Ptr(-1)
		if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
			t.Fatal("expected a negative height to be rejected")
		}
	})

	t.Run("height 0 without previousblockhash accepted", func(t *testing.T) {
		raw := rawBlockFixture(rawHash("genrelD"), 0, "", coinbase("genrelDtx"))
		if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err != nil {
			t.Fatalf("expected genesis to be accepted: %v", err)
		}
	})

	t.Run("height > 0 with valid previousblockhash accepted", func(t *testing.T) {
		raw := rawBlockFixture(rawHash("genrelE"), 1, rawHash("genrelEparent"), coinbase("genrelEtx"))
		if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err != nil {
			t.Fatalf("expected a normal non-genesis block to be accepted: %v", err)
		}
	})
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
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr("zz")}}
	if _, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected invalid scriptPubKey hex to be rejected")
	}
}

// ─── review round: empty scriptPubKey is valid data (task item 2) ───────

func TestDecodeOutput_EmptyScriptPubKeyHexAccepted(t *testing.T) {
	ctx := context.Background()
	raw := rpc.RawVout{Value: "0", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr("")}}
	out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
	if err != nil {
		t.Fatalf("expected a present-but-empty scriptPubKey.hex to be accepted: %v", err)
	}
	if len(out.ScriptPubKey) != 0 {
		t.Errorf("ScriptPubKey = %x, want zero-length", out.ScriptPubKey)
	}
	if out.ScriptPubKey == nil {
		t.Error("ScriptPubKey is nil, want a non-nil zero-length []byte (raw bytes preserved, not invented)")
	}
	if out.ScriptType != script.TypeUnknown {
		t.Errorf("ScriptType = %s, want %s", out.ScriptType, script.TypeUnknown)
	}
	if out.Address != "" {
		t.Errorf("Address = %q, want empty", out.Address)
	}
}

func TestDecodeOutput_MissingScriptPubKeyObjectRejected(t *testing.T) {
	ctx := context.Background()
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: nil}
	if _, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a missing scriptPubKey object to be rejected")
	}
}

func TestDecodeOutput_ScriptPubKeyMissingHexMemberRejected(t *testing.T) {
	ctx := context.Background()
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{}}
	if _, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected a scriptPubKey object with a missing hex member to be rejected")
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
		Value: "1", N: uint32Ptr(0),
		ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex), Type: "pubkeyhash", Address: strPtr("qLies")},
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

func TestDecodeOutput_UnknownWitnessNeverUpgradedToP2QPK(t *testing.T) {
	ctx := context.Background()
	// witness version 3 (not 0/1/2), 32-byte program: structurally
	// TypeUnknownWitness, must NOT be upgraded to P2QPK just because it's
	// witness_unknown-shaped in some generic sense.
	prog := strings.Repeat("cd", 32)
	scriptHex := "5320" + prog // version-3 push opcode is 0x53
	raw := rpc.RawVout{
		Value: "1", N: uint32Ptr(0),
		ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex), Type: "witness_unknown", Address: strPtr("qWitnessUnknown")},
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

// ─── review round: Core address presence by structural type (item 5) ────

func TestDecodeOutput_AddressPresenceByType(t *testing.T) {
	ctx := context.Background()
	p2pkhScript := "76a914" + strings.Repeat("ab", 20) + "88ac"
	p2trScript := "5120" + strings.Repeat("cd", 32)
	p2qpkScript := "5220" + strings.Repeat("ef", 32)
	unknownWitnessScript := "5320" + strings.Repeat("11", 32) // v3/32
	nullDataScript := "6a04deadbeef"
	nonstandardScript := "006301020304" // OP_0 OP_IF <push> ...: not a recognized standard type

	tests := []struct {
		name      string
		scriptHex string
		address   *string
		wantErr   bool
	}{
		{"P2PKH with address -> accepted", p2pkhScript, strPtr("qPKH"), false},
		{"P2PKH missing address -> rejected", p2pkhScript, nil, true},
		{"P2TR missing address -> rejected", p2trScript, nil, true},
		{"P2TR with address -> accepted", p2trScript, strPtr("bq1pTaproot"), false},
		{"P2QPK v2/32 missing address -> rejected", p2qpkScript, nil, true},
		{"P2QPK v2/32 with address -> accepted", p2qpkScript, strPtr("qP2QPKWitnessUnknown"), false},
		{"unknown_witness with address -> accepted", unknownWitnessScript, strPtr("qUnknownWitness"), false},
		{"unknown_witness missing address -> rejected", unknownWitnessScript, nil, true},
		{"nulldata with no address -> accepted", nullDataScript, nil, false},
		{"nulldata with fabricated address -> rejected", nullDataScript, strPtr("qFabricated"), true},
		{"nonstandard/unknown with fabricated address -> rejected", nonstandardScript, strPtr("qFabricated"), true},
		{"nonstandard/unknown with no address -> accepted", nonstandardScript, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := rpc.RawVout{
				Value:        "1",
				N:            uint32Ptr(0),
				ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(tt.scriptHex), Address: tt.address},
			}
			out, err := decodeOutput(ctx, 0, raw, newFakeResolver(nil))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got output %+v", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected acceptance, got error: %v", err)
			}
			if tt.address != nil {
				if out.Address != *tt.address {
					t.Errorf("Address = %q, want %q", out.Address, *tt.address)
				}
			} else if out.Address != "" {
				t.Errorf("Address = %q, want empty", out.Address)
			}
		})
	}
}

func TestDecodeOutput_P2PKUnexpectedAddressRejected(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	scriptHex := "21" + pubKeyHex + "ac"
	raw := rpc.RawVout{
		Value: "1", N: uint32Ptr(0),
		// Core's ScriptToUniv never emits "address" for PUBKEY — a present
		// one here is contradictory RPC data.
		ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex), Address: strPtr("qUnexpected")},
	}
	resolver := newFakeResolver(map[string]string{pubKeyHex: "qResolved"})
	if _, err := decodeOutput(ctx, 0, raw, resolver); err == nil {
		t.Fatal("expected a P2PK output with an unexpected Core-reported address to be rejected")
	}
}

func TestDecodeOutput_MultisigUnexpectedAddressRejected(t *testing.T) {
	ctx := context.Background()
	pub1 := "02" + strings.Repeat("11", 32)
	scriptHex := "51" + "21" + pub1 + "51ae" // 1-of-1
	raw := rpc.RawVout{
		Value: "1", N: uint32Ptr(0),
		ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex), Address: strPtr("qUnexpected")},
	}
	resolver := newFakeResolver(map[string]string{pub1: "qResolved"})
	if _, err := decodeOutput(ctx, 0, raw, resolver); err == nil {
		t.Fatal("expected a multisig output with an unexpected Core-reported address to be rejected")
	}
}

// ─── review round: resolver must never panic (task item 6) ──────────────

func TestDecodeOutput_P2PKNilResolverErrorsNotPanics(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	scriptHex := "21" + pubKeyHex + "ac"
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex)}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decodeOutput panicked with a nil resolver: %v", r)
		}
	}()
	if _, err := decodeOutput(ctx, 0, raw, nil); err == nil {
		t.Fatal("expected a nil resolver on a P2PK output to produce an error")
	}
}

func TestDecodeOutput_MultisigNilResolverErrorsNotPanics(t *testing.T) {
	ctx := context.Background()
	pub1 := "02" + strings.Repeat("11", 32)
	scriptHex := "51" + "21" + pub1 + "51ae"
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex)}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decodeOutput panicked with a nil resolver: %v", r)
		}
	}()
	if _, err := decodeOutput(ctx, 0, raw, nil); err == nil {
		t.Fatal("expected a nil resolver on a multisig output to produce an error")
	}
}

func TestDecodeOutput_ResolverEmptyAddressRejected(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	scriptHex := "21" + pubKeyHex + "ac"
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex)}}
	if _, err := decodeOutput(ctx, 0, raw, emptyResolver{}); err == nil {
		t.Fatal("expected a resolver returning (\"\", nil) to be rejected at the decoder boundary")
	}
}

func TestCoreAddressResolver_NilClientErrorsNotPanics(t *testing.T) {
	ctx := context.Background()
	resolver := NewCoreAddressResolver(nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ResolvePubKeyAddress panicked with a nil client: %v", r)
		}
	}()
	if _, err := resolver.ResolvePubKeyAddress(ctx, "02"+strings.Repeat("11", 32)); err == nil {
		t.Fatal("expected a nil-client resolver to produce an error")
	}
}

// ─── review round: basic raw metric sanity (task item 7) ────────────────

func TestDecodeBlock_NegativeSizeWeightRejected(t *testing.T) {
	ctx := context.Background()
	coinbase := rawCoinbaseTx(rawHash("negmetrictx"), rawP2PKHVout(0, "5", "qAlice"))

	t.Run("negative size", func(t *testing.T) {
		raw := rawBlockFixture(rawHash("negsizeblk"), 100, rawHash("negsizeparent"), coinbase)
		raw.Size = intPtr(-1)
		if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
			t.Fatal("expected a negative block size to be rejected")
		}
	})
	t.Run("negative weight", func(t *testing.T) {
		raw := rawBlockFixture(rawHash("negweightblk"), 100, rawHash("negweightparent"), coinbase)
		raw.Weight = intPtr(-1)
		if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
			t.Fatal("expected a negative block weight to be rejected")
		}
	})
}

func TestDecodeTransaction_NegativeSizeVSizeWeightRejected(t *testing.T) {
	ctx := context.Background()
	build := func() rpc.RawTransaction {
		return rawSpendTx(rawHash("negtxmetric"), rawHash("negtxmetric"),
			[]rpc.RawVin{rawSpendVin(rawHash("negtxmetricprev"), 0, "aabb")},
			[]rpc.RawVout{rawP2PKHVout(0, "1", "qBob")},
		)
	}
	t.Run("negative size", func(t *testing.T) {
		raw := build()
		raw.Size = intPtr(-1)
		if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
			t.Fatal("expected a negative transaction size to be rejected")
		}
	})
	t.Run("negative vsize", func(t *testing.T) {
		raw := build()
		raw.VSize = intPtr(-1)
		if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
			t.Fatal("expected a negative transaction vsize to be rejected")
		}
	})
	t.Run("negative weight", func(t *testing.T) {
		raw := build()
		raw.Weight = intPtr(-1)
		if _, err := DecodeTransaction(ctx, raw, newFakeResolver(nil)); err == nil {
			t.Fatal("expected a negative transaction weight to be rejected")
		}
	})
}

func TestDecodeBlock_MalformedBitsRejected(t *testing.T) {
	ctx := context.Background()
	raw := rawBlockFixture(rawHash("badbitsblk"), 100, rawHash("badbitsparent"), rawCoinbaseTx(rawHash("badbitstx"), rawP2PKHVout(0, "5", "qAlice")))
	raw.Bits = strPtr("not8chars")
	if _, err := DecodeBlock(ctx, raw, newFakeResolver(nil)); err == nil {
		t.Fatal("expected malformed bits (not exactly 8 lowercase hex characters) to be rejected")
	}
}

// ─── P2PK address resolution (prior round) ───────────────────────────────

func TestDecodeOutput_P2PKResolvesAddressViaResolver(t *testing.T) {
	ctx := context.Background()
	pubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	scriptHex := "21" + pubKeyHex + "ac" // <push 33><pubkey> OP_CHECKSIG
	raw := rpc.RawVout{
		Value: "1", N: uint32Ptr(0),
		ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex)}, // no Address: Core omits it for bare P2PK
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
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex)}}
	resolver := newFakeResolver(nil) // no addresses configured -> always errors
	if _, err := decodeOutput(ctx, 0, raw, resolver); err == nil {
		t.Fatal("expected a P2PK resolution failure to reject the whole output")
	}
}

// ─── bare multisig participant resolution (prior round) ─────────────────

func TestDecodeOutput_MultisigResolvesEveryParticipant(t *testing.T) {
	ctx := context.Background()
	pub1 := strings.Repeat("02", 1) + strings.Repeat("11", 32)
	pub2 := strings.Repeat("03", 1) + strings.Repeat("22", 32)
	// 1-of-2 bare multisig: OP_1 <pub1> <pub2> OP_2 OP_CHECKMULTISIG
	scriptHex := "51" + "21" + pub1 + "21" + pub2 + "52ae"
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex)}}
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
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex)}}
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
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex)}}
	// Only pub1 resolvable — pub2 is not, so the whole output must fail
	// rather than silently shortening ParticipantAddresses.
	resolver := newFakeResolver(map[string]string{pub1: "qOnlyOne"})
	if _, err := decodeOutput(ctx, 0, raw, resolver); err == nil {
		t.Fatal("expected an unresolvable multisig participant to reject the whole output")
	}
}

// ─── resolver memoization (prior round) ──────────────────────────────────

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
	raw := rpc.RawVout{Value: "1", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(scriptHex), Address: strPtr("qP2WPKH")}}
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
