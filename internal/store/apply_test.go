package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── fixture builders ───────────────────────────────────────────────────
//
// These deliberately use minimal, synthetic-but-schema-valid data (same
// spirit as internal/store/invariants_test.go's fixtureXxx helpers) — the
// point of these tests is proving Store's atomicity/idempotency/conflict/
// reorg behavior, not re-testing script classification or real chain
// history (that's internal/script and internal/chain's job).

func testBlock(hash string, height int64, prevHash string, txs ...chain.Transaction) chain.Block {
	return chain.Block{
		Hash:         hash,
		Height:       height,
		PreviousHash: prevHash,
		MerkleRoot:   hash,
		Time:         1700000000 + height,
		Bits:         "1d00ffff",
		Difficulty:   1.0,
		Nonce:        uint32(height),
		Size:         100,
		Weight:       400,
		TxCount:      len(txs),
		Transactions: txs,
	}
}

func coinbaseTx(txid string, outputs ...chain.Output) chain.Transaction {
	return chain.Transaction{
		TxID:       txid,
		WTxID:      txid,
		Version:    2,
		LockTime:   0,
		Size:       100,
		VSize:      100,
		Weight:     400,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 4294967295},
		},
		Outputs: outputs,
	}
}

func spendTx(txid, wtxid string, size int, inputs []chain.Input, outputs []chain.Output) chain.Transaction {
	return chain.Transaction{
		TxID:       txid,
		WTxID:      wtxid,
		Version:    2,
		LockTime:   0,
		Size:       size,
		VSize:      size,
		Weight:     size * 4,
		IsCoinbase: false,
		Inputs:     inputs,
		Outputs:    outputs,
	}
}

func spendInput(index uint32, prevTxid string, prevVout uint32, witness chain.WitnessStack) chain.Input {
	return chain.Input{
		Index:       index,
		PreviousOut: &chain.OutPoint{TxID: prevTxid, Index: prevVout},
		ScriptSig:   []byte{},
		Sequence:    4294967295,
		Witness:     witness,
	}
}

func out(index uint32, value int64, addr string) chain.Output {
	return chain.Output{
		Index:        index,
		Value:        chain.Amount(value),
		ScriptPubKey: []byte{0x00},
		ScriptType:   script.TypeP2PKH,
		Address:      addr,
	}
}

func nullOut(index uint32) chain.Output {
	return chain.Output{
		Index:        index,
		Value:        0,
		ScriptPubKey: []byte{0x6a},
		ScriptType:   script.TypeNullData,
	}
}

func p2qpkOut(index uint32, value int64) chain.Output {
	v := script.P2QPKWitnessVersion
	return chain.Output{
		Index:          index,
		Value:          chain.Amount(value),
		ScriptPubKey:   []byte{0x52, 0x20},
		ScriptType:     script.TypeP2QPK,
		WitnessVersion: &v,
		WitnessProgram: bytes.Repeat([]byte{0xab}, script.P2QPKProgramLength),
	}
}

func multisigOut(index uint32, value int64, pubkeys [][]byte, addrs []string) chain.Output {
	return chain.Output{
		Index:                index,
		Value:                chain.Amount(value),
		ScriptPubKey:         []byte{0x51},
		ScriptType:           script.TypeMultisig,
		PubKeys:              pubkeys,
		ParticipantAddresses: addrs,
	}
}

func mustApply(t *testing.T, ctx context.Context, s *Store, block chain.Block) {
	t.Helper()
	if err := s.ApplyBlock(ctx, block); err != nil {
		t.Fatalf("ApplyBlock(%s): %v", block.Hash, err)
	}
}

func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := migratedPool(t)
	return New(pool), pool
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test hex %q: %v", s, err)
	}
	return b
}

// ─── A: single block applies atomically ────────────────────────────────

func TestApplyBlock_SingleBlockAtomic(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("blockA"), 100, "",
		coinbaseTx(hash64("txA"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, block)

	for _, q := range []struct {
		table string
		where string
		arg   string
	}{
		{"blocks", "hash = $1", hash64("blockA")},
		{"transactions", "txid = $1", hash64("txA")},
		{"transaction_variants", "wtxid = $1", hash64("txA")},
		{"block_transactions", "block_hash = $1", hash64("blockA")},
		{"transaction_outputs", "txid = $1", hash64("txA")},
		{"utxo_state", "txid = $1", hash64("txA")},
		{"output_addresses", "txid = $1", hash64("txA")},
	} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+q.table+" WHERE "+q.where, q.arg).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", q.table, err)
		}
		if count != 1 {
			t.Errorf("%s: count = %d, want 1", q.table, count)
		}
	}

	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != hash64("blockA") || tip.Height != 100 {
		t.Errorf("Tip = %+v, want hash=%s height=100", tip, hash64("blockA"))
	}
}

// ─── B, C, R: idempotent replay ────────────────────────────────────────

func TestApplyBlock_IdempotentReplay(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("blockB"), 100, "",
		coinbaseTx(hash64("txB"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, block)

	var before struct {
		received, sent, balance, txCount int64
	}
	if err := pool.QueryRow(ctx, `SELECT total_received_satoshis, total_sent_satoshis, balance_satoshis, tx_count FROM addresses WHERE address = 'qAlice'`).
		Scan(&before.received, &before.sent, &before.balance, &before.txCount); err != nil {
		t.Fatalf("read address before replay: %v", err)
	}

	// B: exact same block replay is idempotent.
	if err := s.ApplyBlock(ctx, block); err != nil {
		t.Fatalf("replay ApplyBlock: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transaction_outputs WHERE txid = $1`, hash64("txB")).Scan(&count); err != nil {
		t.Fatalf("count outputs: %v", err)
	}
	if count != 1 {
		t.Errorf("transaction_outputs count after replay = %d, want 1 (no duplication)", count)
	}

	// C: replay does not increase address balances/counts. R: cache after
	// replay equals cache before replay exactly.
	var after struct {
		received, sent, balance, txCount int64
	}
	if err := pool.QueryRow(ctx, `SELECT total_received_satoshis, total_sent_satoshis, balance_satoshis, tx_count FROM addresses WHERE address = 'qAlice'`).
		Scan(&after.received, &after.sent, &after.balance, &after.txCount); err != nil {
		t.Fatalf("read address after replay: %v", err)
	}
	if after != before {
		t.Errorf("address cache changed on replay: before=%+v after=%+v", before, after)
	}
	if after.received != 5_000_000_000 || after.balance != 5_000_000_000 || after.txCount != 1 {
		t.Errorf("unexpected address cache values: %+v", after)
	}
}

// ─── D, E, F: contradictory immutable data is rejected ─────────────────

func TestApplyBlock_ImmutableConflicts(t *testing.T) {
	ctx := context.Background()

	t.Run("D: contradictory block header data", func(t *testing.T) {
		s, pool := newTestStore(t)
		block := testBlock(hash64("blockD"), 100, "",
			coinbaseTx(hash64("txD1"), out(0, 1_000_000_000, "qAlice")),
		)
		mustApply(t, ctx, s, block)

		conflicting := block
		conflicting.Height = 101 // same hash, different height
		err := s.ApplyBlock(ctx, conflicting)
		if err == nil {
			t.Fatal("expected a contradictory block header to be rejected, got nil")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}

		var height int64
		if err := pool.QueryRow(ctx, `SELECT height FROM blocks WHERE hash = $1`, hash64("blockD")).Scan(&height); err != nil {
			t.Fatalf("read back block height: %v", err)
		}
		if height != 100 {
			t.Errorf("original block height was overwritten: got %d, want 100", height)
		}
	})

	t.Run("E: contradictory txid body", func(t *testing.T) {
		s, _ := newTestStore(t)
		g1 := testBlock(hash64("blockE1"), 200, "",
			coinbaseTx(hash64("txE"), out(0, 1_000_000_000, "qAlice")),
		)
		mustApply(t, ctx, s, g1)

		g2tx := coinbaseTx(hash64("txE"), out(0, 1_000_000_000, "qAlice"))
		g2tx.Version = 3 // same txid, different version -> contradictory body
		g2 := testBlock(hash64("blockE2"), 201, hash64("blockE1"), g2tx)

		err := s.ApplyBlock(ctx, g2)
		if err == nil {
			t.Fatal("expected a contradictory txid body to be rejected, got nil")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("F: contradictory wtxid variant", func(t *testing.T) {
		s, _ := newTestStore(t)
		h1tx := coinbaseTx(hash64("txF"), out(0, 1_000_000_000, "qAlice"))
		h1tx.Size = 100
		h1 := testBlock(hash64("blockF1"), 300, "", h1tx)
		mustApply(t, ctx, s, h1)

		h2tx := coinbaseTx(hash64("txF"), out(0, 1_000_000_000, "qAlice"))
		h2tx.Size = 999 // same txid AND wtxid, different variant metrics
		h2 := testBlock(hash64("blockF2"), 301, hash64("blockF1"), h2tx)

		err := s.ApplyBlock(ctx, h2)
		if err == nil {
			t.Fatal("expected a contradictory wtxid variant to be rejected, got nil")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
	})
}

// ─── G: zero-value nulldata output survives ────────────────────────────

func TestApplyBlock_ZeroValueNullData(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("blockG"), 100, "",
		coinbaseTx(hash64("txG"), out(0, 1_000_000_000, "qAlice"), nullOut(1)),
	)
	mustApply(t, ctx, s, block)

	var value int64
	var scriptType string
	if err := pool.QueryRow(ctx, `SELECT value_satoshis, script_type FROM transaction_outputs WHERE txid = $1 AND vout_index = 1`, hash64("txG")).
		Scan(&value, &scriptType); err != nil {
		t.Fatalf("read back nulldata output: %v", err)
	}
	if value != 0 {
		t.Errorf("value_satoshis = %d, want 0", value)
	}
	if scriptType != "nulldata" {
		t.Errorf("script_type = %s, want nulldata", scriptType)
	}
}

// ─── H: 17,088-byte P2QPK witness survives through ApplyBlock ──────────

func TestApplyBlock_P2QPKWitnessPreservation(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	sig := bytes.Repeat([]byte{0xcd}, script.P2QPKSignatureLength)
	pub := bytes.Repeat([]byte{0xef}, script.P2QPKPublicKeyLength)

	g := testBlock(hash64("blockH1"), 100, "",
		coinbaseTx(hash64("txH1"), p2qpkOut(0, 5_000_000_000)),
	)
	mustApply(t, ctx, s, g)

	spend := spendTx(hash64("txH2"), hash64("txH2w"), 17200,
		[]chain.Input{spendInput(0, hash64("txH1"), 0, chain.WitnessStack{sig, pub})},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	a := testBlock(hash64("blockH2"), 101, hash64("blockH1"), spend)
	mustApply(t, ctx, s, a)

	var data []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = 0 AND item_index = 0`, hash64("txH2w")).Scan(&data); err != nil {
		t.Fatalf("read back witness item 0: %v", err)
	}
	if len(data) != script.P2QPKSignatureLength || !bytes.Equal(data, sig) {
		t.Errorf("witness item 0: len=%d (want %d), equal=%t", len(data), script.P2QPKSignatureLength, bytes.Equal(data, sig))
	}

	var pubData []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = 0 AND item_index = 1`, hash64("txH2w")).Scan(&pubData); err != nil {
		t.Fatalf("read back witness item 1: %v", err)
	}
	if !bytes.Equal(pubData, pub) {
		t.Error("witness item 1 (pubkey) did not round-trip exactly")
	}
}

// ─── I: two wtxid variants sharing one txid remain distinct ────────────
//
// The only way to legitimately observe two wtxid variants of the same
// txid through the Store API is via a reorg: block A is applied with
// txid T / wtxid W1, then rolled back, then a replacement block A' at the
// same height applies the SAME non-witness body (txid T, identical
// inputs/outputs) under a DIFFERENT witness (wtxid W2). Both occurrences
// remain queryable afterward — see docs/ARCHITECTURE.md §16.

func TestApplyBlock_WtxidVariantsRemainDistinct(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("blockI0"), 100, "",
		coinbaseTx(hash64("txI0"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, g)

	w1data := []byte{0xde, 0xad, 0xbe, 0xef}
	spendA := spendTx(hash64("txI1"), hash64("txI1w1"), 200,
		[]chain.Input{spendInput(0, hash64("txI0"), 0, chain.WitnessStack{w1data})},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	blockA := testBlock(hash64("blockI1"), 101, hash64("blockI0"), spendA)
	mustApply(t, ctx, s, blockA)

	if err := s.RollbackTo(ctx, hash64("blockI0")); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	w2data := bytes.Repeat([]byte{0x99}, script.P2QPKSignatureLength)
	spendA2 := spendTx(hash64("txI1"), hash64("txI1w2"), 200,
		[]chain.Input{spendInput(0, hash64("txI0"), 0, chain.WitnessStack{w2data})},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	blockA2 := testBlock(hash64("blockI2"), 101, hash64("blockI0"), spendA2)
	mustApply(t, ctx, s, blockA2)

	var occurrences int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_transactions WHERE txid = $1`, hash64("txI1")).Scan(&occurrences); err != nil {
		t.Fatalf("count occurrences: %v", err)
	}
	if occurrences != 2 {
		t.Fatalf("block_transactions occurrences for txid = %d, want 2", occurrences)
	}

	var got1, got2 []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = 0 AND item_index = 0`, hash64("txI1w1")).Scan(&got1); err != nil {
		t.Fatalf("read back W1 witness: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = 0 AND item_index = 0`, hash64("txI1w2")).Scan(&got2); err != nil {
		t.Fatalf("read back W2 witness: %v", err)
	}
	if !bytes.Equal(got1, w1data) {
		t.Error("W1's witness was not preserved exactly")
	}
	if !bytes.Equal(got2, w2data) || len(got2) != script.P2QPKSignatureLength {
		t.Error("W2's witness was not preserved exactly")
	}
}

// ─── J: normal spend marks exact previous UTXO spent ───────────────────

func TestApplyBlock_SpendMarksExactUTXO(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("blockJ0"), 100, "",
		coinbaseTx(hash64("txJ0"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, g)

	spend := spendTx(hash64("txJ1"), hash64("txJ1"), 200,
		[]chain.Input{spendInput(0, hash64("txJ0"), 0, nil)},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	a := testBlock(hash64("blockJ1"), 101, hash64("blockJ0"), spend)
	mustApply(t, ctx, s, a)

	utxo, err := s.GetUTXO(ctx, hash64("txJ0"), 0)
	if err != nil {
		t.Fatalf("GetUTXO: %v", err)
	}
	if utxo == nil {
		t.Fatal("GetUTXO returned nil for a created output")
	}
	if !utxo.Spent {
		t.Fatal("expected spent = true")
	}
	if utxo.SpendingTxID != hash64("txJ1") || utxo.SpendingVinIndex != 0 || utxo.SpendingBlockHash != hash64("blockJ1") {
		t.Errorf("unexpected spend identity: %+v", utxo)
	}
}

// ─── K, P, Q: double-spend / mid-block failure rolls back the WHOLE block ─

func TestApplyBlock_DoubleSpendRejectsWholeBlock(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("blockK0"), 100, "",
		coinbaseTx(hash64("txK0"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, g)

	spend1 := spendTx(hash64("txK1"), hash64("txK1"), 200,
		[]chain.Input{spendInput(0, hash64("txK0"), 0, nil)},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	a := testBlock(hash64("blockK1"), 101, hash64("blockK0"), spend1)
	mustApply(t, ctx, s, a)

	// K: a second block attempting to spend the SAME already-spent UTXO
	// via a different transaction must fail entirely.
	spend2 := spendTx(hash64("txK2"), hash64("txK2"), 200,
		[]chain.Input{spendInput(0, hash64("txK0"), 0, nil)},
		[]chain.Output{out(0, 4_998_000_000, "qCarol")},
	)
	b := testBlock(hash64("blockK2"), 102, hash64("blockK1"), spend2)
	err := s.ApplyBlock(ctx, b)
	if err == nil {
		t.Fatal("expected double-spend to reject the whole block, got nil")
	}
	if !errors.Is(err, ErrDoubleSpend) {
		t.Errorf("error = %v, want ErrDoubleSpend", err)
	}

	// P: nothing from the failed block persisted.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, hash64("blockK2")).Scan(&count); err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if count != 0 {
		t.Errorf("failed block's header row persisted: count = %d, want 0", count)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transactions WHERE txid = $1`, hash64("txK2")).Scan(&count); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if count != 0 {
		t.Errorf("failed block's transaction row persisted: count = %d, want 0", count)
	}

	// Q: sync_state and canonical data are unchanged — the checkpoint is
	// still block K1, and K0's UTXO is still spent by exactly txK1, not
	// disturbed by the rejected attempt.
	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != hash64("blockK1") {
		t.Errorf("Tip.Hash = %s, want %s (checkpoint must not move on a failed apply)", tip.Hash, hash64("blockK1"))
	}
	utxo, err := s.GetUTXO(ctx, hash64("txK0"), 0)
	if err != nil {
		t.Fatalf("GetUTXO: %v", err)
	}
	if utxo == nil || !utxo.Spent || utxo.SpendingTxID != hash64("txK1") {
		t.Errorf("utxo_state for txK0:0 changed by the rejected block: %+v", utxo)
	}
}

// ─── L: nonexistent prevout rejects whole block ────────────────────────

func TestApplyBlock_MissingPrevoutRejectsWholeBlock(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	spend := spendTx(hash64("txL1"), hash64("txL1"), 200,
		[]chain.Input{spendInput(0, hash64("nosuchtx"), 0, nil)},
		[]chain.Output{out(0, 1_000_000_000, "qBob")},
	)
	block := testBlock(hash64("blockL1"), 100, "", spend)

	err := s.ApplyBlock(ctx, block)
	if err == nil {
		t.Fatal("expected a missing prevout to reject the whole block, got nil")
	}
	if !errors.Is(err, ErrMissingPrevout) {
		t.Errorf("error = %v, want ErrMissingPrevout", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, hash64("blockL1")).Scan(&count); err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if count != 0 {
		t.Errorf("failed block's header row persisted: count = %d, want 0", count)
	}
}

// ─── M, N: fee computation ──────────────────────────────────────────────

func TestApplyBlock_FeeComputation(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("blockM0"), 100, "",
		coinbaseTx(hash64("txM0"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, g)

	spend := spendTx(hash64("txM1"), hash64("txM1"), 200,
		[]chain.Input{spendInput(0, hash64("txM0"), 0, nil)},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	a := testBlock(hash64("blockM1"), 101, hash64("blockM0"), spend)
	mustApply(t, ctx, s, a)

	var fee *int64
	if err := pool.QueryRow(ctx, `SELECT fee_satoshis FROM transactions WHERE txid = $1`, hash64("txM1")).Scan(&fee); err != nil {
		t.Fatalf("read back fee: %v", err)
	}
	if fee == nil || *fee != 1_000_000 {
		t.Errorf("fee_satoshis = %v, want 1000000", fee)
	}

	var coinbaseFee *int64
	if err := pool.QueryRow(ctx, `SELECT fee_satoshis FROM transactions WHERE txid = $1`, hash64("txM0")).Scan(&coinbaseFee); err != nil {
		t.Fatalf("read back coinbase fee: %v", err)
	}
	if coinbaseFee != nil {
		t.Errorf("coinbase fee_satoshis = %v, want NULL", *coinbaseFee)
	}
}

// ─── O: multisig participants never multiply address balance ───────────

func TestApplyBlock_MultisigNoBalanceMultiplication(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	pub1 := bytes.Repeat([]byte{0x01}, 33)
	pub2 := bytes.Repeat([]byte{0x02}, 33)
	block := testBlock(hash64("blockO"), 100, "",
		coinbaseTx(hash64("txO"), multisigOut(0, 3_000_000_000, [][]byte{pub1, pub2}, []string{"qP1", "qP2"})),
	)
	mustApply(t, ctx, s, block)

	var participantCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM output_participants WHERE txid = $1`, hash64("txO")).Scan(&participantCount); err != nil {
		t.Fatalf("count participants: %v", err)
	}
	if participantCount != 2 {
		t.Errorf("output_participants count = %d, want 2", participantCount)
	}

	var addressRowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM output_addresses WHERE txid = $1`, hash64("txO")).Scan(&addressRowCount); err != nil {
		t.Fatalf("count output_addresses: %v", err)
	}
	if addressRowCount != 0 {
		t.Errorf("output_addresses count for multisig output = %d, want 0", addressRowCount)
	}

	for _, addr := range []string{"qP1", "qP2"} {
		var cacheCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM addresses WHERE address = $1`, addr).Scan(&cacheCount); err != nil {
			t.Fatalf("count addresses cache for %s: %v", addr, err)
		}
		if cacheCount != 0 {
			t.Errorf("addresses cache row exists for participant %s, want none (never balance-accounted)", addr)
		}
	}
}

// ─── S: simple one-block reorg restores previously spent UTXO ──────────

func TestRollbackTo_SimpleOneBlockReorg(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("blockS0"), 100, "",
		coinbaseTx(hash64("txS0"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, g)

	spend := spendTx(hash64("txS1"), hash64("txS1"), 200,
		[]chain.Input{spendInput(0, hash64("txS0"), 0, nil)},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	a := testBlock(hash64("blockS1"), 101, hash64("blockS0"), spend)
	mustApply(t, ctx, s, a)

	utxo, err := s.GetUTXO(ctx, hash64("txS0"), 0)
	if err != nil || utxo == nil || !utxo.Spent {
		t.Fatalf("precondition failed: txS0:0 should be spent, got %+v err=%v", utxo, err)
	}

	if err := s.RollbackTo(ctx, hash64("blockS0")); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	utxo, err = s.GetUTXO(ctx, hash64("txS0"), 0)
	if err != nil {
		t.Fatalf("GetUTXO after rollback: %v", err)
	}
	if utxo == nil {
		t.Fatal("expected txS0:0 to still exist after rollback (it was created before the rollback point)")
	}
	if utxo.Spent {
		t.Errorf("expected txS0:0 to be restored to unspent after rollback, got spent=true: %+v", utxo)
	}
	if utxo.SpendingTxID != "" {
		t.Errorf("expected spending fields cleared, got SpendingTxID=%q", utxo.SpendingTxID)
	}
}

// ─── T, U, V: multi-block reorg ─────────────────────────────────────────

func TestRollbackTo_MultiBlockReorg(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("blockT0"), 100, "",
		coinbaseTx(hash64("txT0"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, g)

	spendA := spendTx(hash64("txT1"), hash64("txT1"), 200,
		[]chain.Input{spendInput(0, hash64("txT0"), 0, nil)},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	blockA := testBlock(hash64("blockT1"), 101, hash64("blockT0"), spendA)
	mustApply(t, ctx, s, blockA)

	spendB := spendTx(hash64("txT2"), hash64("txT2"), 200,
		[]chain.Input{spendInput(0, hash64("txT1"), 0, nil)},
		[]chain.Output{out(0, 4_998_000_000, "qCarol")},
	)
	blockB := testBlock(hash64("blockT2"), 102, hash64("blockT1"), spendB)
	mustApply(t, ctx, s, blockB)

	if err := s.RollbackTo(ctx, hash64("blockT0")); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	// T: all canonical UTXOs correctly restored/removed.
	utxo0, err := s.GetUTXO(ctx, hash64("txT0"), 0)
	if err != nil || utxo0 == nil || utxo0.Spent {
		t.Errorf("txT0:0 should be unspent after rollback, got %+v err=%v", utxo0, err)
	}
	utxo1, err := s.GetUTXO(ctx, hash64("txT1"), 0)
	if err != nil {
		t.Fatalf("GetUTXO txT1: %v", err)
	}
	if utxo1 != nil {
		t.Errorf("txT1:0 (created only in the now-orphaned block T1) should be gone from utxo_state, got %+v", utxo1)
	}
	utxo2, err := s.GetUTXO(ctx, hash64("txT2"), 0)
	if err != nil {
		t.Fatalf("GetUTXO txT2: %v", err)
	}
	if utxo2 != nil {
		t.Errorf("txT2:0 (created only in the now-orphaned block T2) should be gone from utxo_state, got %+v", utxo2)
	}

	// U: orphan block/tx/witness/output audit data remains queryable.
	var canonical bool
	var orphanedAt *string
	if err := pool.QueryRow(ctx, `SELECT canonical, orphaned_at::text FROM blocks WHERE hash = $1`, hash64("blockT1")).Scan(&canonical, &orphanedAt); err != nil {
		t.Fatalf("read orphaned block T1: %v", err)
	}
	if canonical {
		t.Error("blockT1.canonical should be false after rollback")
	}
	if orphanedAt == nil {
		t.Error("blockT1.orphaned_at should be set after rollback")
	}
	var txCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transactions WHERE txid = $1`, hash64("txT1")).Scan(&txCount); err != nil {
		t.Fatalf("count orphaned tx: %v", err)
	}
	if txCount != 1 {
		t.Errorf("orphaned transaction txT1 should remain queryable, count = %d", txCount)
	}
	var outCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transaction_outputs WHERE txid = $1`, hash64("txT2")).Scan(&outCount); err != nil {
		t.Fatalf("count orphaned output: %v", err)
	}
	if outCount != 1 {
		t.Errorf("orphaned output txT2:0 should remain queryable, count = %d", outCount)
	}

	// V: sync_state ends at the common ancestor.
	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != hash64("blockT0") || tip.Height != 100 {
		t.Errorf("Tip = %+v, want hash=%s height=100", tip, hash64("blockT0"))
	}
}

// ─── W, X: replacement branch applies normally; address cache matches a
// fresh recomputation from canonical source tables ──────────────────────

func TestRollbackTo_ReplacementBranchAndAddressRecompute(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("blockW0"), 100, "",
		coinbaseTx(hash64("txW0"), out(0, 5_000_000_000, "qAlice")),
	)
	mustApply(t, ctx, s, g)

	spendOld := spendTx(hash64("txW1old"), hash64("txW1old"), 200,
		[]chain.Input{spendInput(0, hash64("txW0"), 0, nil)},
		[]chain.Output{out(0, 4_999_000_000, "qBobOld")},
	)
	blockOld := testBlock(hash64("blockW1old"), 101, hash64("blockW0"), spendOld)
	mustApply(t, ctx, s, blockOld)

	if err := s.RollbackTo(ctx, hash64("blockW0")); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	// W: replacement branch applies normally.
	spendNew := spendTx(hash64("txW1new"), hash64("txW1new"), 200,
		[]chain.Input{spendInput(0, hash64("txW0"), 0, nil)},
		[]chain.Output{out(0, 4_999_500_000, "qBobNew")},
	)
	blockNew := testBlock(hash64("blockW1new"), 101, hash64("blockW0"), spendNew)
	if err := s.ApplyBlock(ctx, blockNew); err != nil {
		t.Fatalf("apply replacement block: %v", err)
	}

	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != hash64("blockW1new") {
		t.Errorf("Tip.Hash = %s, want replacement block %s", tip.Hash, hash64("blockW1new"))
	}

	// X: address cache equals a fresh, independent recomputation from
	// canonical source tables for every address touched by this flow.
	for _, addr := range []string{"qAlice", "qBobNew"} {
		var cached struct {
			received, sent, balance, txCount int64
			firstSeen, lastSeen              *int64
		}
		err := pool.QueryRow(ctx, `
			SELECT total_received_satoshis, total_sent_satoshis, balance_satoshis, tx_count, first_seen_height, last_seen_height
			FROM addresses WHERE address = $1
		`, addr).Scan(&cached.received, &cached.sent, &cached.balance, &cached.txCount, &cached.firstSeen, &cached.lastSeen)
		if err != nil {
			t.Fatalf("read cached address %s: %v", addr, err)
		}

		var fresh struct {
			received, sent, balance, txCount int64
			firstSeen, lastSeen              *int64
		}
		err = pool.QueryRow(ctx, `
			WITH addr_utxos AS (
				SELECT o.value_satoshis, u.spent, u.creation_block_height, u.spending_block_height,
				       u.txid, u.spending_txid
				FROM output_addresses oa
				JOIN transaction_outputs o ON o.txid = oa.txid AND o.vout_index = oa.vout_index
				JOIN utxo_state u ON u.txid = oa.txid AND u.vout_index = oa.vout_index
				WHERE oa.address = $1
			),
			tx_ids AS (
				SELECT txid AS tid FROM addr_utxos
				UNION
				SELECT spending_txid AS tid FROM addr_utxos WHERE spent
			)
			SELECT
				COALESCE((SELECT SUM(value_satoshis) FROM addr_utxos), 0),
				COALESCE((SELECT SUM(value_satoshis) FROM addr_utxos WHERE spent), 0),
				COALESCE((SELECT SUM(value_satoshis) FROM addr_utxos WHERE NOT spent), 0),
				(SELECT COUNT(*) FROM tx_ids),
				(SELECT MIN(creation_block_height) FROM addr_utxos),
				(SELECT MAX(GREATEST(creation_block_height, COALESCE(spending_block_height, creation_block_height))) FROM addr_utxos)
		`, addr).Scan(&fresh.received, &fresh.sent, &fresh.balance, &fresh.txCount, &fresh.firstSeen, &fresh.lastSeen)
		if err != nil {
			t.Fatalf("fresh recompute for %s: %v", addr, err)
		}

		if cached.received != fresh.received || cached.sent != fresh.sent || cached.balance != fresh.balance ||
			cached.txCount != fresh.txCount ||
			derefInt64(cached.firstSeen) != derefInt64(fresh.firstSeen) ||
			derefInt64(cached.lastSeen) != derefInt64(fresh.lastSeen) {
			t.Errorf("address %s: cached={received:%d sent:%d balance:%d txCount:%d firstSeen:%d lastSeen:%d} fresh={received:%d sent:%d balance:%d txCount:%d firstSeen:%d lastSeen:%d} (should be identical)",
				addr,
				cached.received, cached.sent, cached.balance, cached.txCount, derefInt64(cached.firstSeen), derefInt64(cached.lastSeen),
				fresh.received, fresh.sent, fresh.balance, fresh.txCount, derefInt64(fresh.firstSeen), derefInt64(fresh.lastSeen),
			)
		}
	}

	// The orphaned branch's address must have no lingering cache entry —
	// nothing on the surviving canonical chain references it anymore.
	var orphanCacheCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM addresses WHERE address = 'qBobOld'`).Scan(&orphanCacheCount); err != nil {
		t.Fatalf("count qBobOld cache: %v", err)
	}
	if orphanCacheCount != 0 {
		t.Errorf("qBobOld address cache still present after its only block was orphaned: count = %d", orphanCacheCount)
	}
}

// ─── item 14: real mainnet fixture check ────────────────────────────────
//
// Real scriptPubKey bytes/txids from already-documented Phase-2A mainnet
// vectors (internal/script/classify_test.go), run through the real
// script.Classify pipeline and then through ApplyBlock, to exercise the
// store boundary with genuine chain data rather than only synthetic
// fixtures. The surrounding block headers are synthetic (this repo has no
// offline historical block source) — see task item 14, which explicitly
// permits this.

func TestApplyBlock_RealMainnetFixtures(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	type vector struct {
		name      string
		blockHash string
		height    int64
		txid      string
		scriptHex string
		wantType  script.Type
		value     int64
	}
	vectors := []vector{
		{
			name:      "block 1 coinbase — bare P2PK",
			blockHash: hash64("realblock1"),
			height:    900001,
			txid:      "1d4bdd70951bae6dd62b265a1877b7b140ea86b6ff0cd51102eec801c3947a1d",
			scriptHex: "21029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cdac",
			wantType:  script.TypeP2PK,
			value:     10_000_000_000,
		},
		{
			name:      "block 8000 coinbase — P2PKH",
			blockHash: hash64("realblock8000"),
			height:    900002,
			txid:      "a8ee14b21e7d42a4e9c155de159c9836ce932d4c4cccf77a0f23a71acd031b45",
			scriptHex: "76a914db6cdf671aa4dc3a395b934ca08bffb54658f36c88ac",
			wantType:  script.TypeP2PKH,
			value:     10_000_000_000,
		},
		{
			name:      "block 38393 — OP_RETURN witness commitment",
			blockHash: hash64("realblock38393"),
			height:    900003,
			txid:      "01d172762f277d6b2a48bc935ed2603dddd23f9eb85d84bd325fde05a3787be0",
			scriptHex: "6a24aa21a9ede2f61c3f71d1defd3fa999dfa36953755c690689799962b48bebd836974e8cf9",
			wantType:  script.TypeNullData,
			value:     0,
		},
		{
			name:      "block 494289 — native SegWit P2WPKH",
			blockHash: hash64("realblock494289"),
			height:    900004,
			txid:      "180c6aee4e8ff354868f7f44945192e1bd2941827413203f5b69c36bb3fb4a29",
			scriptHex: "001471fcd715a320938d8dfa1b56d9acdab9b1616be1",
			wantType:  script.TypeP2WPKH,
			value:     500_000_000,
		},
		{
			name:      "block 1284510 — real Taproot output",
			blockHash: hash64("realblock1284510"),
			height:    900005,
			txid:      "8c7381260e076f781de4c0c5246c709579c80e964738a719579ae4fd5c312106",
			scriptHex: "51202e44fe044d16a3b7900c179d9bb3fc005f0d5e92c89b8c7c0d340c7d6f56077c",
			wantType:  script.TypeP2TR,
			value:     250_000_000,
		},
	}

	prevHash := ""
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			classified := script.Classify(mustHex(t, v.scriptHex))
			if classified.Type != v.wantType {
				t.Fatalf("Classify = %s, want %s", classified.Type, v.wantType)
			}

			o := chain.Output{
				Index:          0,
				Value:          chain.Amount(v.value),
				ScriptPubKey:   mustHex(t, v.scriptHex),
				ScriptType:     classified.Type,
				WitnessVersion: classified.WitnessVersion,
				WitnessProgram: classified.WitnessProgram,
			}
			block := testBlock(v.blockHash, v.height, prevHash, coinbaseTx(v.txid, o))
			if err := s.ApplyBlock(ctx, block); err != nil {
				t.Fatalf("ApplyBlock: %v", err)
			}

			var gotType string
			var gotScript []byte
			if err := pool.QueryRow(ctx, `SELECT script_type, script_pubkey FROM transaction_outputs WHERE txid = $1 AND vout_index = 0`, v.txid).
				Scan(&gotType, &gotScript); err != nil {
				t.Fatalf("read back output: %v", err)
			}
			if gotType != string(v.wantType) {
				t.Errorf("stored script_type = %s, want %s", gotType, v.wantType)
			}
			if !bytes.Equal(gotScript, mustHex(t, v.scriptHex)) {
				t.Error("stored script_pubkey did not round-trip exactly")
			}
		})
		prevHash = v.blockHash
	}
}
