package query

import (
	"context"
	"testing"
)

// snapshotSync coordinates a query goroutine with a concurrent mutation via
// snapshotTestHook: the query blocks inside the hook (called exactly once,
// right after its read-only transaction's first statement has fixed its
// REPEATABLE READ snapshot) until the test explicitly releases it — never
// sleep-based.
type snapshotSync struct {
	reached chan struct{}
	proceed chan struct{}
}

func newSnapshotSync(t *testing.T) *snapshotSync {
	t.Helper()
	ss := &snapshotSync{reached: make(chan struct{}), proceed: make(chan struct{})}
	snapshotTestHook = func() {
		close(ss.reached)
		<-ss.proceed
	}
	t.Cleanup(func() { snapshotTestHook = nil })
	return ss
}

// waitForSnapshot blocks until the in-flight query's snapshot has been
// fixed (its first statement has executed).
func (ss *snapshotSync) waitForSnapshot(t *testing.T) {
	t.Helper()
	<-ss.reached
}

// release lets the in-flight query continue past the hook to completion,
// and disables the hook immediately afterward — a "fresh" query issued
// later in the same test must run with no hook installed at all, not just
// an already-fired one (whose channels are already closed).
func (ss *snapshotSync) release() {
	close(ss.proceed)
}

// disable removes the hook so subsequent queries in the same test run
// unmodified. Must be called after the in-flight query (that the hook was
// installed for) has actually completed — calling it too early would let
// that query run past its intended synchronization point.
func (ss *snapshotSync) disable() {
	snapshotTestHook = nil
}

// 5: a multi-statement TransactionDetail response cannot mix branches. The
// query's snapshot is fixed on branch A (A0->A1->A2); a concurrent,
// REAL Store.RollbackTo + Store.ApplyBlock sequence commits a full reorg to
// branch B in between the snapshot being fixed and the query's remaining
// reads. The in-flight query must still return A2's transaction as
// canonical (its snapshot-time truth) — combining canonical occurrence
// state with mutable utxo_state, exactly the scenario the review flagged.
// A subsequent NEW query must see the same transaction as orphaned.
func TestSnapshotConsistency_TransactionDetail_ConcurrentReorg(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("snaptx-genesis", 0, "", coinbaseTx("snaptx-genesis", 100_00000000, "qSnapTxGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("snaptx-A1", 1, g.Hash, coinbaseTx("snaptx-A1", 50_00000000, "qSnapTxA1"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}
	a2 := block("snaptx-A2", 2, a1.Hash, coinbaseTx("snaptx-A2", 50_00000000, "qSnapTxA2"))
	if err := st.ApplyBlock(ctx, a2); err != nil {
		t.Fatalf("apply A2: %v", err)
	}
	a2Txid := a2.Transactions[0].TxID

	ss := newSnapshotSync(t)

	type result struct {
		detail TransactionDetail
		err    error
	}
	done := make(chan result, 1)
	go func() {
		d, err := q.TransactionByTxID(ctx, a2Txid, false)
		done <- result{d, err}
	}()

	ss.waitForSnapshot(t)

	// Concurrently, through the REAL Store, reorg branch A entirely away.
	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("snaptx-B1", 1, g.Hash, coinbaseTx("snaptx-B1", 50_00000000, "qSnapTxB1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}
	b2 := block("snaptx-B2", 2, b1.Hash, coinbaseTx("snaptx-B2", 50_00000000, "qSnapTxB2"))
	if err := st.ApplyBlock(ctx, b2); err != nil {
		t.Fatalf("apply B2: %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight TransactionByTxID: %v", r.err)
	}
	if len(r.detail.Occurrences) != 1 {
		t.Fatalf("in-flight occurrences = %+v, want exactly 1", r.detail.Occurrences)
	}
	if !r.detail.Occurrences[0].Canonical {
		t.Fatalf("in-flight occurrence Canonical = false, want true (must reflect the snapshot fixed BEFORE the concurrent reorg, not the state after it)")
	}
	if r.detail.Occurrences[0].BlockHash != a2.Hash {
		t.Fatalf("in-flight occurrence block hash = %s, want %s", r.detail.Occurrences[0].BlockHash, a2.Hash)
	}

	// A fresh query, started AFTER the reorg committed, must see the new
	// (post-reorg) truth: the same transaction is now orphaned.
	fresh, err := q.TransactionByTxID(ctx, a2Txid, false)
	if err != nil {
		t.Fatalf("fresh TransactionByTxID: %v", err)
	}
	if fresh.Occurrences[0].Canonical {
		t.Fatalf("fresh occurrence Canonical = true, want false (A2 is now orphaned)")
	}
}

// 5 (continued): the same property for BlockDetail — an in-flight
// BlockByHash query's snapshot must not observe a concurrent reorg that
// orphans the very block being fetched.
func TestSnapshotConsistency_BlockDetail_ConcurrentReorg(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("snapblk-genesis", 0, "", coinbaseTx("snapblk-genesis", 100_00000000, "qSnapBlkGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("snapblk-A1", 1, g.Hash, coinbaseTx("snapblk-A1", 50_00000000, "qSnapBlkA1"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}
	a2 := block("snapblk-A2", 2, a1.Hash, coinbaseTx("snapblk-A2", 50_00000000, "qSnapBlkA2"))
	if err := st.ApplyBlock(ctx, a2); err != nil {
		t.Fatalf("apply A2: %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		detail BlockDetail
		err    error
	}
	done := make(chan result, 1)
	go func() {
		d, err := q.BlockByHash(ctx, a2.Hash)
		done <- result{d, err}
	}()

	ss.waitForSnapshot(t)

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("snapblk-B1", 1, g.Hash, coinbaseTx("snapblk-B1", 50_00000000, "qSnapBlkB1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}
	b2 := block("snapblk-B2", 2, b1.Hash, coinbaseTx("snapblk-B2", 50_00000000, "qSnapBlkB2"))
	if err := st.ApplyBlock(ctx, b2); err != nil {
		t.Fatalf("apply B2: %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight BlockByHash: %v", r.err)
	}
	if !r.detail.Canonical {
		t.Fatalf("in-flight block Canonical = false, want true (snapshot predates the concurrent reorg)")
	}
	if len(r.detail.Transactions) != 1 || r.detail.Transactions[0].TxID != a2.Transactions[0].TxID {
		t.Fatalf("in-flight block transactions = %+v", r.detail.Transactions)
	}

	fresh, err := q.BlockByHash(ctx, a2.Hash)
	if err != nil {
		t.Fatalf("fresh BlockByHash: %v", err)
	}
	if fresh.Canonical {
		t.Fatalf("fresh block Canonical = true, want false (A2 is now orphaned)")
	}
}

// ExplorerOverview must not mix Status and RecentBlocks across a concurrent
// reorg: the in-flight composite read's snapshot is fixed on branch
// A0->A1->A2 before a REAL Store.RollbackTo + Store.ApplyBlock sequence
// commits a full reorg to branch B0->B1->B2; the in-flight result must
// still describe branch A entirely (both Status and RecentBlocks), never a
// tip from one branch paired with recent blocks from the other. A
// subsequent fresh call must see branch B entirely.
func TestSnapshotConsistency_ExplorerOverview_ConcurrentReorg(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("snapov-genesis", 0, "", coinbaseTx("snapov-genesis", 100_00000000, "qSnapOvGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("snapov-A1", 1, g.Hash, coinbaseTx("snapov-A1", 50_00000000, "qSnapOvA1"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}
	a2 := block("snapov-A2", 2, a1.Hash, coinbaseTx("snapov-A2", 50_00000000, "qSnapOvA2"))
	if err := st.ApplyBlock(ctx, a2); err != nil {
		t.Fatalf("apply A2: %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		overview ExplorerOverview
		err      error
	}
	done := make(chan result, 1)
	go func() {
		o, err := q.ExplorerOverview(ctx, 10)
		done <- result{o, err}
	}()

	ss.waitForSnapshot(t)

	// Concurrently, through the REAL Store, reorg branch A entirely away.
	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("snapov-B1", 1, g.Hash, coinbaseTx("snapov-B1", 50_00000000, "qSnapOvB1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}
	b2 := block("snapov-B2", 2, b1.Hash, coinbaseTx("snapov-B2", 50_00000000, "qSnapOvB2"))
	if err := st.ApplyBlock(ctx, b2); err != nil {
		t.Fatalf("apply B2: %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight ExplorerOverview: %v", r.err)
	}
	if r.overview.Status.IndexedHash == nil || *r.overview.Status.IndexedHash != a2.Hash {
		t.Fatalf("in-flight Status.IndexedHash = %v, want %s (must reflect the snapshot fixed BEFORE the concurrent reorg)", r.overview.Status.IndexedHash, a2.Hash)
	}
	if len(r.overview.RecentBlocks.Blocks) == 0 || r.overview.RecentBlocks.Blocks[0].Hash != a2.Hash {
		t.Fatalf("in-flight RecentBlocks[0] = %+v, want hash %s", r.overview.RecentBlocks.Blocks, a2.Hash)
	}

	fresh, err := q.ExplorerOverview(ctx, 10)
	if err != nil {
		t.Fatalf("fresh ExplorerOverview: %v", err)
	}
	if fresh.Status.IndexedHash == nil || *fresh.Status.IndexedHash != b2.Hash {
		t.Fatalf("fresh Status.IndexedHash = %v, want %s", fresh.Status.IndexedHash, b2.Hash)
	}
	if len(fresh.RecentBlocks.Blocks) == 0 || fresh.RecentBlocks.Blocks[0].Hash != b2.Hash {
		t.Fatalf("fresh RecentBlocks[0] = %+v, want hash %s", fresh.RecentBlocks.Blocks, b2.Hash)
	}
}

// AddressDetail must not mix AddressSummary and AddressHistory across a
// concurrent reorg: branch A pays qSnapshotAddress 30 QOGE in A1; the
// in-flight composite read's snapshot is fixed before a REAL
// Store.RollbackTo + Store.ApplyBlock(B1) commits a reorg to a branch that
// never pays that address. The in-flight result must still show the
// snapshot-time balance AND history together (30 QOGE, A1 present), never a
// balance from one branch paired with history from the other. A fresh call
// afterward must see branch B entirely (0 QOGE, A1 absent).
func TestSnapshotConsistency_AddressDetail_ConcurrentReorg(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	const addr = "qSnapshotAddress"

	g := block("snapad-genesis", 0, "", coinbaseTx("snapad-genesis", 100_00000000, "qSnapAdGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("snapad-A1", 1, g.Hash, coinbaseTx("snapad-A1", 30_00000000, addr))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}
	a1Txid := a1.Transactions[0].TxID

	ss := newSnapshotSync(t)

	type result struct {
		detail AddressDetail
		err    error
	}
	done := make(chan result, 1)
	go func() {
		d, err := q.AddressDetail(ctx, addr, nil, nil, 10)
		done <- result{d, err}
	}()

	ss.waitForSnapshot(t)

	// Concurrently, through the REAL Store, reorg to a branch that never
	// pays addr.
	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("snapad-B1", 1, g.Hash, coinbaseTx("snapad-B1", 30_00000000, "qSnapAdOther"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight AddressDetail: %v", r.err)
	}
	if r.detail.Summary.BalanceSatoshis != 30_00000000 {
		t.Fatalf("in-flight balance = %d sats, want 30_00000000 (must reflect the snapshot fixed BEFORE the concurrent reorg)", r.detail.Summary.BalanceSatoshis)
	}
	found := false
	for _, e := range r.detail.History.Transactions {
		if e.TxID == a1Txid {
			found = true
		}
	}
	if !found {
		t.Fatalf("in-flight history = %+v, want A1's tx %s present", r.detail.History.Transactions, a1Txid)
	}

	fresh, err := q.AddressDetail(ctx, addr, nil, nil, 10)
	if err != nil {
		t.Fatalf("fresh AddressDetail: %v", err)
	}
	if fresh.Summary.BalanceSatoshis != 0 {
		t.Fatalf("fresh balance = %d sats, want 0", fresh.Summary.BalanceSatoshis)
	}
	for _, e := range fresh.History.Transactions {
		if e.TxID == a1Txid {
			t.Fatalf("fresh history unexpectedly contains orphaned A1 tx %s: %+v", a1Txid, fresh.History.Transactions)
		}
	}
}
