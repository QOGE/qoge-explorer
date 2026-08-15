package query

import (
	"context"
	"testing"
)

// TestSnapshotConsistency_RichListOverview_ConcurrentReorg is spec item 26:
// an in-flight RichListOverview's snapshot is fixed on branch A (genesis +
// A1, A1 paying qRichSnapA) before a REAL, concurrent reorg replaces A1
// with B1 (paying a different address). The in-flight response must be
// wholly branch-A-consistent — indexed hash AND ranking both from branch
// A, never mixed — while a fresh call afterward is wholly
// branch-B-consistent. Same snapshotTestHook-based synchronization every
// other composite read in this package uses (no sleep).
func TestSnapshotConsistency_RichListOverview_ConcurrentReorg(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlistsnap-g", 0, "", coinbaseTx("richlistsnap-g", 100*qoge, "qRichSnapG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("richlistsnap-a1", 1, g.Hash, coinbaseTx("richlistsnap-a1", 100*qoge, "qRichSnapA"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		overview RichListOverview
		err      error
	}
	done := make(chan result, 1)
	go func() {
		o, err := q.RichListOverview(ctx)
		done <- result{o, err}
	}()

	ss.waitForSnapshot(t)

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("richlistsnap-b1", 1, g.Hash, coinbaseTx("richlistsnap-b1", 100*qoge, "qRichSnapB"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight RichListOverview: %v", r.err)
	}
	if r.overview.IndexedHash == nil || *r.overview.IndexedHash != a1.Hash {
		t.Fatalf("in-flight IndexedHash = %v, want %s (branch-A snapshot fixed before the reorg)", r.overview.IndexedHash, a1.Hash)
	}
	if len(r.overview.Entries) != 1 || r.overview.Entries[0].Address != "qRichSnapA" {
		t.Fatalf("in-flight Entries = %+v, want [qRichSnapA] (must not mix in B1's ranking)", r.overview.Entries)
	}

	fresh, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("fresh RichListOverview: %v", err)
	}
	if fresh.IndexedHash == nil || *fresh.IndexedHash != b1.Hash {
		t.Fatalf("fresh IndexedHash = %v, want %s", fresh.IndexedHash, b1.Hash)
	}
	if len(fresh.Entries) != 1 || fresh.Entries[0].Address != "qRichSnapB" {
		t.Fatalf("fresh Entries = %+v, want [qRichSnapB]", fresh.Entries)
	}
}

// TestSnapshotConsistency_RichListOverview_ConcurrentApplyBlock is spec
// item 27: an in-flight RichListOverview's snapshot is fixed at height N
// before a REAL, concurrent Store.ApplyBlock commits height N+1 that
// changes the ranking. The in-flight response must remain entirely at N;
// a fresh call afterward must see entirely N+1.
func TestSnapshotConsistency_RichListOverview_ConcurrentApplyBlock(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlistsnapappend-g", 0, "", coinbaseTx("richlistsnapappend-g", 100*qoge, "qRichSnapAppendG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("richlistsnapappend-h1", 1, g.Hash, coinbaseTx("richlistsnapappend-h1", 10*qoge, "qRichSnapAppendA"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		overview RichListOverview
		err      error
	}
	done := make(chan result, 1)
	go func() {
		o, err := q.RichListOverview(ctx)
		done <- result{o, err}
	}()

	ss.waitForSnapshot(t)

	h2 := block("richlistsnapappend-h2", 2, h1.Hash, coinbaseTx("richlistsnapappend-h2", 50*qoge, "qRichSnapAppendB"))
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply height 2 concurrently: %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight RichListOverview: %v", r.err)
	}
	if r.overview.IndexedHash == nil || *r.overview.IndexedHash != h1.Hash {
		t.Fatalf("in-flight IndexedHash = %v, want %s", r.overview.IndexedHash, h1.Hash)
	}
	if len(r.overview.Entries) != 1 || r.overview.Entries[0].Address != "qRichSnapAppendA" {
		t.Fatalf("in-flight Entries = %+v, want [qRichSnapAppendA] (must not include the concurrently-applied h2)", r.overview.Entries)
	}

	fresh, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("fresh RichListOverview: %v", err)
	}
	if fresh.IndexedHash == nil || *fresh.IndexedHash != h2.Hash {
		t.Fatalf("fresh IndexedHash = %v, want %s", fresh.IndexedHash, h2.Hash)
	}
	if len(fresh.Entries) != 2 || fresh.Entries[0].Address != "qRichSnapAppendB" {
		t.Fatalf("fresh Entries = %+v, want [qRichSnapAppendB, qRichSnapAppendA]", fresh.Entries)
	}
}
