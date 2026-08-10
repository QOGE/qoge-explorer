package query

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// decodedFixture drives blocks through the REAL, already-reviewed
// decode.DecodeBlock -> Store.ApplyBlock pipeline (never ad-hoc SQL, never a
// bypass of the decoder/classifier), producing:
//
//	genesis (height 0): coinbase, ONE bare-P2PK output, 100 QOGE.
//	block1  (height 1): coinbase, ONE P2PKH output, 50 QOGE.
//	block2  (height 2): coinbase2 (P2PKH, 50 QOGE) + spendTx:
//	          vin0 spends block1's coinbase output, empty scriptSig, a
//	          2-item witness stack shaped exactly like a P2QPK spend:
//	          [17,088-byte signature, 32-byte pubkey].
//	          vout0 P2QPK     (v2/32, "qP2QPKDest",   25 QOGE)
//	          vout1 P2TR      (v1/32, "qP2TRDest",    10 QOGE)
//	          vout2 P2WPKH    (v0/20, "qP2WPKHDest",   5 QOGE)
//	          vout3 OP_RETURN (nulldata, no address,   0 QOGE)
//	          fee = 50 - (25+10+5+0) = 10 QOGE
type decodedFixture struct {
	genesisTxid, block1CBTxid, block2CBTxid string
	spendTxid, spendWtxid                   string
	sigItem, pubkeyItem                     []byte
	genesisPubKey                           []byte
}

func buildDecodedFixture(t *testing.T, ctx context.Context, st *store.Store) decodedFixture {
	t.Helper()
	resolver := fakeAddressResolver{}

	genesisScript, genesisPubKey := p2pkScript("genesis-pubkey")
	genesisRaw := rawBlockFixture(fakeHash("dv-genesis"), 0, "",
		rawCoinbaseTx("dv-genesis-cb", "51", rawVout(0, 100_00000000, genesisScript, "pubkey", nil)),
	)
	genesisBlock, err := decode.DecodeBlock(ctx, genesisRaw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(genesis): %v", err)
	}
	if err := st.ApplyBlock(ctx, genesisBlock); err != nil {
		t.Fatalf("ApplyBlock(genesis): %v", err)
	}

	b1Addr := "qBlock1CB"
	b1cbScript := p2pkhScript("dv-block1-cb")
	block1Raw := rawBlockFixture(fakeHash("dv-block1"), 1, genesisBlock.Hash,
		rawCoinbaseTx("dv-block1-cb", "51", rawVout(0, 50_00000000, b1cbScript, "pubkeyhash", &b1Addr)),
	)
	block1, err := decode.DecodeBlock(ctx, block1Raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(block1): %v", err)
	}
	if err := st.ApplyBlock(ctx, block1); err != nil {
		t.Fatalf("ApplyBlock(block1): %v", err)
	}
	block1CBTxid := *block1Raw.Tx[0].TxID

	cb2Addr := "qBlock2CB"
	cb2Script := p2pkhScript("dv-block2-cb")
	cb2 := rawCoinbaseTx("dv-block2-cb", "51", rawVout(0, 50_00000000, cb2Script, "pubkeyhash", &cb2Addr))

	sigItem := bytes.Repeat([]byte{0xab}, script.P2QPKSignatureLength)
	pubkeyItem := bytes.Repeat([]byte{0xcd}, script.P2QPKPublicKeyLength)
	p2qpkProgram := bytes.Repeat([]byte{0xef}, script.P2QPKProgramLength)
	p2trProgram := bytes.Repeat([]byte{0x11}, 32)
	p2wpkhProgram := bytes.Repeat([]byte{0x22}, 20)

	p2qpkAddr := "qP2QPKDest"
	p2trAddr := "qP2TRDest"
	p2wpkhAddr := "qP2WPKHDest"

	vin := []rpc.RawVin{
		rawSpendVin(block1CBTxid, 0, "", hex.EncodeToString(sigItem), hex.EncodeToString(pubkeyItem)),
	}
	vout := []rpc.RawVout{
		rawVout(0, 25_00000000, witnessProgramScript(script.P2QPKWitnessVersion, p2qpkProgram), "witness_unknown", &p2qpkAddr),
		rawVout(1, 10_00000000, witnessProgramScript(1, p2trProgram), "witness_v1_taproot", &p2trAddr),
		rawVout(2, 5_00000000, witnessProgramScript(0, p2wpkhProgram), "witness_v0_keyhash", &p2wpkhAddr),
		rawVout(3, 0, nullDataScript([]byte("qoge")), "nulldata", nil),
	}
	spend := rawSpendTx("dv-spend", 17200, 4310, 17240, vin, vout)

	block2Raw := rawBlockFixture(fakeHash("dv-block2"), 2, block1.Hash, cb2, spend)
	block2, err := decode.DecodeBlock(ctx, block2Raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(block2): %v", err)
	}
	if err := st.ApplyBlock(ctx, block2); err != nil {
		t.Fatalf("ApplyBlock(block2): %v", err)
	}

	return decodedFixture{
		genesisTxid:   *genesisRaw.Tx[0].TxID,
		block1CBTxid:  block1CBTxid,
		block2CBTxid:  *cb2.TxID,
		spendTxid:     *spend.TxID,
		spendWtxid:    *spend.Hash,
		sigItem:       sigItem,
		pubkeyItem:    pubkeyItem,
		genesisPubKey: genesisPubKey,
	}
}

// 27: P2QPK end-to-end pipeline — real DecodeBlock -> Store.ApplyBlock ->
// query layer. Default response never dumps the 17,088-byte signature;
// explicit opt-in returns it byte-exact.
func TestDecoded_P2QPKPipeline(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildDecodedFixture(t, ctx, st)

	if f.spendTxid == f.spendWtxid {
		t.Fatalf("fixture invalid: witness-bearing tx must have txid != wtxid")
	}

	// Default: no raw witness.
	got, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}
	p2qpkOut := got.Outputs[0]
	if p2qpkOut.ScriptType != string(script.TypeP2QPK) {
		t.Fatalf("Outputs[0].ScriptType = %s, want p2qpk", p2qpkOut.ScriptType)
	}
	if p2qpkOut.WitnessVersion == nil || *p2qpkOut.WitnessVersion != script.P2QPKWitnessVersion {
		t.Fatalf("Outputs[0].WitnessVersion = %v, want %d", p2qpkOut.WitnessVersion, script.P2QPKWitnessVersion)
	}
	if p2qpkOut.Address == nil || *p2qpkOut.Address != "qP2QPKDest" {
		t.Fatalf("Outputs[0].Address = %v, want qP2QPKDest", p2qpkOut.Address)
	}

	if len(got.Inputs) != 1 || len(got.Inputs[0].Witness) != 2 {
		t.Fatalf("Inputs = %+v, want 1 input with a 2-item witness", got.Inputs)
	}
	w := got.Inputs[0].Witness
	if w[0].SizeBytes != script.P2QPKSignatureLength || w[0].DataHex != nil {
		t.Fatalf("default witness[0] = %+v, want size=%d DataHex=nil", w[0], script.P2QPKSignatureLength)
	}
	if w[1].SizeBytes != script.P2QPKPublicKeyLength || w[1].DataHex != nil {
		t.Fatalf("default witness[1] = %+v, want size=%d DataHex=nil", w[1], script.P2QPKPublicKeyLength)
	}

	// Explicit opt-in: byte-exact raw witness.
	gotRaw, err := q.TransactionByTxID(ctx, f.spendTxid, true)
	if err != nil {
		t.Fatalf("TransactionByTxID(includeRawWitness): %v", err)
	}
	wr := gotRaw.Inputs[0].Witness
	if wr[0].DataHex == nil || *wr[0].DataHex != hex.EncodeToString(f.sigItem) {
		t.Fatalf("raw witness[0] not byte-exact")
	}
	if len(*wr[0].DataHex)/2 != script.P2QPKSignatureLength {
		t.Fatalf("raw witness[0] length = %d, want %d", len(*wr[0].DataHex)/2, script.P2QPKSignatureLength)
	}
	if wr[1].DataHex == nil || *wr[1].DataHex != hex.EncodeToString(f.pubkeyItem) {
		t.Fatalf("raw witness[1] not byte-exact")
	}
}

// 13: P2TR negative control — v1/32 must remain visibly distinct from
// P2QPK's v2/32, never conflated.
func TestDecoded_P2TRDistinctFromP2QPK(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildDecodedFixture(t, ctx, st)

	got, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}

	p2qpk := got.Outputs[0]
	p2tr := got.Outputs[1]

	if p2tr.ScriptType != string(script.TypeP2TR) {
		t.Fatalf("Outputs[1].ScriptType = %s, want p2tr", p2tr.ScriptType)
	}
	if p2tr.WitnessVersion == nil || *p2tr.WitnessVersion != 1 {
		t.Fatalf("Outputs[1].WitnessVersion = %v, want 1", p2tr.WitnessVersion)
	}
	if p2tr.ScriptType == p2qpk.ScriptType {
		t.Fatalf("P2TR and P2QPK outputs classified identically: %s", p2tr.ScriptType)
	}
	if *p2tr.WitnessVersion == *p2qpk.WitnessVersion {
		t.Fatalf("P2TR and P2QPK share witness_version %d, want distinct (1 vs 2)", *p2tr.WitnessVersion)
	}
	if p2tr.WitnessProgram == nil || p2qpk.WitnessProgram == nil || len(*p2tr.WitnessProgram) != 64 || len(*p2qpk.WitnessProgram) != 64 {
		t.Fatalf("both P2TR and P2QPK programs must be 32 bytes (64 hex chars): p2tr=%v p2qpk=%v", p2tr.WitnessProgram, p2qpk.WitnessProgram)
	}
}

// 28: real-vector-shaped presentation across script types, through the real
// decoder pipeline.
func TestDecoded_RealVectorPresentation(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildDecodedFixture(t, ctx, st)

	// Genesis: height 0, P2PK, exact 100 QOGE, resolver-derived address.
	genesisDetail, err := q.BlockByHeight(ctx, 0)
	if err != nil {
		t.Fatalf("BlockByHeight(0): %v", err)
	}
	if !genesisDetail.Canonical || genesisDetail.Height != 0 {
		t.Fatalf("genesis block = %+v", genesisDetail)
	}
	genesisTx, err := q.TransactionByTxID(ctx, f.genesisTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID(genesis): %v", err)
	}
	if genesisTx.Outputs[0].ScriptType != string(script.TypeP2PK) {
		t.Fatalf("genesis output script_type = %s, want p2pk", genesisTx.Outputs[0].ScriptType)
	}
	if genesisTx.Outputs[0].ValueSatoshis != 100_00000000 || genesisTx.Outputs[0].ValueQOGE != "100.00000000" {
		t.Fatalf("genesis output value = (%d, %s), want (10000000000, 100.00000000)",
			genesisTx.Outputs[0].ValueSatoshis, genesisTx.Outputs[0].ValueQOGE)
	}
	wantAddr := "qResolved" + hex.EncodeToString(f.genesisPubKey)[:8]
	if genesisTx.Outputs[0].Address == nil || *genesisTx.Outputs[0].Address != wantAddr {
		t.Fatalf("genesis output address = %v, want %s", genesisTx.Outputs[0].Address, wantAddr)
	}
	// Genesis output is never tracked in canonical UTXO state.
	if genesisTx.Outputs[0].Spent != nil {
		t.Fatalf("genesis output Spent = %v, want nil", genesisTx.Outputs[0].Spent)
	}

	// P2PKH: block1 coinbase.
	block1Tx, err := q.TransactionByTxID(ctx, f.block1CBTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID(block1 coinbase): %v", err)
	}
	if block1Tx.Outputs[0].ScriptType != string(script.TypeP2PKH) {
		t.Fatalf("block1 coinbase script_type = %s, want p2pkh", block1Tx.Outputs[0].ScriptType)
	}
	if block1Tx.Outputs[0].Address == nil || *block1Tx.Outputs[0].Address != "qBlock1CB" {
		t.Fatalf("block1 coinbase address = %v, want qBlock1CB", block1Tx.Outputs[0].Address)
	}

	// OP_RETURN: raw output visible, never treated as a UTXO/balance.
	spendTx, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID(spend): %v", err)
	}
	nullOut := spendTx.Outputs[3]
	if nullOut.ScriptType != string(script.TypeNullData) {
		t.Fatalf("Outputs[3].ScriptType = %s, want nulldata", nullOut.ScriptType)
	}
	if nullOut.Address != nil {
		t.Fatalf("nulldata output Address = %v, want nil", nullOut.Address)
	}
	if nullOut.Spent != nil {
		t.Fatalf("nulldata output Spent = %v, want nil (never a UTXO, never tracked)", nullOut.Spent)
	}

	// P2WPKH.
	wpkhOut := spendTx.Outputs[2]
	if wpkhOut.ScriptType != string(script.TypeP2WPKH) {
		t.Fatalf("Outputs[2].ScriptType = %s, want p2wpkh", wpkhOut.ScriptType)
	}
	if wpkhOut.WitnessVersion == nil || *wpkhOut.WitnessVersion != 0 {
		t.Fatalf("Outputs[2].WitnessVersion = %v, want 0", wpkhOut.WitnessVersion)
	}
	if wpkhOut.Address == nil || *wpkhOut.Address != "qP2WPKHDest" {
		t.Fatalf("Outputs[2].Address = %v, want qP2WPKHDest", wpkhOut.Address)
	}

	// Exact fee: 50 - (25+10+5+0) = 10 QOGE.
	if spendTx.FeeSatoshis == nil || *spendTx.FeeSatoshis != 10_00000000 {
		t.Fatalf("spend fee = %v, want 1000000000", spendTx.FeeSatoshis)
	}
	if spendTx.FeeQOGE == nil || *spendTx.FeeQOGE != "10.00000000" {
		t.Fatalf("spend fee QOGE = %v, want 10.00000000", spendTx.FeeQOGE)
	}
}
