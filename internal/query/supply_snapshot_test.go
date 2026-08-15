package query

import (
	"context"
	"testing"
)

// TestSnapshotConsistency_SupplyOverview_ConcurrentApplyBlock is spec item
// 16: an in-flight SupplyOverview's snapshot is fixed at height N before a
// REAL, concurrent Store.ApplyBlock commits height N+1. The in-flight
// response must remain ENTIRELY at N — old indexed height/hash, old
// rollup-derived totals — never a mix; a fresh call afterward must see
// ENTIRELY N+1. This complements
// TestSnapshotConsistency_SupplyOverview_ConcurrentReorg (a rollback +
// re-apply) with the simpler pure-append case.
func TestSnapshotConsistency_SupplyOverview_ConcurrentApplyBlock(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("supplysnapappend-g", 0, "", coinbaseTx("supplysnapappend-g", 100*qoge, "qSupplySnapAppendG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		overview SupplyOverview
		err      error
	}
	done := make(chan result, 1)
	go func() {
		o, err := q.SupplyOverview(ctx)
		done <- result{o, err}
	}()

	ss.waitForSnapshot(t)

	h1 := block("supplysnapappend-h1", 1, g.Hash, coinbaseTx("supplysnapappend-h1", 50*qoge, "qSupplySnapAppendH1")) // underclaim: unclaimed=50
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1 concurrently: %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight SupplyOverview: %v", r.err)
	}
	if r.overview.IndexedHeight != 0 || r.overview.IndexedHash == nil || *r.overview.IndexedHash != g.Hash {
		t.Fatalf("in-flight anchor = (%d, %v), want (0, %s) — snapshot fixed before the concurrent ApplyBlock", r.overview.IndexedHeight, r.overview.IndexedHash, g.Hash)
	}
	if want := 100 * qoge; r.overview.CoinbaseOutputsSatoshis != want {
		t.Fatalf("in-flight CoinbaseOutputsSatoshis = %d, want %d (genesis only)", r.overview.CoinbaseOutputsSatoshis, want)
	}
	if r.overview.UnclaimedRewardSatoshis != 0 {
		t.Fatalf("in-flight UnclaimedRewardSatoshis = %d, want 0 (must not mix in h1's underclaim)", r.overview.UnclaimedRewardSatoshis)
	}

	fresh, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("fresh SupplyOverview: %v", err)
	}
	if fresh.IndexedHeight != 1 || fresh.IndexedHash == nil || *fresh.IndexedHash != h1.Hash {
		t.Fatalf("fresh anchor = (%d, %v), want (1, %s)", fresh.IndexedHeight, fresh.IndexedHash, h1.Hash)
	}
	if want := 150 * qoge; fresh.CoinbaseOutputsSatoshis != want {
		t.Fatalf("fresh CoinbaseOutputsSatoshis = %d, want %d (genesis 100 + h1 50)", fresh.CoinbaseOutputsSatoshis, want)
	}
	if want := 50 * qoge; fresh.UnclaimedRewardSatoshis != want {
		t.Fatalf("fresh UnclaimedRewardSatoshis = %d, want %d", fresh.UnclaimedRewardSatoshis, want)
	}
}

// TestSnapshotConsistency_SupplyOverview_ConcurrentReorg is spec item 50: an
// in-flight SupplyOverview's snapshot is fixed on branch A (genesis + A1,
// A1 underclaiming) before a REAL, concurrent reorg replaces A1 with B1 (a
// full claim). The in-flight response must be wholly branch-A-consistent —
// never height/hash from one branch paired with monetary totals from the
// other — while a fresh call afterward is wholly branch-B-consistent. Same
// snapshotTestHook-based synchronization every other composite read in this
// package already uses (no sleep).
func TestSnapshotConsistency_SupplyOverview_ConcurrentReorg(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("supplysnap-g", 0, "", coinbaseTx("supplysnap-g", 100*qoge, "qSupplySnapG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("supplysnap-a1", 1, g.Hash, coinbaseTx("supplysnap-a1", 40*qoge, "qSupplySnapA1")) // underclaim: unclaimed=60
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		overview SupplyOverview
		err      error
	}
	done := make(chan result, 1)
	go func() {
		o, err := q.SupplyOverview(ctx)
		done <- result{o, err}
	}()

	ss.waitForSnapshot(t)

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("supplysnap-b1", 1, g.Hash, coinbaseTx("supplysnap-b1", 100*qoge, "qSupplySnapB1")) // full claim
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight SupplyOverview: %v", r.err)
	}
	if r.overview.IndexedHash == nil || *r.overview.IndexedHash != a1.Hash {
		t.Fatalf("in-flight IndexedHash = %v, want %s (branch-A snapshot fixed before the reorg)", r.overview.IndexedHash, a1.Hash)
	}
	if want := 140 * qoge; r.overview.CoinbaseOutputsSatoshis != want {
		t.Fatalf("in-flight CoinbaseOutputsSatoshis = %d, want %d (branch A: genesis 100 + A1 40)", r.overview.CoinbaseOutputsSatoshis, want)
	}
	if want := 60 * qoge; r.overview.UnclaimedRewardSatoshis != want {
		t.Fatalf("in-flight UnclaimedRewardSatoshis = %d, want %d (must not mix in B1's full-claim truth)", r.overview.UnclaimedRewardSatoshis, want)
	}

	fresh, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("fresh SupplyOverview: %v", err)
	}
	if fresh.IndexedHash == nil || *fresh.IndexedHash != b1.Hash {
		t.Fatalf("fresh IndexedHash = %v, want %s", fresh.IndexedHash, b1.Hash)
	}
	if want := 200 * qoge; fresh.CoinbaseOutputsSatoshis != want {
		t.Fatalf("fresh CoinbaseOutputsSatoshis = %d, want %d (branch B: genesis 100 + B1 100)", fresh.CoinbaseOutputsSatoshis, want)
	}
	if fresh.UnclaimedRewardSatoshis != 0 {
		t.Fatalf("fresh UnclaimedRewardSatoshis = %d, want 0", fresh.UnclaimedRewardSatoshis)
	}
}
