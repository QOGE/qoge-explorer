package indexer

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// TestSyncToTip_P2QPKPipeline exercises the REAL orchestration path — fake
// RPC -> Indexer -> rpc.RawBlock -> decode.DecodeBlock -> Store.ApplyBlock
// -> PostgreSQL — with a synthetic transaction carrying an exact
// 17,088-byte SLH-DSA-shaped witness item 0 and a 32-byte witness item 1,
// spending into a structural P2QPK output (witness v2, 32-byte program,
// Core RPC type "witness_unknown", Core-provided address). The indexer
// itself never inspects or transforms the witness bytes; this proves
// byte-exact persistence end to end (task item 22).
func TestSyncToTip_P2QPKPipeline(t *testing.T) {
	ctx := context.Background()
	st, pool := newTestStore(t)

	g := buildBlock("PQ-g", 0, "")
	block1 := buildBlock("PQ-1", 1, g.hash) // ordinary P2PKH coinbase, spent below
	prevTxid := *block1.txs[0].TxID

	sigItem := bytes.Repeat([]byte{0xab}, script.P2QPKSignatureLength)    // exactly 17,088 bytes
	pubKeyItem := bytes.Repeat([]byte{0xcd}, script.P2QPKPublicKeyLength) // exactly 32 bytes
	program := bytes.Repeat([]byte{0xef}, script.P2QPKProgramLength)      // exactly 32 bytes
	const p2qpkValue = 25_00000000

	coinbase2 := fakeCoinbaseTx("PQ-2-cb", 50_00000000)
	spendTx := p2qpkSpendTx("PQ-2-spend", prevTxid, 0, p2qpkValue, sigItem, pubKeyItem, program, "qP2QPKDestination")

	block2 := &fakeBlock{
		hash:     fakeHash("PQ-2"),
		prevHash: block1.hash,
		height:   2,
		txs:      []rpc.RawTransaction{coinbase2, spendTx},
	}

	fr := newFakeRPC()
	fr.setActiveChain(g, block1, block2)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("SyncToTip: %v", err)
	}

	tip, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != 2 || tip.Hash != block2.hash {
		t.Fatalf("tip = (%d, %s), want (2, %s)", tip.Height, tip.Hash, block2.hash)
	}

	spendTxid := *spendTx.TxID
	spendWtxid := *spendTx.Hash
	if spendTxid == spendWtxid {
		t.Fatalf("fixture is invalid: txid must differ from wtxid for a witness-bearing transaction")
	}

	// Byte-exact witness persistence, read directly from PostgreSQL.
	var item0, item1 []byte
	if err := pool.QueryRow(ctx,
		`SELECT data FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = 0 AND item_index = 0`,
		spendWtxid,
	).Scan(&item0); err != nil {
		t.Fatalf("read witness item 0: %v", err)
	}
	if len(item0) != script.P2QPKSignatureLength {
		t.Errorf("witness item 0 length = %d, want %d", len(item0), script.P2QPKSignatureLength)
	}
	if !bytes.Equal(item0, sigItem) {
		t.Errorf("witness item 0 not byte-exact")
	}

	if err := pool.QueryRow(ctx,
		`SELECT data FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = 0 AND item_index = 1`,
		spendWtxid,
	).Scan(&item1); err != nil {
		t.Fatalf("read witness item 1: %v", err)
	}
	if len(item1) != script.P2QPKPublicKeyLength {
		t.Errorf("witness item 1 length = %d, want %d", len(item1), script.P2QPKPublicKeyLength)
	}
	if !bytes.Equal(item1, pubKeyItem) {
		t.Errorf("witness item 1 not byte-exact")
	}

	// The P2QPK output itself: structural classification, witness
	// program, and Core-reported address preserved as-is.
	var scriptType string
	var witnessVersion *int
	var witnessProgram []byte
	if err := pool.QueryRow(ctx,
		`SELECT script_type, witness_version, witness_program FROM transaction_outputs WHERE txid = $1 AND vout_index = 0`,
		spendTxid,
	).Scan(&scriptType, &witnessVersion, &witnessProgram); err != nil {
		t.Fatalf("read p2qpk output: %v", err)
	}
	if scriptType != string(script.TypeP2QPK) {
		t.Errorf("script_type = %s, want %s", scriptType, script.TypeP2QPK)
	}
	if witnessVersion == nil || *witnessVersion != script.P2QPKWitnessVersion {
		t.Errorf("witness_version = %v, want %d", witnessVersion, script.P2QPKWitnessVersion)
	}
	if !bytes.Equal(witnessProgram, program) {
		t.Errorf("witness_program not byte-exact")
	}

	var addr string
	if err := pool.QueryRow(ctx,
		`SELECT address FROM output_addresses WHERE txid = $1 AND vout_index = 0`, spendTxid,
	).Scan(&addr); err != nil {
		t.Fatalf("read p2qpk output address: %v", err)
	}
	if addr != "qP2QPKDestination" {
		t.Errorf("address = %s, want qP2QPKDestination", addr)
	}
}

// p2qpkSpendTx builds a single-input, single-output RawTransaction whose
// input carries an exact two-item witness stack (signature, pubkey) and
// whose output is a structural P2QPK destination — witness version 2, a
// 32-byte program, and a Core-reported address (the shape Core's RPC
// actually emits for a witness_unknown output ExtractDestination
// succeeds for).
func p2qpkSpendTx(label, prevTxid string, prevVout uint32, valueSatoshis int64, sigItem, pubKeyItem, program []byte, address string) rpc.RawTransaction {
	txid := fakeHash(label + "-tx")
	wtxid := fakeHash(label + "-wtx")
	version := uint32(2)
	size := 17200
	vsize := 4310
	weight := 17240
	locktime := uint32(0)
	sequence := uint32(0xffffffff)
	scriptSigHex := ""
	n := uint32(0)

	scriptPubKey := make([]byte, 0, 2+len(program))
	scriptPubKey = append(scriptPubKey, 0x52, byte(len(program))) // OP_2 <push 32>
	scriptPubKey = append(scriptPubKey, program...)
	scriptHex := hex.EncodeToString(scriptPubKey)
	addr := address

	return rpc.RawTransaction{
		TxID:     &txid,
		Hash:     &wtxid,
		Version:  &version,
		Size:     &size,
		VSize:    &vsize,
		Weight:   &weight,
		LockTime: &locktime,
		Vin: []rpc.RawVin{
			{
				TxID:        &prevTxid,
				Vout:        &prevVout,
				ScriptSig:   &rpc.RawScriptSig{Hex: &scriptSigHex},
				Sequence:    &sequence,
				TxInWitness: []string{hex.EncodeToString(sigItem), hex.EncodeToString(pubKeyItem)},
			},
		},
		Vout: []rpc.RawVout{
			{Value: qogeAmount(valueSatoshis), N: &n, ScriptPubKey: &rpc.RawScriptPubKey{Hex: &scriptHex, Type: "witness_unknown", Address: &addr}},
		},
	}
}
