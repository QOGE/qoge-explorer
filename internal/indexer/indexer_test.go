package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── A: fresh DB starts at height 0 ─────────────────────────────────────

func TestSyncToTip_FreshDBStartsAtGenesis(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("A-g", 0, "")
	b1 := buildBlock("A-1", 1, g.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, b1)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("SyncToTip: %v", err)
	}

	tip, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != 1 || tip.Hash != b1.hash {
		t.Fatalf("tip = (%d, %s), want (1, %s)", tip.Height, tip.Hash, b1.hash)
	}

	// S: never bootstraps above height 0 — first applied height must be 0.
	if calls := fr.hashCalls[0]; calls == 0 {
		t.Errorf("GetBlockHash(0) was never called; indexer must always start historical sync at genesis")
	}
}

// ─── B: sequential sync 0..N produces checkpoint N ──────────────────────

func TestSyncToTip_SequentialSyncProducesCheckpointN(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	const n = 10
	blocks := buildChain("B", n)
	fr := newFakeRPC()
	fr.setActiveChain(blocks...)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("SyncToTip: %v", err)
	}

	tip, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != int64(n) {
		t.Fatalf("tip height = %d, want %d", tip.Height, n)
	}
	if tip.Hash != blocks[n].hash {
		t.Fatalf("tip hash = %s, want %s", tip.Hash, blocks[n].hash)
	}
}

// ─── C: restart resumes from checkpoint+1 ───────────────────────────────

func TestSyncToTip_RestartResumesFromCheckpointPlusOne(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	blocks := buildChain("C", 5)
	fr := newFakeRPC()
	fr.setActiveChain(blocks[:4]...) // sync only through height 3 first

	idx1 := New(fr, st, nil, time.Second, nil)
	if err := idx1.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}
	tip, _ := st.Tip(ctx)
	if tip.Height != 3 {
		t.Fatalf("tip after first sync = %d, want 3", tip.Height)
	}

	// "Restart": brand new Indexer over the SAME store, extended chain.
	fr.setActiveChain(blocks...)
	idx2 := New(fr, st, nil, time.Second, nil)
	if err := idx2.SyncToTip(ctx); err != nil {
		t.Fatalf("second SyncToTip: %v", err)
	}
	tip, _ = st.Tip(ctx)
	if tip.Height != 5 {
		t.Fatalf("tip after resume = %d, want 5", tip.Height)
	}
	if fr.hashCalls[4] == 0 {
		t.Errorf("GetBlockHash(4) never called on resume")
	}
}

// ─── D: already-caught-up SyncToTip is a no-op ──────────────────────────

func TestSyncToTip_AlreadyCaughtUpIsNoOp(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	blocks := buildChain("D", 3)
	fr := newFakeRPC()
	fr.setActiveChain(blocks...)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}

	_, _, verbose2Before := fr.callCounts()

	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("second (no-op) SyncToTip: %v", err)
	}
	_, _, verbose2After := fr.callCounts()
	if verbose2After != verbose2Before {
		t.Errorf("GetBlockVerbose2 called %d more time(s) on a caught-up pass; want 0", verbose2After-verbose2Before)
	}

	tip, _ := st.Tip(ctx)
	if tip.Height != 3 || tip.Hash != blocks[3].hash {
		t.Fatalf("tip changed on no-op pass: (%d, %s)", tip.Height, tip.Hash)
	}
}

// ─── E: Core tip grows while syncing; indexer continues to new tip ──────

func TestSyncToTip_CoreTipGrowsDuringSync(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	blocks := buildChain("E", 8)
	fr := newFakeRPC()
	fr.setActiveChain(blocks...) // Core already has 0..8 on disk...
	fr.queueCountOnce(5)         // ...but first getblockcount call reports only 5

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("SyncToTip: %v", err)
	}

	tip, _ := st.Tip(ctx)
	if tip.Height != 8 {
		t.Fatalf("tip height = %d, want 8 (indexer should continue past the initial snapshot target)", tip.Height)
	}
}

// ─── F: stale block (hash changed between the two GetBlockHash(H) calls) is NOT applied ──

func TestSyncToTip_StaleHashNotApplied(t *testing.T) {
	ctx := context.Background()
	st, pool := newTestStore(t)

	g := buildBlock("F-g", 0, "")
	staleB1 := buildBlock("F-stale-1", 1, g.hash)
	realB1 := buildBlock("F-real-1", 1, g.hash)

	fr := newFakeRPC()
	// Active chain already reports the REAL branch at height 1 (as if Core
	// reorganized between the indexer's two GetBlockHash(1) calls); the
	// FIRST call is overridden to return the stale hash once.
	fr.setActiveChain(g, realB1)
	fr.registerOrphan(staleB1)
	fr.queueHashOnce(1, staleB1.hash)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("SyncToTip: %v", err)
	}

	tip, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != 1 || tip.Hash != realB1.hash {
		t.Fatalf("tip = (%d, %s), want (1, %s) — the real branch, never the stale one", tip.Height, tip.Hash, realB1.hash)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, staleB1.hash).Scan(&count); err != nil {
		t.Fatalf("count stale block rows: %v", err)
	}
	if count != 0 {
		t.Errorf("stale block %s was persisted (count=%d), want 0 — it must never reach ApplyBlock", staleB1.hash, count)
	}
}

// ─── G: Core changes immediately after a block was applied; next reconciliation rolls it back ──

func TestSyncToTip_ChainMovesRightAfterApply(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("G-g", 0, "")
	a1 := buildBlock("G-a1", 1, g.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, a1)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}
	tip, _ := st.Tip(ctx)
	if tip.Height != 1 || tip.Hash != a1.hash {
		t.Fatalf("tip after first sync = (%d, %s), want (1, %s)", tip.Height, tip.Hash, a1.hash)
	}

	// Core silently reorganizes to a different height-1 block while the
	// indexer isn't looking (simulating "immediately after commit").
	b1 := buildBlock("G-b1", 1, g.hash)
	fr.setActiveChain(g, b1)

	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("second SyncToTip: %v", err)
	}
	tip, _ = st.Tip(ctx)
	if tip.Height != 1 || tip.Hash != b1.hash {
		t.Fatalf("tip after reconciliation = (%d, %s), want (1, %s)", tip.Height, tip.Hash, b1.hash)
	}
}

// ─── H: same-height shallow reorg: ancestor found, rollback, replacement applied ──

func TestSyncToTip_ShallowReorg(t *testing.T) {
	ctx := context.Background()
	st, pool := newTestStore(t)

	g := buildBlock("H-g", 0, "")
	a1 := buildBlock("H-a1", 1, g.hash)
	a2 := buildBlock("H-a2", 2, a1.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, a1, a2)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}

	b1 := buildBlock("H-b1", 1, g.hash)
	b2 := buildBlock("H-b2", 2, b1.hash)
	fr.setActiveChain(g, b1, b2)

	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("second SyncToTip: %v", err)
	}
	tip, _ := st.Tip(ctx)
	if tip.Height != 2 || tip.Hash != b2.hash {
		t.Fatalf("tip = (%d, %s), want (2, %s)", tip.Height, tip.Hash, b2.hash)
	}

	assertCanonical(t, ctx, st, 0, g.hash)
	assertCanonical(t, ctx, st, 1, b1.hash)
	assertCanonical(t, ctx, st, 2, b2.hash)
	assertOrphaned(t, ctx, pool, a1.hash)
	assertOrphaned(t, ctx, pool, a2.hash)
}

// ─── I: Core tip becomes shorter; ancestor search works ─────────────────

func TestSyncToTip_CoreTipBecomesShorter(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	blocks := buildChain("I", 5)
	fr := newFakeRPC()
	fr.setActiveChain(blocks...)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}

	// Core's chain "becomes shorter": now only 0..2 are active (2 replaced
	// by a competing block).
	shorter2 := buildBlock("I-short-2", 2, blocks[1].hash)
	fr.setActiveChain(blocks[0], blocks[1], shorter2)

	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("second SyncToTip: %v", err)
	}
	tip, _ := st.Tip(ctx)
	if tip.Height != 2 || tip.Hash != shorter2.hash {
		t.Fatalf("tip = (%d, %s), want (2, %s)", tip.Height, tip.Hash, shorter2.hash)
	}
}

// ─── J: reorg depth exactly 100 is automatic ─────────────────────────────

func TestSyncToTip_ReorgDepthExactly100IsAutomatic(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	const depth = 100
	blocksA := buildChain("J-a", depth)
	fr := newFakeRPC()
	fr.setActiveChain(blocksA...)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}
	tip, _ := st.Tip(ctx)
	if tip.Height != depth {
		t.Fatalf("tip height = %d, want %d", tip.Height, depth)
	}

	// Replacement branch diverges right after genesis, height 1..100.
	blocksB := make([]*fakeBlock, depth+1)
	blocksB[0] = blocksA[0]
	for h := 1; h <= depth; h++ {
		blocksB[h] = buildBlock("J-b-"+itoa(h), int64(h), blocksB[h-1].hash)
	}
	fr.setActiveChain(blocksB...)

	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("reorg SyncToTip: %v", err)
	}
	tip, _ = st.Tip(ctx)
	if tip.Height != depth || tip.Hash != blocksB[depth].hash {
		t.Fatalf("tip after depth-100 reorg = (%d, %s), want (%d, %s)", tip.Height, tip.Hash, depth, blocksB[depth].hash)
	}
}

// ─── K: reorg depth 101 halts with ErrReorgTooDeep; database unchanged ──

func TestSyncToTip_ReorgDepth101Halts(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	const depth = 101
	blocksA := buildChain("K-a", depth)
	fr := newFakeRPC()
	fr.setActiveChain(blocksA...)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}

	blocksB := make([]*fakeBlock, depth+1)
	blocksB[0] = blocksA[0]
	for h := 1; h <= depth; h++ {
		blocksB[h] = buildBlock("K-b-"+itoa(h), int64(h), blocksB[h-1].hash)
	}
	fr.setActiveChain(blocksB...)

	tipBefore, _ := st.Tip(ctx)

	err := idx.SyncToTip(ctx)
	if !errors.Is(err, ErrReorgTooDeep) {
		t.Fatalf("SyncToTip error = %v, want ErrReorgTooDeep", err)
	}

	tipAfter, _ := st.Tip(ctx)
	if tipAfter != tipBefore {
		t.Fatalf("tip changed on a rejected deep reorg: before=%+v after=%+v", tipBefore, tipAfter)
	}
}

// ─── L: no common ancestor within 100 blocks: ErrReorgTooDeep, database unchanged ──

func TestSyncToTip_NoCommonAncestorWithinWindow(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	blocksA := buildChain("L-a", 100)
	fr := newFakeRPC()
	fr.setActiveChain(blocksA...)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}

	// A totally different chain sharing NO history at all — even height 0
	// differs, and it's also 100 blocks long, so the whole window is
	// searched and nothing matches.
	blocksB := make([]*fakeBlock, 101)
	var prevHash string
	for h := 0; h <= 100; h++ {
		blocksB[h] = buildBlock("L-b-"+itoa(h), int64(h), prevHash)
		prevHash = blocksB[h].hash
	}
	fr.setActiveChain(blocksB...)

	tipBefore, _ := st.Tip(ctx)
	err := idx.SyncToTip(ctx)
	if !errors.Is(err, ErrReorgTooDeep) {
		t.Fatalf("SyncToTip error = %v, want ErrReorgTooDeep", err)
	}
	tipAfter, _ := st.Tip(ctx)
	if tipAfter != tipBefore {
		t.Fatalf("tip changed on a rejected no-ancestor reorg: before=%+v after=%+v", tipBefore, tipAfter)
	}
}

// ─── M: local canonical block missing inside expected history: ErrLocalChainGap ──

func TestSyncToTip_LocalChainGapHalts(t *testing.T) {
	ctx := context.Background()
	st, pool := newTestStore(t)

	blocks := buildChain("M", 5)
	fr := newFakeRPC()
	fr.setActiveChain(blocks...)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}

	// Corrupt local history directly: orphan height 2's blocks row behind
	// Store's back, simulating a local integrity gap below the tip.
	if _, err := pool.Exec(ctx, `UPDATE blocks SET canonical = false, orphaned_at = now() WHERE height = 2`); err != nil {
		t.Fatalf("corrupt local block: %v", err)
	}

	// Force a reorg whose ancestor search must walk back through height 2:
	// a full competing branch diverging right after genesis, so the
	// downward search visits 5,4,3,2 (the corrupted height) before it
	// could ever reach a real match at 1 or 0.
	competing := make([]*fakeBlock, 6)
	competing[0] = blocks[0]
	for h := 1; h <= 5; h++ {
		competing[h] = buildBlock("M-competing-"+itoa(h), int64(h), competing[h-1].hash)
	}
	fr.setActiveChain(competing...)

	err := idx.SyncToTip(ctx)
	if !errors.Is(err, ErrLocalChainGap) {
		t.Fatalf("SyncToTip error = %v, want ErrLocalChainGap", err)
	}
}

// ─── N: reorg branch flip-back: old orphan branch safely re-promoted ────

func TestSyncToTip_BranchFlipBack(t *testing.T) {
	ctx := context.Background()
	st, pool := newTestStore(t)

	g := buildBlock("N-g", 0, "")
	a1 := buildBlock("N-a1", 1, g.hash)
	a2 := buildBlock("N-a2", 2, a1.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, a1, a2)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("sync A: %v", err)
	}

	b1 := buildBlock("N-b1", 1, g.hash)
	b2 := buildBlock("N-b2", 2, b1.hash)
	fr.setActiveChain(g, b1, b2)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("sync B: %v", err)
	}
	tip, _ := st.Tip(ctx)
	if tip.Hash != b2.hash {
		t.Fatalf("tip after flip to B = %s, want %s", tip.Hash, b2.hash)
	}

	// Flip back to A. a1/a2 are still registered as orphans in the fake
	// (never discarded), exactly like a real Core node retaining orphan
	// block data.
	fr.setActiveChain(g, a1, a2)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("sync back to A: %v", err)
	}
	tip, _ = st.Tip(ctx)
	if tip.Height != 2 || tip.Hash != a2.hash {
		t.Fatalf("tip after flip back to A = (%d, %s), want (2, %s)", tip.Height, tip.Hash, a2.hash)
	}

	assertCanonical(t, ctx, st, 1, a1.hash)
	assertCanonical(t, ctx, st, 2, a2.hash)
	assertOrphaned(t, ctx, pool, b1.hash)
	assertOrphaned(t, ctx, pool, b2.hash)

	// A's blocks/transactions are still queryable as audit history despite
	// having been orphaned and re-promoted.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, a1.hash).Scan(&count); err != nil {
		t.Fatalf("count a1: %v", err)
	}
	if count != 1 {
		t.Errorf("a1 row count = %d, want 1", count)
	}
}

// ─── O: malformed DecodeBlock result/RPC data: no checkpoint movement ───

func TestSyncToTip_MalformedBlockDoesNotMoveCheckpoint(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("O-g", 0, "")
	bad := buildBlock("O-bad", 1, g.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, bad)

	// Missing MerkleRoot -> DecodeBlock must fail.
	raw := bad.raw()
	raw.MerkleRoot = nil
	fr.setRawOverride(bad.hash, raw)

	idx := New(fr, st, nil, time.Second, nil)
	err := idx.SyncToTip(ctx)
	if err == nil {
		t.Fatal("expected an error decoding a malformed block")
	}
	if errors.Is(err, ErrReorgTooDeep) || errors.Is(err, ErrLocalChainGap) || errors.Is(err, ErrRemoteChainMoved) {
		t.Fatalf("unexpected sentinel error: %v", err)
	}

	// Genesis (height 0) applied fine; the malformed block at height 1
	// must never advance the checkpoint past it.
	tip, _ := st.Tip(ctx)
	if tip.Height != 0 {
		t.Fatalf("checkpoint moved past genesis on a malformed block: tip = %+v", tip)
	}
}

// ─── P: RPC block fetch failure: no checkpoint movement ─────────────────

func TestSyncToTip_RPCFetchFailureDoesNotMoveCheckpoint(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("P-g", 0, "")
	b1 := buildBlock("P-1", 1, g.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, b1)
	fr.queueVerbose2ErrOnce(b1.hash, errors.New("connection reset"))

	idx := New(fr, st, nil, time.Second, nil)
	err := idx.SyncToTip(ctx)
	if err == nil {
		t.Fatal("expected an RPC fetch error")
	}

	tip, _ := st.Tip(ctx)
	if tip.Height != 0 {
		t.Fatalf("checkpoint moved past height 0 on an RPC failure at height 1: tip = %+v", tip)
	}
}

// ─── Q: context cancellation: Run exits cleanly ──────────────────────────

func TestRun_ContextCancellationExitsCleanly(t *testing.T) {
	st, _ := newTestStore(t)

	g := buildBlock("Q-g", 0, "")
	fr := newFakeRPC()
	fr.setActiveChain(g)

	idx := New(fr, st, nil, 10*time.Second, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- idx.Run(ctx) }()

	// Let Run complete its first (immediate, already-caught-up) SyncToTip
	// pass and settle into the poll wait before cancelling.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on clean cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit within 5s of context cancellation")
	}
}

// ─── R: no overlapping sync passes ───────────────────────────────────────

func TestSyncToTip_NoOverlappingPasses(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("R-g", 0, "")
	fr := newFakeRPC()
	fr.setActiveChain(g)
	fr.enableCountGate()

	idx := New(fr, st, nil, time.Second, nil)

	firstDone := make(chan error, 1)
	go func() { firstDone <- idx.SyncToTip(ctx) }()

	fr.waitGateEnter() // first call is now blocked inside GetBlockCount

	err := idx.SyncToTip(ctx)
	if !errors.Is(err, ErrSyncInProgress) {
		t.Fatalf("second concurrent SyncToTip error = %v, want ErrSyncInProgress", err)
	}

	fr.releaseGate()
	if err := <-firstDone; err != nil {
		t.Fatalf("first SyncToTip: %v", err)
	}
}

// ─── U: reorg between reconcile() and the next child fetch ──────────────
//
// Review round follow-up: reconcile() confirms the local tip is still on
// Core's active chain, but Core reorganizes in the window between that
// check and the next height's fetch. Store.ApplyBlock correctly rejects
// the resulting block with ErrNonSequentialBlock (its own parent no
// longer matches the checkpoint) — this must be diagnosed as normal
// remote chain movement, not surfaced as a terminal error.

func TestSyncToTip_ReorgBetweenReconcileAndNextChildFetch(t *testing.T) {
	ctx := context.Background()
	st, pool := newTestStore(t)

	g := buildBlock("U-g", 0, "")
	a1 := buildBlock("U-a1", 1, g.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, a1)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	b1 := buildBlock("U-b1", 1, g.hash)
	b2 := buildBlock("U-b2", 2, b1.hash)
	// Core has ALREADY reorganized to A0->B1->B2 by the time this pass
	// runs, but reconcile()'s first GetBlockHash(1) call is stubbed to
	// return the stale A1 hash exactly once — modeling "reconcile
	// observed the chain a moment before Core reorganized."
	fr.setActiveChain(g, b1, b2)
	fr.queueHashOnce(1, a1.hash)

	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	tip, _ := st.Tip(ctx)
	if tip.Height != 2 || tip.Hash != b2.hash {
		t.Fatalf("tip = (%d, %s), want (2, %s)", tip.Height, tip.Hash, b2.hash)
	}
	assertCanonical(t, ctx, st, 0, g.hash)
	assertCanonical(t, ctx, st, 1, b1.hash)
	assertCanonical(t, ctx, st, 2, b2.hash)
	assertOrphaned(t, ctx, pool, a1.hash)
}

// ─── V: tip retreats between reconcile() and the target read ────────────

func TestSyncToTip_TipRetreatsBetweenReconcileAndTargetRead(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("V-g", 0, "")
	a1 := buildBlock("V-a1", 1, g.hash)
	a2 := buildBlock("V-a2", 2, a1.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, a1, a2)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Core has ALREADY retreated to a shorter chain (A0->A1 only) by the
	// time this pass runs, but reconcile()'s checks are stubbed to
	// return the stale height-2 values exactly once each — modeling
	// "reconcile observed the taller chain a moment before Core
	// retreated," which the SEPARATE getblockcount read moments later
	// (for `target`) must not still trust.
	fr.setActiveChain(g, a1)
	fr.queueCountOnce(2)
	fr.queueHashOnce(2, a2.hash)

	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	tip, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != 1 || tip.Hash != a1.hash {
		t.Fatalf("tip = (%d, %s), want (1, %s) — the shortened Core branch, not the stale A2 tip", tip.Height, tip.Hash, a1.hash)
	}
}

// ─── W: the requested height disappears during forward sync ─────────────

func TestSyncToTip_RequestedHeightDisappearsDuringApply(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("W-g", 0, "")
	a1 := buildBlock("W-1", 1, g.hash)
	fr := newFakeRPC()
	// Core's real active chain only reaches height 1, but the FIRST
	// getblockcount call (the syncToTipLocked target snapshot; reconcile
	// makes no RPC calls at all against a fresh store) is stubbed to
	// report a target of 2 once — modeling a target snapshotted right
	// before Core retreated. GetBlockHash(2) then genuinely has nothing
	// to return, exactly like a real node erroring for a height above
	// its current tip.
	fr.setActiveChain(g, a1)
	fr.queueCountOnce(2)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("SyncToTip: %v", err)
	}

	tip, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != 1 || tip.Hash != a1.hash {
		t.Fatalf("tip = (%d, %s), want (1, %s)", tip.Height, tip.Hash, a1.hash)
	}
}

// ─── X: same-height reorg discovered only at the final caught-up check ──

func TestSyncToTip_SameHeightReorgAfterReconcile(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("X-g", 0, "")
	a1 := buildBlock("X-a1", 1, g.hash)
	a2 := buildBlock("X-a2", 2, a1.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, a1, a2)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	b1 := buildBlock("X-b1", 1, g.hash)
	b2 := buildBlock("X-b2", 2, b1.hash)
	// Core has ALREADY switched to A0->B1->B2 at the same height, but
	// reconcile()'s GetBlockHash(2) call is stubbed to return the stale
	// A2 hash exactly once, so reconcile() falsely confirms the tip is
	// still valid. The final caught-up confirmation (a SEPARATE
	// GetBlockHash(2) call, made after reconcile() already returned)
	// must catch the mismatch instead of returning stale success.
	fr.setActiveChain(g, b1, b2)
	fr.queueHashOnce(2, a2.hash)

	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	tip, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != 2 || tip.Hash != b2.hash {
		t.Fatalf("tip = (%d, %s), want (2, %s) — never a stale A2 success", tip.Height, tip.Hash, b2.hash)
	}
}

// ─── Y: stable Core + a contradictory fetched block stays terminal ──────

func TestSyncToTip_StableCoreContradictoryParentIsTerminal(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	g := buildBlock("Y-g", 0, "")
	a1 := buildBlock("Y-a1", 1, g.hash)
	fr := newFakeRPC()
	fr.setActiveChain(g, a1)

	idx := New(fr, st, nil, time.Second, nil)
	if err := idx.SyncToTip(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Core/local remain perfectly stable at height 1 (A1) throughout —
	// GetBlockHash(1) never changes. The height-2 block Core hands back
	// is internally self-consistent (its own hash is stable across the
	// two-hash race check) but claims a bogus, unrelated parent instead
	// of A1. This must never be explained away as a race: it is a
	// genuinely contradictory fetched block.
	bogusParent := fakeHash("Y-bogus-parent")
	bad2 := buildBlock("Y-bad2", 2, bogusParent)
	fr.setActiveChain(g, a1, bad2)

	tipBefore, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}

	err = idx.SyncToTip(ctx)
	if err == nil {
		t.Fatal("expected a terminal error for a contradictory fetched block")
	}
	if errors.Is(err, ErrRemoteChainMoved) {
		t.Fatalf("error = %v, must not be classified as a retryable remote-chain-moved race", err)
	}
	if errors.Is(err, ErrReorgTooDeep) || errors.Is(err, ErrLocalChainGap) {
		t.Fatalf("unexpected sentinel error: %v", err)
	}

	tipAfter, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tipAfter != tipBefore {
		t.Fatalf("checkpoint changed on a terminal contradictory-block error: before=%+v after=%+v", tipBefore, tipAfter)
	}
}

// ─── negative GetBlockCount is rejected as malformed RPC data ───────────
//
// Review round item 10: Core can never legitimately report a negative
// height. Silently accepting one could make an uninitialized store
// (Tip().Height == -1) appear accidentally "caught up" against it.

func TestSyncToTip_NegativeBlockCountRejected(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	fr := newFakeRPC()
	fr.queueCountOnce(-1)

	tipBefore, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}

	idx := New(fr, st, nil, time.Second, nil)
	err = idx.SyncToTip(ctx)
	if err == nil {
		t.Fatal("expected an error for a negative getblockcount result")
	}
	if errors.Is(err, ErrRemoteChainMoved) {
		t.Fatalf("negative getblockcount must not be classified as a retryable race: %v", err)
	}

	tipAfter, err := st.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tipAfter != tipBefore {
		t.Fatalf("checkpoint changed on a malformed negative getblockcount: before=%+v after=%+v", tipBefore, tipAfter)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────

func buildChain(prefix string, height int) []*fakeBlock {
	blocks := make([]*fakeBlock, height+1)
	prevHash := ""
	for h := 0; h <= height; h++ {
		blocks[h] = buildBlock(prefix+"-"+itoa(h), int64(h), prevHash)
		prevHash = blocks[h].hash
	}
	return blocks
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func assertCanonical(t *testing.T, ctx context.Context, st canonicalHashReader, height int64, wantHash string) {
	t.Helper()
	hash, found, err := st.CanonicalBlockHash(ctx, height)
	if err != nil {
		t.Fatalf("CanonicalBlockHash(%d): %v", height, err)
	}
	if !found || hash != wantHash {
		t.Errorf("CanonicalBlockHash(%d) = (%s, %t), want (%s, true)", height, hash, found, wantHash)
	}
}

type canonicalHashReader interface {
	CanonicalBlockHash(ctx context.Context, height int64) (string, bool, error)
}

func assertOrphaned(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hash string) {
	t.Helper()
	var canonical bool
	if err := pool.QueryRow(ctx, `SELECT canonical FROM blocks WHERE hash = $1`, hash).Scan(&canonical); err != nil {
		t.Fatalf("read canonical flag for %s: %v", hash, err)
	}
	if canonical {
		t.Errorf("block %s is still canonical, want orphaned", hash)
	}
}
