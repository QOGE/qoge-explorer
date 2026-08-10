package store

import (
	"context"
	"testing"
)

// ─── Phase 2C.2 task item 3 / item T: Store.CanonicalBlockHash ─────────

func TestCanonicalBlockHash_NoRowFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	hash, found, err := s.CanonicalBlockHash(ctx, 0)
	if err != nil {
		t.Fatalf("CanonicalBlockHash: %v", err)
	}
	if found {
		t.Fatalf("found = true on an empty store, want false")
	}
	if hash != "" {
		t.Fatalf("hash = %q on not-found, want empty", hash)
	}
}

func TestCanonicalBlockHash_ReturnsCanonicalHash(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("cbhG"), 0, "", coinbaseTx(hash64("cbhGtx"), out(0, 1_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)
	child := testBlock(hash64("cbhChild"), 1, hash64("cbhG"), coinbaseTx(hash64("cbhChildtx"), out(0, 1_000_000_000, "qBob")))
	mustApply(t, ctx, s, child)

	hash, found, err := s.CanonicalBlockHash(ctx, 0)
	if err != nil {
		t.Fatalf("CanonicalBlockHash(0): %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if hash != hash64("cbhG") {
		t.Errorf("hash = %s, want %s", hash, hash64("cbhG"))
	}

	hash, found, err = s.CanonicalBlockHash(ctx, 1)
	if err != nil {
		t.Fatalf("CanonicalBlockHash(1): %v", err)
	}
	if !found || hash != hash64("cbhChild") {
		t.Errorf("CanonicalBlockHash(1) = (%s, %t), want (%s, true)", hash, found, hash64("cbhChild"))
	}
}

func TestCanonicalBlockHash_AboveTipNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("cbhAbove"), 0, "", coinbaseTx(hash64("cbhAbovetx"), out(0, 1_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)

	_, found, err := s.CanonicalBlockHash(ctx, 5)
	if err != nil {
		t.Fatalf("CanonicalBlockHash(5): %v", err)
	}
	if found {
		t.Fatal("found = true above the current tip, want false")
	}
}

func TestCanonicalBlockHash_OrphanedBlockNotReturned(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("cbhOrphG"), 0, "", coinbaseTx(hash64("cbhOrphGtx"), out(0, 1_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)
	a1 := testBlock(hash64("cbhOrphA1"), 1, hash64("cbhOrphG"), coinbaseTx(hash64("cbhOrphA1tx"), out(0, 1_000_000_000, "qBob")))
	mustApply(t, ctx, s, a1)

	if err := s.RollbackTo(ctx, hash64("cbhOrphG")); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	// a1 is now orphaned (canonical = false): CanonicalBlockHash must not
	// report it, even though its blocks row still exists as audit history.
	_, found, err := s.CanonicalBlockHash(ctx, 1)
	if err != nil {
		t.Fatalf("CanonicalBlockHash(1) after rollback: %v", err)
	}
	if found {
		t.Fatal("found = true for an orphaned block, want false")
	}

	// Height 0 is still canonical.
	hash, found, err := s.CanonicalBlockHash(ctx, 0)
	if err != nil {
		t.Fatalf("CanonicalBlockHash(0) after rollback: %v", err)
	}
	if !found || hash != hash64("cbhOrphG") {
		t.Errorf("CanonicalBlockHash(0) = (%s, %t), want (%s, true)", hash, found, hash64("cbhOrphG"))
	}
}
