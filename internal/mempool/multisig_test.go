package mempool

import (
	"bytes"
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// TestMultisig_ParticipantsNotCreditedAsDestination is spec item 44: a
// bare multisig output must produce NO mempool_output_addresses row (no
// single balance-accounting destination), and its participant identities
// must be stored separately in mempool_output_participants — never
// double-counted.
func TestMultisig_ParticipantsNotCreditedAsDestination(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	pub1 := compressedPubKey("multisig-1")
	pub2 := compressedPubKey("multisig-2")
	msScript := multisigScript([][]byte{pub1, pub2})

	prevTxid := fakeHash("multisig-prevout")
	raw := rawMempoolTx("multisig-e2e",
		[]rpc.RawVin{rawSpendVin(prevTxid, 0, "473044")},
		[]rpc.RawVout{rawVout(0, 5_00000000, msScript, "multisig", nil)},
	)

	txn, err := decode.DecodeTransaction(ctx, raw, fakeAddressResolver{})
	if err != nil {
		t.Fatalf("DecodeTransaction: %v", err)
	}
	if txn.Outputs[0].ScriptType != script.TypeMultisig {
		t.Fatalf("output script_type = %s, want multisig", txn.Outputs[0].ScriptType)
	}
	if len(txn.Outputs[0].ParticipantAddresses) != 2 {
		t.Fatalf("participant addresses = %d, want 2", len(txn.Outputs[0].ParticipantAddresses))
	}

	candidateTx := CandidateTransaction{
		Transaction: txn,
		FeeSatoshis: 1000,
		EntryTime:   1_700_000_000,
		EntryHeight: i64Ptr(200),
		Replaceable: boolPtr(false),
	}
	if _, err := mstore.ReplaceSnapshot(ctx, candidateOf(200, fakeHash("multisig-tip"), candidateTx)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	var addressRowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mempool_output_addresses WHERE txid = $1`, txn.TxID).Scan(&addressRowCount); err != nil {
		t.Fatalf("count mempool_output_addresses: %v", err)
	}
	if addressRowCount != 0 {
		t.Fatalf("mempool_output_addresses rows for multisig output = %d, want 0 (no balance-accounting destination)", addressRowCount)
	}

	var participantCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mempool_output_participants WHERE txid = $1`, txn.TxID).Scan(&participantCount); err != nil {
		t.Fatalf("count mempool_output_participants: %v", err)
	}
	if participantCount != 2 {
		t.Fatalf("mempool_output_participants rows = %d, want 2", participantCount)
	}

	var pubkeysStored [][]byte
	rows, err := pool.Query(ctx, `SELECT pubkey FROM mempool_output_participants WHERE txid = $1 ORDER BY address`, txn.TxID)
	if err != nil {
		t.Fatalf("query participant pubkeys: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pk []byte
		if err := rows.Scan(&pk); err != nil {
			t.Fatalf("scan pubkey: %v", err)
		}
		pubkeysStored = append(pubkeysStored, pk)
	}
	if len(pubkeysStored) != 2 {
		t.Fatalf("stored pubkeys = %d, want 2", len(pubkeysStored))
	}
	for _, pk := range pubkeysStored {
		if !bytes.Equal(pk, pub1) && !bytes.Equal(pk, pub2) {
			t.Fatalf("stored pubkey %x matches neither fixture pubkey", pk)
		}
	}
}

// TestOutputs_ZeroValueAndOpReturnPersisted is spec item 45: mempool
// output persistence describes the transaction, not confirmed UTXO
// spendability — zero-value and OP_RETURN outputs must be persisted like
// any other, never dropped, and must never create any confirmed
// utxo_state row (there is no such table write path in this package at
// all).
func TestOutputs_ZeroValueAndOpReturnPersisted(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	addr := "qOpReturnFixtureDest"
	prevTxid := fakeHash("opreturn-prevout")
	largeData := bytes.Repeat([]byte{0x42}, 80) // realistic large OP_RETURN payload

	raw := rawMempoolTx("opreturn-e2e",
		[]rpc.RawVin{rawSpendVin(prevTxid, 0, "473044")},
		[]rpc.RawVout{
			rawVout(0, 0, nullDataScript(largeData), "nulldata", nil),
			rawVout(1, 1_00000000, p2pkhScript("opreturn-change"), "pubkeyhash", &addr),
		},
	)

	txn, err := decode.DecodeTransaction(ctx, raw, fakeAddressResolver{})
	if err != nil {
		t.Fatalf("DecodeTransaction: %v", err)
	}

	candidateTx := CandidateTransaction{
		Transaction: txn,
		FeeSatoshis: 500,
		EntryTime:   1_700_000_000,
		EntryHeight: i64Ptr(300),
		Replaceable: boolPtr(true),
	}
	if _, err := mstore.ReplaceSnapshot(ctx, candidateOf(300, fakeHash("opreturn-tip"), candidateTx)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	var outputCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mempool_outputs WHERE txid = $1`, txn.TxID).Scan(&outputCount); err != nil {
		t.Fatalf("count mempool_outputs: %v", err)
	}
	if outputCount != 2 {
		t.Fatalf("mempool_outputs rows = %d, want 2 (OP_RETURN output must not be dropped)", outputCount)
	}

	var value int64
	var scriptType string
	var scriptPubKey []byte
	if err := pool.QueryRow(ctx, `SELECT value_satoshis, script_type, script_pubkey FROM mempool_outputs WHERE txid = $1 AND vout_index = 0`, txn.TxID).
		Scan(&value, &scriptType, &scriptPubKey); err != nil {
		t.Fatalf("read OP_RETURN output: %v", err)
	}
	if value != 0 {
		t.Fatalf("OP_RETURN value_satoshis = %d, want 0", value)
	}
	if scriptType != string(script.TypeNullData) {
		t.Fatalf("OP_RETURN script_type = %s, want nulldata", scriptType)
	}
	if !bytes.Equal(scriptPubKey, nullDataScript(largeData)) {
		t.Fatalf("OP_RETURN script_pubkey not byte-exact")
	}

	// No confirmed utxo_state row is ever created for a mempool output —
	// this package never writes to that table at all (see
	// confirmed_nonmutation_test.go for the exhaustive version of this
	// check); a targeted spot-check here confirms it for this specific
	// txid too.
	var utxoCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM utxo_state WHERE txid = $1`, txn.TxID).Scan(&utxoCount); err != nil {
		t.Fatalf("count utxo_state: %v", err)
	}
	if utxoCount != 0 {
		t.Fatalf("utxo_state rows for mempool txid = %d, want 0", utxoCount)
	}
}
