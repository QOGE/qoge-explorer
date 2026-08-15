package query

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// richListRowCounts is a row-count fingerprint across every table
// RichListOverview reads or could conceivably touch — mirrors
// supply_nonmutation_test.go's supplyRowCounts. A coarse but cheap way to
// prove RichListOverview genuinely never writes.
type richListRowCounts struct {
	syncState, blocks, addrs int64
}

func snapshotRichListRowCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) richListRowCounts {
	t.Helper()
	count := func(table string) int64 {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	return richListRowCounts{
		syncState: count("sync_state"),
		blocks:    count("blocks"),
		addrs:     count("addresses"),
	}
}

// TestRichListOverview_NonMutation is spec item 38: repeated
// RichListOverview calls — on an empty database and a populated one —
// must never insert, update, or delete a single row. query.Store's
// read-only enforcement is already structural (every statement is a
// SELECT — readTx's REPEATABLE READ / READ ONLY transaction), but this is
// a genuine end-to-end proof, not just a code-review claim.
func TestRichListOverview_NonMutation(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	before := snapshotRichListRowCounts(t, ctx, pool)
	if _, err := q.RichListOverview(ctx); err != nil {
		t.Fatalf("RichListOverview on empty database: %v", err)
	}
	if got := snapshotRichListRowCounts(t, ctx, pool); got != before {
		t.Fatalf("row counts changed after RichListOverview on empty database: before=%+v after=%+v", before, got)
	}

	g := block("richlist-nonmut-g", 0, "", coinbaseTx("richlist-nonmut-g", 100*qoge, "qRichlistNonMutG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("richlist-nonmut-h1", 1, g.Hash, coinbaseTx("richlist-nonmut-h1", 100*qoge, "qRichlistNonMutH1"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}

	before = snapshotRichListRowCounts(t, ctx, pool)
	for i := 0; i < 3; i++ {
		if _, err := q.RichListOverview(ctx); err != nil {
			t.Fatalf("RichListOverview call %d: %v", i, err)
		}
	}
	if got := snapshotRichListRowCounts(t, ctx, pool); got != before {
		t.Fatalf("row counts changed after repeated RichListOverview calls: before=%+v after=%+v", before, got)
	}
}
