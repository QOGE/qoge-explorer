package query

import (
	"context"
	"errors"
	"testing"
)

// TestSnapshotConsistency_MempoolOverview_ConcurrentReplacement is spec
// item 32: an in-flight MempoolOverview's snapshot is fixed on generation 1
// (tx A) before a REAL, concurrent Store.ReplaceSnapshot commits generation
// 2 (tx B). The in-flight read must observe a COHERENT generation 1 (state
// generation=1, tx A present, tx B absent) — never generation 1 paired with
// tx B, or generation 2 paired with tx A. A fresh read afterward must
// observe a coherent generation 2.
func TestSnapshotConsistency_MempoolOverview_ConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	txA := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("snapov-A", 0), 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("snapov-tip-1"), txA)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen1): %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		overview MempoolOverview
		err      error
	}
	done := make(chan result, 1)
	go func() {
		o, err := q.MempoolOverview(ctx, nil, 50)
		done <- result{o, err}
	}()

	ss.waitForSnapshot(t)

	txB := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("snapov-B", 0), 2000, 1_700_000_100, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(2, fakeHash("snapov-tip-2"), txB)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen2): %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight MempoolOverview: %v", r.err)
	}
	if r.overview.State.Generation != 1 {
		t.Fatalf("in-flight state generation = %d, want 1 (must reflect the snapshot fixed BEFORE the concurrent replacement)", r.overview.State.Generation)
	}
	if len(r.overview.Transactions.Transactions) != 1 || r.overview.Transactions.Transactions[0].TxID != txA.TxID {
		t.Fatalf("in-flight transactions = %+v, want exactly txA (%s)", r.overview.Transactions.Transactions, txA.TxID)
	}

	fresh, err := q.MempoolOverview(ctx, nil, 50)
	if err != nil {
		t.Fatalf("fresh MempoolOverview: %v", err)
	}
	if fresh.State.Generation != 2 {
		t.Fatalf("fresh state generation = %d, want 2", fresh.State.Generation)
	}
	if len(fresh.Transactions.Transactions) != 1 || fresh.Transactions.Transactions[0].TxID != txB.TxID {
		t.Fatalf("fresh transactions = %+v, want exactly txB (%s)", fresh.Transactions.Transactions, txB.TxID)
	}
}

// TestSnapshotConsistency_MempoolDetail_ConcurrentReplacement is spec item
// 33: transaction A exists in generation 1; while an in-flight
// MempoolTransactionByTxID(A) is assembling its detail response, a REAL
// concurrent Store.ReplaceSnapshot removes A entirely (replacing with an
// unrelated tx B in generation 2). Because the in-flight read's snapshot
// was fixed BEFORE that replacement committed, PostgreSQL's REPEATABLE READ
// isolation guarantees every remaining statement in that same read-only
// transaction still sees A's complete pre-replacement body — the in-flight
// call must return the COMPLETE generation-1 transaction A (never a
// not-found, and never any row mixed in from generation 2). A fresh call
// afterward must see generation 2: A gone (ErrNotFound), B present.
func TestSnapshotConsistency_MempoolDetail_ConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	txA := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("snapdetail-A", 0), 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("snapdetail-tip-1"), txA)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen1): %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		detail MempoolTransactionDetail
		err    error
	}
	done := make(chan result, 1)
	go func() {
		d, err := q.MempoolTransactionByTxID(ctx, txA.TxID, false)
		done <- result{d, err}
	}()

	ss.waitForSnapshot(t)

	txB := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("snapdetail-B", 0), 2000, 1_700_000_100, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(2, fakeHash("snapdetail-tip-2"), txB)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen2): %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight MempoolTransactionByTxID(A): %v, want the complete generation-1 transaction (never not-found — REPEATABLE READ must still see the pre-replacement row)", r.err)
	}
	if r.detail.TxID != txA.TxID || r.detail.WTxID != txA.WTxID {
		t.Fatalf("in-flight detail identities = %s/%s, want %s/%s", r.detail.TxID, r.detail.WTxID, txA.TxID, txA.WTxID)
	}
	if r.detail.Snapshot.Generation != 1 {
		t.Fatalf("in-flight detail Snapshot.Generation = %d, want 1 (must not observe the concurrent generation-2 replacement)", r.detail.Snapshot.Generation)
	}
	if len(r.detail.Outputs) != len(txA.Outputs) || len(r.detail.Inputs) != len(txA.Inputs) {
		t.Fatalf("in-flight detail inputs/outputs = %d/%d, want %d/%d (complete generation-1 body, never mixed)",
			len(r.detail.Inputs), len(r.detail.Outputs), len(txA.Inputs), len(txA.Outputs))
	}

	// A fresh call afterward must see generation 2: A gone, B present.
	_, err := q.MempoolTransactionByTxID(ctx, txA.TxID, false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("fresh MempoolTransactionByTxID(A) error = %v, want ErrNotFound (A was replaced away)", err)
	}
	freshB, err := q.MempoolTransactionByTxID(ctx, txB.TxID, false)
	if err != nil {
		t.Fatalf("fresh MempoolTransactionByTxID(B): %v", err)
	}
	if freshB.Snapshot.Generation != 2 {
		t.Fatalf("fresh detail Snapshot.Generation = %d, want 2", freshB.Snapshot.Generation)
	}
}
