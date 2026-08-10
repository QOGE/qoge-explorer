package query

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// reorgFixture applies genesis -> A1 -> A2 (the "old" branch), then rolls
// back to genesis and applies B1 -> B2 (the new canonical branch) —
// mirroring exactly what internal/indexer's reorg orchestration does via
// Store.RollbackTo, never ad-hoc SQL.
type reorgFixture struct {
	genesis      chain.Block
	oldA1, oldA2 chain.Block
	newB1, newB2 chain.Block
}

func buildReorgFixture(t *testing.T, ctx context.Context, st *store.Store) reorgFixture {
	t.Helper()

	g := block("reorg-genesis", 0, "", coinbaseTx("reorg-genesis", 100_00000000, "qReorgGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	a1 := block("reorg-A1", 1, g.Hash, coinbaseTx("reorg-A1", 50_00000000, "qReorgA1"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}
	a2 := block("reorg-A2", 2, a1.Hash, coinbaseTx("reorg-A2", 50_00000000, "qReorgA2"))
	if err := st.ApplyBlock(ctx, a2); err != nil {
		t.Fatalf("apply A2: %v", err)
	}

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}

	b1 := block("reorg-B1", 1, g.Hash, coinbaseTx("reorg-B1", 50_00000000, "qReorgB1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}
	b2 := block("reorg-B2", 2, b1.Hash, coinbaseTx("reorg-B2", 50_00000000, "qReorgB2"))
	if err := st.ApplyBlock(ctx, b2); err != nil {
		t.Fatalf("apply B2: %v", err)
	}

	return reorgFixture{genesis: g, oldA1: a1, oldA2: a2, newB1: b1, newB2: b2}
}

// E / S: block by orphan hash is visibly marked noncanonical, and remains
// queryable by hash after the branch it belonged to lost the canonical
// chain.
func TestBlockByHash_Orphan(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildReorgFixture(t, ctx, st)

	got, err := q.BlockByHash(ctx, f.oldA2.Hash)
	if err != nil {
		t.Fatalf("BlockByHash(orphaned A2): %v", err)
	}
	if got.Canonical {
		t.Fatalf("BlockByHash(orphaned A2).Canonical = true, want false")
	}
	if got.Height != 2 || got.Hash != f.oldA2.Hash {
		t.Fatalf("BlockByHash(orphaned A2) = %+v, want height=2 hash=%s", got, f.oldA2.Hash)
	}

	gotA1, err := q.BlockByHash(ctx, f.oldA1.Hash)
	if err != nil {
		t.Fatalf("BlockByHash(orphaned A1): %v", err)
	}
	if gotA1.Canonical {
		t.Fatalf("BlockByHash(orphaned A1).Canonical = true, want false")
	}
}

// R: branch reorg changes query results to the new canonical branch — a
// height lookup at 2 now returns B2, never A2, and the recent-blocks list
// reflects the new branch exclusively.
func TestReorg_HeightLookupFollowsNewCanonicalBranch(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildReorgFixture(t, ctx, st)

	got, err := q.BlockByHeight(ctx, 2)
	if err != nil {
		t.Fatalf("BlockByHeight(2): %v", err)
	}
	if got.Hash != f.newB2.Hash {
		t.Fatalf("BlockByHeight(2) = %s, want new canonical B2 %s", got.Hash, f.newB2.Hash)
	}

	got1, err := q.BlockByHeight(ctx, 1)
	if err != nil {
		t.Fatalf("BlockByHeight(1): %v", err)
	}
	if got1.Hash != f.newB1.Hash {
		t.Fatalf("BlockByHeight(1) = %s, want new canonical B1 %s", got1.Hash, f.newB1.Hash)
	}

	page, err := q.RecentBlocks(ctx, nil, 10)
	if err != nil {
		t.Fatalf("RecentBlocks: %v", err)
	}
	if len(page.Blocks) != 3 {
		t.Fatalf("RecentBlocks after reorg returned %d blocks, want 3 (genesis, B1, B2)", len(page.Blocks))
	}
	for _, b := range page.Blocks {
		if b.Hash == f.oldA1.Hash || b.Hash == f.oldA2.Hash {
			t.Fatalf("RecentBlocks after reorg still includes orphaned block %s", b.Hash)
		}
	}
}

// T: branch flip-back restores the canonical query view correctly — rolling
// back to genesis again and re-applying the ORIGINAL A branch must make A2
// canonical again and B2 orphaned, exactly mirroring internal/indexer's
// documented "safe orphan re-promotion" behavior.
func TestReorg_FlipBackRestoresCanonicalView(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)
	f := buildReorgFixture(t, ctx, st)

	if err := st.RollbackTo(ctx, f.genesis.Hash); err != nil {
		t.Fatalf("rollback to genesis (flip back): %v", err)
	}
	if err := st.ApplyBlock(ctx, f.oldA1); err != nil {
		t.Fatalf("re-apply A1: %v", err)
	}
	if err := st.ApplyBlock(ctx, f.oldA2); err != nil {
		t.Fatalf("re-apply A2: %v", err)
	}

	got, err := q.BlockByHeight(ctx, 2)
	if err != nil {
		t.Fatalf("BlockByHeight(2) after flip-back: %v", err)
	}
	if got.Hash != f.oldA2.Hash {
		t.Fatalf("BlockByHeight(2) after flip-back = %s, want re-promoted A2 %s", got.Hash, f.oldA2.Hash)
	}

	gotB2, err := q.BlockByHash(ctx, f.newB2.Hash)
	if err != nil {
		t.Fatalf("BlockByHash(B2) after flip-back: %v", err)
	}
	if gotB2.Canonical {
		t.Fatalf("B2.Canonical = true after flip-back, want false (now orphaned)")
	}

	gotA2ByHash, err := q.BlockByHash(ctx, f.oldA2.Hash)
	if err != nil {
		t.Fatalf("BlockByHash(A2) after flip-back: %v", err)
	}
	if !gotA2ByHash.Canonical {
		t.Fatalf("A2.Canonical = false after flip-back, want true (re-promoted)")
	}
}
