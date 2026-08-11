package mempool

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/jackc/pgx/v5/pgxpool"
)

// simpleCandidateTx builds a minimal, valid, non-coinbase
// CandidateTransaction spending prevTxid:0, paying valueSats to a fresh
// P2PKH output — the common shape most of this file's tests need.
func simpleCandidateTx(label, prevTxid string, valueSats, feeSats int64, depends []string) CandidateTransaction {
	txid := fakeHash(label + "-txid")
	addr := "qStore" + label
	return CandidateTransaction{
		Transaction: chain.Transaction{
			TxID:     txid,
			WTxID:    txid,
			Version:  2,
			LockTime: 0,
			Size:     200,
			VSize:    150,
			Weight:   600,
			Inputs: []chain.Input{
				{
					Index:       0,
					PreviousOut: &chain.OutPoint{TxID: prevTxid, Index: 0},
					ScriptSig:   []byte{0x47, 0x30, 0x44},
					Sequence:    0xfffffffd,
				},
			},
			Outputs: []chain.Output{
				{
					Index:        0,
					Value:        chain.Amount(valueSats),
					ScriptPubKey: p2pkhScript(label),
					ScriptType:   script.TypeP2PKH,
					Address:      addr,
				},
			},
		},
		FeeSatoshis: feeSats,
		EntryTime:   1_700_000_000,
		EntryHeight: i64Ptr(100),
		Replaceable: boolPtr(true),
		Depends:     depends,
	}
}

func candidateOf(coreHeight int64, coreHash string, txs ...CandidateTransaction) Candidate {
	return Candidate{CoreTipHeight: coreHeight, CoreTipHash: coreHash, Transactions: txs}
}

// TestReplaceSnapshot_InitialCommit is spec item 37.A.
func TestReplaceSnapshot_InitialCommit(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("A", fakeHash("prevA"), 10_00000000, 1000, nil)
	gen, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tip100"), txA))
	if err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	if gen != 1 {
		t.Fatalf("generation = %d, want 1", gen)
	}

	state, err := st.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.Initialized || state.Generation != 1 || state.TxCount != 1 {
		t.Fatalf("state = %+v, want initialized=true generation=1 tx_count=1", state)
	}
	requireMempoolTxExists(t, ctx, pool, txA.TxID, true)
}

// TestReplaceSnapshot_ReplacementRemovesOldRows is spec item 37.B.
func TestReplaceSnapshot_ReplacementRemovesOldRows(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("B-A", fakeHash("prevBA"), 10_00000000, 1000, nil)
	if _, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipB1"), txA)); err != nil {
		t.Fatalf("ReplaceSnapshot A: %v", err)
	}

	txB := simpleCandidateTx("B-B", fakeHash("prevBB"), 20_00000000, 2000, nil)
	gen, err := st.ReplaceSnapshot(ctx, candidateOf(101, fakeHash("tipB2"), txB))
	if err != nil {
		t.Fatalf("ReplaceSnapshot B: %v", err)
	}
	if gen != 2 {
		t.Fatalf("generation = %d, want 2", gen)
	}

	requireMempoolTxExists(t, ctx, pool, txA.TxID, false)
	requireMempoolTxExists(t, ctx, pool, txB.TxID, true)

	// Child rows (inputs) are gone too, not just the parent — proves the
	// cascade actually fired rather than merely orphaning children.
	var inputCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mempool_inputs WHERE txid = $1`, txA.TxID).Scan(&inputCount); err != nil {
		t.Fatalf("count mempool_inputs for txA: %v", err)
	}
	if inputCount != 0 {
		t.Fatalf("mempool_inputs for replaced txA = %d, want 0", inputCount)
	}
}

// TestReplaceSnapshot_NonEmptyToEmpty is spec item 37.C.
func TestReplaceSnapshot_NonEmptyToEmpty(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("C-A", fakeHash("prevCA"), 10_00000000, 1000, nil)
	if _, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipC1"), txA)); err != nil {
		t.Fatalf("ReplaceSnapshot non-empty: %v", err)
	}

	gen, err := st.ReplaceSnapshot(ctx, candidateOf(101, fakeHash("tipC2")))
	if err != nil {
		t.Fatalf("ReplaceSnapshot empty: %v", err)
	}
	if gen != 2 {
		t.Fatalf("generation = %d, want 2", gen)
	}

	state, err := st.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.Initialized || state.TxCount != 0 || state.TotalVSize != 0 || state.TotalFeeSatoshis != 0 {
		t.Fatalf("state after empty snapshot = %+v, want initialized=true, all counts zero", state)
	}
	requireMempoolTxExists(t, ctx, pool, txA.TxID, false)
}

// TestReplaceSnapshot_EmptyToNonEmpty is spec item 37.D.
func TestReplaceSnapshot_EmptyToNonEmpty(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	if _, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipD1"))); err != nil {
		t.Fatalf("ReplaceSnapshot empty: %v", err)
	}

	txA := simpleCandidateTx("D-A", fakeHash("prevDA"), 5_00000000, 500, nil)
	gen, err := st.ReplaceSnapshot(ctx, candidateOf(101, fakeHash("tipD2"), txA))
	if err != nil {
		t.Fatalf("ReplaceSnapshot non-empty: %v", err)
	}
	if gen != 2 {
		t.Fatalf("generation = %d, want 2", gen)
	}
	requireMempoolTxExists(t, ctx, pool, txA.TxID, true)
}

// TestReplaceSnapshot_FailedReplacementLeavesPriorSnapshotIntact is spec
// item 37.E. A candidate with a dangling dependency reference fails
// validate() before any DB mutation is attempted (in-memory), so this
// test additionally exercises a candidate that passes validate() but
// fails at the database layer (a script_type the CHECK constraint
// rejects), proving the whole transaction rolls back.
func TestReplaceSnapshot_FailedReplacementLeavesPriorSnapshotIntact(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("E-A", fakeHash("prevEA"), 10_00000000, 1000, nil)
	if _, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipE1"), txA)); err != nil {
		t.Fatalf("ReplaceSnapshot A: %v", err)
	}
	stateBefore, err := st.State(ctx)
	if err != nil {
		t.Fatalf("State before: %v", err)
	}

	bad := simpleCandidateTx("E-BAD", fakeHash("prevEBAD"), 1_00000000, 100, nil)
	bad.Outputs[0].ScriptType = "not-a-real-type" // violates the DB CHECK constraint
	if _, err := st.ReplaceSnapshot(ctx, candidateOf(101, fakeHash("tipE2"), bad)); err == nil {
		t.Fatalf("ReplaceSnapshot with invalid script_type: got nil error, want a failure")
	}

	stateAfter, err := st.State(ctx)
	if err != nil {
		t.Fatalf("State after: %v", err)
	}
	if stateAfter.Generation != stateBefore.Generation ||
		stateAfter.TxCount != stateBefore.TxCount ||
		stateAfter.TotalVSize != stateBefore.TotalVSize ||
		stateAfter.TotalFeeSatoshis != stateBefore.TotalFeeSatoshis ||
		*stateAfter.CoreTipHeight != *stateBefore.CoreTipHeight ||
		*stateAfter.CoreTipHash != *stateBefore.CoreTipHash {
		t.Fatalf("state changed after failed replacement: before=%+v after=%+v", stateBefore, stateAfter)
	}
	requireMempoolTxExists(t, ctx, pool, txA.TxID, true) // prior snapshot A untouched
	requireMempoolTxExists(t, ctx, pool, bad.TxID, false)
}

// TestReplaceSnapshot_GenerationOnlyIncrementsOnSuccess is spec item 37.F,
// building on the failed-replacement behavior above.
func TestReplaceSnapshot_GenerationOnlyIncrementsOnSuccess(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("F-A", fakeHash("prevFA"), 10_00000000, 1000, nil)
	gen1, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipF1"), txA))
	if err != nil || gen1 != 1 {
		t.Fatalf("ReplaceSnapshot A: gen=%d err=%v, want gen=1 err=nil", gen1, err)
	}

	bad := simpleCandidateTx("F-BAD", fakeHash("prevFBAD"), 1_00000000, 100, nil)
	bad.Outputs[0].ScriptType = "not-a-real-type"
	if _, err := st.ReplaceSnapshot(ctx, candidateOf(101, fakeHash("tipF2"), bad)); err == nil {
		t.Fatalf("ReplaceSnapshot with invalid script_type: got nil error")
	}

	txB := simpleCandidateTx("F-B", fakeHash("prevFB"), 5_00000000, 500, nil)
	gen2, err := st.ReplaceSnapshot(ctx, candidateOf(102, fakeHash("tipF3"), txB))
	if err != nil {
		t.Fatalf("ReplaceSnapshot B: %v", err)
	}
	if gen2 != 2 {
		t.Fatalf("generation after one successful + one failed replacement = %d, want 2 (failed attempt must not have incremented it)", gen2)
	}
}

// TestReplaceSnapshot_StateUpdatedAtomicallyWithRows is spec item 37.G:
// mempool_state and the row set are updated in the SAME transaction, so a
// reader never observes new rows with old state or vice versa. Verified
// here by confirming tx_count/total_vsize/total_fee_satoshis exactly
// match the just-committed candidate.
func TestReplaceSnapshot_StateUpdatedAtomicallyWithRows(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("G-A", fakeHash("prevGA"), 10_00000000, 1234, nil)
	txB := simpleCandidateTx("G-B", fakeHash("prevGB"), 20_00000000, 5678, nil)
	if _, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipG"), txA, txB)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	state, err := st.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	wantVSize := int64(txA.VSize + txB.VSize)
	wantFee := txA.FeeSatoshis + txB.FeeSatoshis
	if state.TxCount != 2 || state.TotalVSize != wantVSize || state.TotalFeeSatoshis != wantFee {
		t.Fatalf("state = %+v, want tx_count=2 total_vsize=%d total_fee_satoshis=%d", state, wantVSize, wantFee)
	}
	if state.CoreTipHeight == nil || *state.CoreTipHeight != 100 {
		t.Fatalf("state.CoreTipHeight = %v, want 100", state.CoreTipHeight)
	}
}

// TestReplaceSnapshot_DuplicateTxidRejected is spec item 37.H.
func TestReplaceSnapshot_DuplicateTxidRejected(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("H-A", fakeHash("prevHA"), 10_00000000, 1000, nil)
	txDup := txA // same TxID
	txDup.WTxID = fakeHash("H-different-wtxid")
	txDup.Outputs[0].Address = "qDifferentDestination"

	_, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipH"), txA, txDup))
	if err == nil {
		t.Fatalf("ReplaceSnapshot with duplicate txid: got nil error, want rejection")
	}
}

// TestReplaceSnapshot_DuplicateWTxidRejected is spec item 37.I.
func TestReplaceSnapshot_DuplicateWTxidRejected(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("I-A", fakeHash("prevIA"), 10_00000000, 1000, nil)
	txB := simpleCandidateTx("I-B", fakeHash("prevIB"), 20_00000000, 2000, nil)
	txB.WTxID = txA.WTxID // same WTxID, different TxID

	_, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipI"), txA, txB))
	if err == nil {
		t.Fatalf("ReplaceSnapshot with duplicate wtxid: got nil error, want rejection")
	}
}

// TestReplaceSnapshot_CoinbaseShapedTransactionRejected is spec item 37.J.
func TestReplaceSnapshot_CoinbaseShapedTransactionRejected(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	coinbase := simpleCandidateTx("J-CB", fakeHash("prevJ"), 10_00000000, 0, nil)
	coinbase.IsCoinbase = true
	coinbase.Inputs = []chain.Input{
		{Index: 0, Coinbase: []byte{0x51}, Sequence: 0xffffffff},
	}

	_, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipJ"), coinbase))
	if err == nil {
		t.Fatalf("ReplaceSnapshot with coinbase-shaped transaction: got nil error, want rejection")
	}
	requireMempoolTxExists(t, ctx, pool, coinbase.TxID, false)
}

// TestReplaceSnapshot_DependencyOrderIndependent is the required
// regression test for the child-before-parent FK ordering bug: a valid
// mempool dependency graph must commit successfully regardless of where
// in candidate.Transactions the child (dependent) and parent (depended-
// upon) transactions fall — txid is an opaque hash with no topological
// meaning, and sync.go lists candidate transactions in lexicographic
// txid order, which can and does put a child before its parent.
func TestReplaceSnapshot_DependencyOrderIndependent(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	// Fixed so lexical order is deterministic: child sorts BEFORE parent.
	childTxid := "0000000000000000000000000000000000000000000000000000000000000001"
	parentTxid := "fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe"
	if childTxid >= parentTxid {
		t.Fatalf("fixture bug: childTxid must sort before parentTxid")
	}

	child := simpleCandidateTx("OrderChild", fakeHash("prevOrderChild"), 5_00000000, 500, []string{parentTxid})
	child.TxID = childTxid
	child.WTxID = childTxid

	parent := simpleCandidateTx("OrderParent", fakeHash("prevOrderParent"), 10_00000000, 1000, nil)
	parent.TxID = parentTxid
	parent.WTxID = parentTxid

	// Deliberately passed in child, parent order — the exact order that
	// broke the old single-pass insertion.
	gen, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("order-tip"), child, parent))
	if err != nil {
		t.Fatalf("ReplaceSnapshot(child, parent order): %v", err)
	}
	if gen != 1 {
		t.Fatalf("generation = %d, want 1", gen)
	}

	requireMempoolTxExists(t, ctx, pool, childTxid, true)
	requireMempoolTxExists(t, ctx, pool, parentTxid, true)

	rows, err := pool.Query(ctx, `SELECT txid, depends_on_txid FROM mempool_dependencies`)
	if err != nil {
		t.Fatalf("query mempool_dependencies: %v", err)
	}
	defer rows.Close()
	var deps [][2]string
	for rows.Next() {
		var txid, dependsOn string
		if err := rows.Scan(&txid, &dependsOn); err != nil {
			t.Fatalf("scan dependency row: %v", err)
		}
		deps = append(deps, [2]string{txid, dependsOn})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate dependency rows: %v", err)
	}
	if len(deps) != 1 || deps[0][0] != childTxid || deps[0][1] != parentTxid {
		t.Fatalf("mempool_dependencies = %v, want exactly [[%s %s]]", deps, childTxid, parentTxid)
	}
}

// TestReplaceSnapshot_DanglingDependencyRejected: a "depends" reference to
// a txid outside the candidate snapshot is contradictory data (spec item
// 33) and must be rejected, not silently stored.
func TestReplaceSnapshot_DanglingDependencyRejected(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	st := NewStore(pool)

	txA := simpleCandidateTx("Dangle-A", fakeHash("prevDangleA"), 10_00000000, 1000, []string{fakeHash("nonexistent-parent-txid")})

	_, err := st.ReplaceSnapshot(ctx, candidateOf(100, fakeHash("tipDangle"), txA))
	if err == nil {
		t.Fatalf("ReplaceSnapshot with dangling dependency: got nil error, want rejection")
	}
	requireMempoolTxExists(t, ctx, pool, txA.TxID, false)
}

func requireMempoolTxExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, txid string, wantExists bool) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mempool_transactions WHERE txid = $1)`, txid).Scan(&exists); err != nil {
		t.Fatalf("check mempool_transactions existence for %s: %v", txid, err)
	}
	if exists != wantExists {
		t.Fatalf("mempool_transactions row for %s exists=%t, want %t", txid, exists, wantExists)
	}
}
