package query

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// txFixture builds:
//
//	genesis: coinbase -> addrG, 100 QOGE (isGenesis: not in utxo_state)
//	block1:  coinbase with TWO outputs -> addrX1 (30 QOGE), addrX2 (20 QOGE)
//	block2:  coinbase2 (addrCB2, 50 QOGE) + spendTx:
//	           vin0 spends block1:0 (scriptSig + 2-item witness)
//	           vin1 spends block1:1 (scriptSig only, no witness)
//	           vout0 -> addrY1, 15 QOGE, P2WPKH (witness metadata)
//	           vout1 -> addrY2, 34 QOGE, P2PKH
//	         fee = (30+20) - (15+34) = 1 QOGE
//
// spendTx carries witness data on vin0 only, so WTxID != TxID.
type txFixture struct {
	genesis, block1, block2 chain.Block
	spendTxid, spendWtxid   string
	sigItem, pubkeyItem     []byte
	coinbaseBytes           []byte
	scriptSig0, scriptSig1  []byte
}

func buildTxFixture(t *testing.T, ctx context.Context, st *store.Store) txFixture {
	t.Helper()

	g := block("tx-genesis", 0, "", coinbaseTx("tx-genesis", 100_00000000, "qTxGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	coinbaseBytes := []byte{0xde, 0xad, 0xbe, 0xef}
	cb1txid := fakeHash("tx-b1-cb-tx")
	cb1 := chain.Transaction{
		TxID: cb1txid, WTxID: cb1txid,
		Version: 1, LockTime: 0,
		Size: 120, VSize: 120, Weight: 480,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: coinbaseBytes, Sequence: 0xffffffff},
		},
		Outputs: []chain.Output{
			{Index: 0, Value: chain.Amount(30_00000000), ScriptPubKey: p2pkhScript("tx-b1-out0"), ScriptType: script.TypeP2PKH, Address: "qTxX1"},
			{Index: 1, Value: chain.Amount(20_00000000), ScriptPubKey: p2pkhScript("tx-b1-out1"), ScriptType: script.TypeP2PKH, Address: "qTxX2"},
		},
	}
	b1 := block("tx-block1", 1, g.Hash, cb1)
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	cb2 := coinbaseTx("tx-b2-cb", 50_00000000, "qTxCB2")

	sigItem := bytes.Repeat([]byte{0xaa}, 71)
	pubkeyItem := bytes.Repeat([]byte{0xbb}, 33)
	scriptSig0 := []byte{0x16, 0x00, 0x14} // arbitrary, structurally non-empty
	scriptSig1 := []byte{0x47, 0x30, 0x44, 0x02, 0x20}

	spendTxid := fakeHash("tx-b2-spend-tx")
	spendWtxid := fakeHash("tx-b2-spend-wtx")
	witnessVersion0 := 0
	spend := chain.Transaction{
		TxID: spendTxid, WTxID: spendWtxid,
		Version: 2, LockTime: 0,
		Size: 250, VSize: 150, Weight: 600,
		Inputs: []chain.Input{
			{
				Index:       0,
				PreviousOut: &chain.OutPoint{TxID: cb1txid, Index: 0},
				ScriptSig:   scriptSig0,
				Sequence:    0xffffffff,
				Witness:     chain.WitnessStack{sigItem, pubkeyItem},
			},
			{
				Index:       1,
				PreviousOut: &chain.OutPoint{TxID: cb1txid, Index: 1},
				ScriptSig:   scriptSig1,
				Sequence:    0xfffffffe,
			},
		},
		Outputs: []chain.Output{
			{
				Index:          0,
				Value:          chain.Amount(15_00000000),
				ScriptPubKey:   bytes.Repeat([]byte{0x00}, 22),
				ScriptType:     script.TypeP2WPKH,
				WitnessVersion: &witnessVersion0,
				WitnessProgram: bytes.Repeat([]byte{0xcc}, 20),
				Address:        "qTxY1",
			},
			{
				Index:        1,
				Value:        chain.Amount(34_00000000),
				ScriptPubKey: p2pkhScript("tx-b2-out1"),
				ScriptType:   script.TypeP2PKH,
				Address:      "qTxY2",
			},
		},
	}

	b2 := block("tx-block2", 2, b1.Hash, cb2, spend)
	if err := st.ApplyBlock(ctx, b2); err != nil {
		t.Fatalf("apply block2: %v", err)
	}

	return txFixture{
		genesis: g, block1: b1, block2: b2,
		spendTxid: spendTxid, spendWtxid: spendWtxid,
		sigItem: sigItem, pubkeyItem: pubkeyItem,
		coinbaseBytes: coinbaseBytes,
		scriptSig0:    scriptSig0, scriptSig1: scriptSig1,
	}
}

// F: transaction by txid.
func TestTransactionByTxID(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildTxFixture(t, ctx, st)

	got, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}
	if got.TxID != f.spendTxid || got.WTxID != f.spendWtxid {
		t.Fatalf("got (txid=%s wtxid=%s), want (txid=%s wtxid=%s)", got.TxID, got.WTxID, f.spendTxid, f.spendWtxid)
	}
	if got.IsCoinbase {
		t.Fatalf("IsCoinbase = true, want false")
	}
	if len(got.Occurrences) != 1 || !got.Occurrences[0].Canonical || got.Occurrences[0].BlockHash != f.block2.Hash {
		t.Fatalf("Occurrences = %+v", got.Occurrences)
	}

	if _, err := q.TransactionByTxID(ctx, fakeHash("does-not-exist"), false); err != ErrNotFound {
		t.Fatalf("TransactionByTxID(missing) err = %v, want ErrNotFound", err)
	}
}

// G: transaction by wtxid (distinct from txid).
func TestTransactionByWTxID(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildTxFixture(t, ctx, st)

	if f.spendTxid == f.spendWtxid {
		t.Fatalf("fixture invalid: txid must differ from wtxid for a witness-bearing tx")
	}

	got, err := q.TransactionByWTxID(ctx, f.spendWtxid, false)
	if err != nil {
		t.Fatalf("TransactionByWTxID: %v", err)
	}
	if got.TxID != f.spendTxid || got.WTxID != f.spendWtxid {
		t.Fatalf("got (txid=%s wtxid=%s), want (txid=%s wtxid=%s)", got.TxID, got.WTxID, f.spendTxid, f.spendWtxid)
	}

	if _, err := q.TransactionByWTxID(ctx, fakeHash("does-not-exist"), false); err != ErrNotFound {
		t.Fatalf("TransactionByWTxID(missing) err = %v, want ErrNotFound", err)
	}
}

// H: transaction input ordering.
func TestTransaction_InputOrdering(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildTxFixture(t, ctx, st)

	got, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}
	if len(got.Inputs) != 2 {
		t.Fatalf("len(Inputs) = %d, want 2", len(got.Inputs))
	}
	if got.Inputs[0].VinIndex != 0 || got.Inputs[0].PrevVoutIndex == nil || *got.Inputs[0].PrevVoutIndex != 0 {
		t.Fatalf("Inputs[0] = %+v, want vin=0 prevVout=0", got.Inputs[0])
	}
	if got.Inputs[1].VinIndex != 1 || got.Inputs[1].PrevVoutIndex == nil || *got.Inputs[1].PrevVoutIndex != 1 {
		t.Fatalf("Inputs[1] = %+v, want vin=1 prevVout=1", got.Inputs[1])
	}
	if got.Inputs[0].PrevTxID == nil || *got.Inputs[0].PrevTxID != f.block1.Transactions[0].TxID {
		t.Fatalf("Inputs[0].PrevTxID = %v, want %s", got.Inputs[0].PrevTxID, f.block1.Transactions[0].TxID)
	}
}

// I: output ordering.
func TestTransaction_OutputOrdering(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildTxFixture(t, ctx, st)

	got, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}
	if len(got.Outputs) != 2 {
		t.Fatalf("len(Outputs) = %d, want 2", len(got.Outputs))
	}
	if got.Outputs[0].VoutIndex != 0 || got.Outputs[0].ValueSatoshis != 15_00000000 {
		t.Fatalf("Outputs[0] = %+v", got.Outputs[0])
	}
	if got.Outputs[1].VoutIndex != 1 || got.Outputs[1].ValueSatoshis != 34_00000000 {
		t.Fatalf("Outputs[1] = %+v", got.Outputs[1])
	}
	if got.Outputs[0].ScriptType != string(script.TypeP2WPKH) {
		t.Fatalf("Outputs[0].ScriptType = %s, want p2wpkh", got.Outputs[0].ScriptType)
	}
	if got.Outputs[0].WitnessVersion == nil || *got.Outputs[0].WitnessVersion != 0 {
		t.Fatalf("Outputs[0].WitnessVersion = %v, want 0", got.Outputs[0].WitnessVersion)
	}
	if got.Outputs[0].WitnessProgram == nil || len(*got.Outputs[0].WitnessProgram) != 40 { // 20 bytes hex-encoded
		t.Fatalf("Outputs[0].WitnessProgram = %v, want 40 hex chars", got.Outputs[0].WitnessProgram)
	}
}

// J: coinbase raw bytes preserved.
func TestTransaction_CoinbaseBytesPreserved(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildTxFixture(t, ctx, st)

	got, err := q.TransactionByTxID(ctx, f.block1.Transactions[0].TxID, false)
	if err != nil {
		t.Fatalf("TransactionByTxID(coinbase): %v", err)
	}
	if !got.IsCoinbase {
		t.Fatalf("IsCoinbase = false, want true")
	}
	if len(got.Inputs) != 1 || got.Inputs[0].CoinbaseHex == nil {
		t.Fatalf("Inputs = %+v, want one input with CoinbaseHex set", got.Inputs)
	}
	if *got.Inputs[0].CoinbaseHex != hex.EncodeToString(f.coinbaseBytes) {
		t.Fatalf("CoinbaseHex = %s, want %s", *got.Inputs[0].CoinbaseHex, hex.EncodeToString(f.coinbaseBytes))
	}
}

// K: ordinary scriptSig preserved.
func TestTransaction_ScriptSigPreserved(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildTxFixture(t, ctx, st)

	got, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}
	if got.Inputs[0].ScriptSigHex != hex.EncodeToString(f.scriptSig0) {
		t.Fatalf("Inputs[0].ScriptSigHex = %s, want %s", got.Inputs[0].ScriptSigHex, hex.EncodeToString(f.scriptSig0))
	}
	if got.Inputs[1].ScriptSigHex != hex.EncodeToString(f.scriptSig1) {
		t.Fatalf("Inputs[1].ScriptSigHex = %s, want %s", got.Inputs[1].ScriptSigHex, hex.EncodeToString(f.scriptSig1))
	}
}

// L, M: exact integer satoshi values and exact QOGE decimal strings.
func TestTransaction_ExactMoneyRepresentation(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildTxFixture(t, ctx, st)

	got, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}
	if got.FeeSatoshis == nil || *got.FeeSatoshis != 1_00000000 {
		t.Fatalf("FeeSatoshis = %v, want 100000000", got.FeeSatoshis)
	}
	if got.FeeQOGE == nil || *got.FeeQOGE != "1.00000000" {
		t.Fatalf("FeeQOGE = %v, want 1.00000000", got.FeeQOGE)
	}
	if got.Outputs[0].ValueSatoshis != 15_00000000 || got.Outputs[0].ValueQOGE != "15.00000000" {
		t.Fatalf("Outputs[0] money = (%d, %s), want (1500000000, 15.00000000)", got.Outputs[0].ValueSatoshis, got.Outputs[0].ValueQOGE)
	}
	if got.Outputs[1].ValueSatoshis != 34_00000000 || got.Outputs[1].ValueQOGE != "34.00000000" {
		t.Fatalf("Outputs[1] money = (%d, %s), want (3400000000, 34.00000000)", got.Outputs[1].ValueSatoshis, got.Outputs[1].ValueQOGE)
	}

	cb, err := q.TransactionByTxID(ctx, f.block1.Transactions[0].TxID, false)
	if err != nil {
		t.Fatalf("TransactionByTxID(coinbase): %v", err)
	}
	if cb.FeeSatoshis != nil {
		t.Fatalf("coinbase FeeSatoshis = %v, want nil", cb.FeeSatoshis)
	}
}

// N: canonical spent/unspent status.
func TestTransaction_SpentUnspentStatus(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildTxFixture(t, ctx, st)

	cb1, err := q.TransactionByTxID(ctx, f.block1.Transactions[0].TxID, false)
	if err != nil {
		t.Fatalf("TransactionByTxID(block1 coinbase): %v", err)
	}
	for i, out := range cb1.Outputs {
		if out.Spent == nil || !*out.Spent {
			t.Fatalf("block1 coinbase output %d Spent = %v, want true (both spent by block2's spendTx)", i, out.Spent)
		}
	}

	spend, err := q.TransactionByTxID(ctx, f.spendTxid, false)
	if err != nil {
		t.Fatalf("TransactionByTxID(spend): %v", err)
	}
	for i, out := range spend.Outputs {
		if out.Spent == nil || *out.Spent {
			t.Fatalf("spendTx output %d Spent = %v, want false (unspent)", i, out.Spent)
		}
	}

	gen, err := q.TransactionByTxID(ctx, f.genesis.Transactions[0].TxID, false)
	if err != nil {
		t.Fatalf("TransactionByTxID(genesis coinbase): %v", err)
	}
	if gen.Outputs[0].Spent != nil {
		t.Fatalf("genesis output Spent = %v, want nil (never tracked in utxo_state)", gen.Outputs[0].Spent)
	}
}
