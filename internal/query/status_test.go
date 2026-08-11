package query

import (
	"context"
	"testing"
)

// A: indexed status.
func TestStatus(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	status, err := q.Status(ctx)
	if err != nil {
		t.Fatalf("Status (bootstrap): %v", err)
	}
	if status.IndexedHeight != -1 || status.IndexedHash != nil {
		t.Fatalf("bootstrap status = (%d, %v), want (-1, nil)", status.IndexedHeight, status.IndexedHash)
	}

	g := block("status-genesis", 0, "", coinbaseTx("status-genesis", 100_00000000, "qStatusGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock genesis: %v", err)
	}

	status, err = q.Status(ctx)
	if err != nil {
		t.Fatalf("Status (after genesis): %v", err)
	}
	if status.IndexedHeight != 0 {
		t.Fatalf("indexed height = %d, want 0", status.IndexedHeight)
	}
	if status.IndexedHash == nil || *status.IndexedHash != g.Hash {
		t.Fatalf("indexed hash = %v, want %s", status.IndexedHash, g.Hash)
	}
}
