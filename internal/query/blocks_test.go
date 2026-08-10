package query

import (
	"context"
	"testing"
)

// B: recent canonical block list ordering + keyset pagination.
func TestRecentBlocks_OrderingAndPagination(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("rb-genesis", 0, "", coinbaseTx("rb-genesis", 100_00000000, "qRB0"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	prev := g
	var hashes []string
	hashes = append(hashes, g.Hash)
	for h := int64(1); h <= 5; h++ {
		b := block(labelFor("rb", h), h, prev.Hash, coinbaseTx(labelFor("rb", h), 50_00000000, "qRB"+labelFor("rb", h)))
		if err := st.ApplyBlock(ctx, b); err != nil {
			t.Fatalf("apply block %d: %v", h, err)
		}
		hashes = append(hashes, b.Hash)
		prev = b
	}

	// pageSize=2: newest-first, first page is heights 5,4.
	page, err := q.RecentBlocks(ctx, nil, 2)
	if err != nil {
		t.Fatalf("RecentBlocks page1: %v", err)
	}
	if len(page.Blocks) != 2 || page.Blocks[0].Height != 5 || page.Blocks[1].Height != 4 {
		t.Fatalf("page1 = %+v, want heights [5,4]", page.Blocks)
	}
	if page.NextBeforeHeight == nil || *page.NextBeforeHeight != 4 {
		t.Fatalf("page1 NextBeforeHeight = %v, want 4", page.NextBeforeHeight)
	}

	page2, err := q.RecentBlocks(ctx, page.NextBeforeHeight, 2)
	if err != nil {
		t.Fatalf("RecentBlocks page2: %v", err)
	}
	if len(page2.Blocks) != 2 || page2.Blocks[0].Height != 3 || page2.Blocks[1].Height != 2 {
		t.Fatalf("page2 = %+v, want heights [3,2]", page2.Blocks)
	}

	page3, err := q.RecentBlocks(ctx, page2.NextBeforeHeight, 2)
	if err != nil {
		t.Fatalf("RecentBlocks page3: %v", err)
	}
	if len(page3.Blocks) != 2 || page3.Blocks[0].Height != 1 || page3.Blocks[1].Height != 0 {
		t.Fatalf("page3 = %+v, want heights [1,0]", page3.Blocks)
	}
	if page3.NextBeforeHeight != nil {
		t.Fatalf("page3 NextBeforeHeight = %v, want nil (reached genesis)", page3.NextBeforeHeight)
	}

	// Hard maximum page size is enforced regardless of what's requested.
	pageMax, err := q.RecentBlocks(ctx, nil, 10000)
	if err != nil {
		t.Fatalf("RecentBlocks oversized: %v", err)
	}
	if len(pageMax.Blocks) != 6 {
		t.Fatalf("oversized page returned %d blocks, want 6 (all of them, still under MaxPageSize)", len(pageMax.Blocks))
	}
}

func labelFor(prefix string, h int64) string {
	return prefix + "-" + string(rune('a'+h))
}

// C: canonical block by height.
func TestBlockByHeight(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("bh-genesis", 0, "", coinbaseTx("bh-genesis", 100_00000000, "qBH0"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("bh-1", 1, g.Hash, coinbaseTx("bh-1", 50_00000000, "qBH1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block 1: %v", err)
	}

	got, err := q.BlockByHeight(ctx, 1)
	if err != nil {
		t.Fatalf("BlockByHeight(1): %v", err)
	}
	if got.Hash != b1.Hash || !got.Canonical {
		t.Fatalf("BlockByHeight(1) = %+v, want hash=%s canonical=true", got, b1.Hash)
	}
	if len(got.Transactions) != 1 || got.Transactions[0].TxID != b1.Transactions[0].TxID {
		t.Fatalf("BlockByHeight(1).Transactions = %+v", got.Transactions)
	}

	if _, err := q.BlockByHeight(ctx, 99); err != ErrNotFound {
		t.Fatalf("BlockByHeight(99) err = %v, want ErrNotFound", err)
	}
}

// D: block by canonical hash.
func TestBlockByHash_Canonical(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("bhc-genesis", 0, "", coinbaseTx("bhc-genesis", 100_00000000, "qBHC0"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	got, err := q.BlockByHash(ctx, g.Hash)
	if err != nil {
		t.Fatalf("BlockByHash: %v", err)
	}
	if !got.Canonical || got.Height != 0 {
		t.Fatalf("BlockByHash(genesis) = %+v, want canonical=true height=0", got)
	}

	if _, err := q.BlockByHash(ctx, fakeHash("does-not-exist")); err != ErrNotFound {
		t.Fatalf("BlockByHash(missing) err = %v, want ErrNotFound", err)
	}
}
