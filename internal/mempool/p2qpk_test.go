package mempool

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// TestP2QPKAndP2TR_EndToEnd drives a truthful fixture through the full
// real pipeline — rpc.RawTransaction -> decode.DecodeTransaction ->
// mempool.Candidate -> Store.ReplaceSnapshot -> raw PostgreSQL
// verification — exactly as spec items 41/42/43 require. It covers, in
// one transaction:
//
//   - a P2QPK output (witness v2, 32-byte program) spent by a witness
//     stack shaped exactly like a real P2QPK spend: item 0 exactly
//     17,088 bytes (SLH-DSA signature), item 1 exactly 32 bytes
//     (public key) — never truncated, byte-exact on read-back.
//   - a P2TR negative control (witness v1, 32-byte program) — must
//     classify as p2tr, never p2qpk.
//   - txid != wtxid, guarded explicitly (a witness transaction's wtxid
//     must differ from its txid).
func TestP2QPKAndP2TR_EndToEnd(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	sigItem := bytes.Repeat([]byte{0xab}, script.P2QPKSignatureLength)
	pubkeyItem := bytes.Repeat([]byte{0xcd}, script.P2QPKPublicKeyLength)
	p2qpkProgram := bytes.Repeat([]byte{0xef}, script.P2QPKProgramLength)
	p2trProgram := bytes.Repeat([]byte{0x11}, 32)

	p2qpkAddr := "qP2QPKMempoolDest"
	p2trAddr := "qP2TRMempoolDest"

	prevTxid := fakeHash("p2qpk-prevout")

	raw := rawMempoolTx("p2qpk-e2e",
		[]rpc.RawVin{rawSpendVin(prevTxid, 0, "", hex.EncodeToString(sigItem), hex.EncodeToString(pubkeyItem))},
		[]rpc.RawVout{
			rawVout(0, 25_00000000, witnessProgramScript(script.P2QPKWitnessVersion, p2qpkProgram), "witness_unknown", &p2qpkAddr),
			rawVout(1, 10_00000000, witnessProgramScript(1, p2trProgram), "witness_v1_taproot", &p2trAddr),
		},
	)

	if *raw.TxID == *raw.Hash {
		t.Fatalf("fixture txid == wtxid (%s); this test requires a witness transaction where they differ", *raw.TxID)
	}

	txn, err := decode.DecodeTransaction(ctx, raw, fakeAddressResolver{})
	if err != nil {
		t.Fatalf("DecodeTransaction: %v", err)
	}
	if txn.TxID == txn.WTxID {
		t.Fatalf("decoded txid == wtxid (%s), want distinct", txn.TxID)
	}

	candidateTx := CandidateTransaction{
		Transaction: txn,
		FeeSatoshis: 10_00000000,
		EntryTime:   1_700_000_000,
		EntryHeight: i64Ptr(500),
		Replaceable: boolPtr(true),
	}
	if _, err := mstore.ReplaceSnapshot(ctx, candidateOf(500, fakeHash("p2qpk-e2e-tip"), candidateTx)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// Raw DB verification: txid and wtxid persisted distinctly and
	// exactly.
	var gotTxid, gotWtxid string
	if err := pool.QueryRow(ctx, `SELECT txid, wtxid FROM mempool_transactions WHERE txid = $1`, txn.TxID).Scan(&gotTxid, &gotWtxid); err != nil {
		t.Fatalf("read mempool_transactions: %v", err)
	}
	if gotTxid != txn.TxID || gotWtxid != txn.WTxID || gotTxid == gotWtxid {
		t.Fatalf("persisted txid=%s wtxid=%s, want distinct and matching decoded values (%s / %s)", gotTxid, gotWtxid, txn.TxID, txn.WTxID)
	}

	// P2QPK output: script_type, witness_version, witness_program exact.
	var scriptType string
	var witnessVersion int
	var witnessProgram []byte
	if err := pool.QueryRow(ctx, `SELECT script_type, witness_version, witness_program FROM mempool_outputs WHERE txid = $1 AND vout_index = 0`, txn.TxID).
		Scan(&scriptType, &witnessVersion, &witnessProgram); err != nil {
		t.Fatalf("read p2qpk output: %v", err)
	}
	if scriptType != string(script.TypeP2QPK) {
		t.Fatalf("output 0 script_type = %s, want p2qpk", scriptType)
	}
	if witnessVersion != script.P2QPKWitnessVersion || !bytes.Equal(witnessProgram, p2qpkProgram) {
		t.Fatalf("output 0 witness_version=%d witness_program=%x, want version=%d program=%x", witnessVersion, witnessProgram, script.P2QPKWitnessVersion, p2qpkProgram)
	}

	// P2TR negative control: must be p2tr, not p2qpk.
	var trScriptType string
	if err := pool.QueryRow(ctx, `SELECT script_type FROM mempool_outputs WHERE txid = $1 AND vout_index = 1`, txn.TxID).Scan(&trScriptType); err != nil {
		t.Fatalf("read p2tr output: %v", err)
	}
	if trScriptType != string(script.TypeP2TR) {
		t.Fatalf("output 1 script_type = %s, want p2tr", trScriptType)
	}

	// Witness stack: byte-exact, ordering preserved, never truncated.
	rows, err := pool.Query(ctx, `SELECT item_index, data FROM mempool_input_witness WHERE txid = $1 AND vin_index = 0 ORDER BY item_index`, txn.TxID)
	if err != nil {
		t.Fatalf("query witness: %v", err)
	}
	defer rows.Close()
	var items [][]byte
	for rows.Next() {
		var idx int
		var data []byte
		if err := rows.Scan(&idx, &data); err != nil {
			t.Fatalf("scan witness row: %v", err)
		}
		if idx != len(items) {
			t.Fatalf("witness item_index = %d, want %d (ordering must be preserved)", idx, len(items))
		}
		items = append(items, data)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate witness rows: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("witness item count = %d, want 2", len(items))
	}
	if len(items[0]) != script.P2QPKSignatureLength || !bytes.Equal(items[0], sigItem) {
		t.Fatalf("witness item 0 length=%d, want %d bytes exactly matching the fixture signature", len(items[0]), script.P2QPKSignatureLength)
	}
	if len(items[1]) != script.P2QPKPublicKeyLength || !bytes.Equal(items[1], pubkeyItem) {
		t.Fatalf("witness item 1 length=%d, want %d bytes exactly matching the fixture pubkey", len(items[1]), script.P2QPKPublicKeyLength)
	}

	// The 32-byte P2QPK output witness program is NOT the public key —
	// confirm they are actually different byte sequences in this fixture
	// (guards against accidentally testing a tautology).
	if bytes.Equal(p2qpkProgram, pubkeyItem) {
		t.Fatalf("fixture bug: p2qpkProgram must differ from pubkeyItem (program is a commitment, not the raw key)")
	}
}
