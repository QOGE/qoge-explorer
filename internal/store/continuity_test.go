package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
)

// ─── Review round: canonical tip continuity (task item 1) ──────────────

func TestApplyBlock_CanonicalTipContinuity(t *testing.T) {
	ctx := context.Background()

	t.Run("tip replay succeeds", func(t *testing.T) {
		s, _ := newTestStore(t)
		g := testBlock(hash64("contG1"), 100, "", coinbaseTx(hash64("contG1tx"), out(0, 1_000_000_000, "qAlice")))
		mustApply(t, ctx, s, g)
		if err := s.ApplyBlock(ctx, g); err != nil {
			t.Fatalf("tip replay: %v", err)
		}
	})

	t.Run("immediate child succeeds", func(t *testing.T) {
		s, _ := newTestStore(t)
		g := testBlock(hash64("contG2"), 100, "", coinbaseTx(hash64("contG2tx"), out(0, 1_000_000_000, "qAlice")))
		mustApply(t, ctx, s, g)
		child := testBlock(hash64("contG2child"), 101, hash64("contG2"), coinbaseTx(hash64("contG2childtx"), out(0, 1_000_000_000, "qBob")))
		if err := s.ApplyBlock(ctx, child); err != nil {
			t.Fatalf("immediate child: %v", err)
		}
	})

	t.Run("height jump rejected", func(t *testing.T) {
		s, pool := newTestStore(t)
		g := testBlock(hash64("contG3"), 100, "", coinbaseTx(hash64("contG3tx"), out(0, 1_000_000_000, "qAlice")))
		mustApply(t, ctx, s, g)
		jump := testBlock(hash64("contG3jump"), 102, hash64("contG3"), coinbaseTx(hash64("contG3jumptx"), out(0, 1_000_000_000, "qBob")))
		err := s.ApplyBlock(ctx, jump)
		if err == nil {
			t.Fatal("expected a height jump to be rejected")
		}
		if !errors.Is(err, ErrNonSequentialBlock) {
			t.Errorf("error = %v, want ErrNonSequentialBlock", err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, hash64("contG3jump")).Scan(&count); err != nil {
			t.Fatalf("count blocks: %v", err)
		}
		if count != 0 {
			t.Errorf("rejected block persisted: count = %d", count)
		}
	})

	t.Run("wrong prev_hash rejected", func(t *testing.T) {
		s, pool := newTestStore(t)
		g := testBlock(hash64("contG4"), 100, "", coinbaseTx(hash64("contG4tx"), out(0, 1_000_000_000, "qAlice")))
		mustApply(t, ctx, s, g)
		wrong := testBlock(hash64("contG4child"), 101, hash64("wrongprevhash"), coinbaseTx(hash64("contG4childtx"), out(0, 1_000_000_000, "qBob")))
		err := s.ApplyBlock(ctx, wrong)
		if err == nil {
			t.Fatal("expected a wrong prev_hash to be rejected")
		}
		if !errors.Is(err, ErrNonSequentialBlock) {
			t.Errorf("error = %v, want ErrNonSequentialBlock", err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, hash64("contG4child")).Scan(&count); err != nil {
			t.Fatalf("count blocks: %v", err)
		}
		if count != 0 {
			t.Errorf("rejected block persisted: count = %d", count)
		}
	})

	t.Run("old canonical block below tip rejected", func(t *testing.T) {
		s, _ := newTestStore(t)
		g := testBlock(hash64("contG5"), 100, "", coinbaseTx(hash64("contG5tx"), out(0, 1_000_000_000, "qAlice")))
		mustApply(t, ctx, s, g)
		child := testBlock(hash64("contG5child"), 101, hash64("contG5"), coinbaseTx(hash64("contG5childtx"), out(0, 1_000_000_000, "qBob")))
		mustApply(t, ctx, s, child)

		err := s.ApplyBlock(ctx, g)
		if err == nil {
			t.Fatal("expected an old canonical block below the tip to be rejected")
		}
		if !errors.Is(err, ErrNonSequentialBlock) {
			t.Errorf("error = %v, want ErrNonSequentialBlock", err)
		}
	})

	t.Run("old orphan below current replacement tip rejected and cannot recreate state", func(t *testing.T) {
		s, _ := newTestStore(t)
		g := testBlock(hash64("contG6"), 100, "", coinbaseTx(hash64("contG6tx"), out(0, 5_000_000_000, "qAlice")))
		mustApply(t, ctx, s, g)

		a1spend := spendTx(hash64("contG6a1tx"), hash64("contG6a1tx"), 200,
			[]chain.Input{spendInput(0, hash64("contG6tx"), 0, nil)},
			[]chain.Output{out(0, 4_999_000_000, "qBob")},
		)
		a1 := testBlock(hash64("contG6a1"), 101, hash64("contG6"), a1spend)
		mustApply(t, ctx, s, a1)

		if err := s.RollbackTo(ctx, hash64("contG6")); err != nil {
			t.Fatalf("RollbackTo: %v", err)
		}

		b1spend := spendTx(hash64("contG6b1tx"), hash64("contG6b1tx"), 200,
			[]chain.Input{spendInput(0, hash64("contG6tx"), 0, nil)},
			[]chain.Output{out(0, 4_998_000_000, "qCarol")},
		)
		b1 := testBlock(hash64("contG6b1"), 101, hash64("contG6"), b1spend)
		mustApply(t, ctx, s, b1)

		b2 := testBlock(hash64("contG6b2"), 102, hash64("contG6b1"), coinbaseTx(hash64("contG6b2tx"), out(0, 1_000_000_000, "qDave")))
		mustApply(t, ctx, s, b2)

		// a1 is now an old orphan two below the current tip (b2). It is
		// NOT the immediate child of the current tip, so re-applying it
		// must be rejected by continuity, not silently promoted.
		err := s.ApplyBlock(ctx, a1)
		if err == nil {
			t.Fatal("expected an old orphan below the current tip to be rejected")
		}
		if !errors.Is(err, ErrNonSequentialBlock) {
			t.Errorf("error = %v, want ErrNonSequentialBlock", err)
		}

		utxo, err := s.GetUTXO(ctx, hash64("contG6a1tx"), 0)
		if err != nil {
			t.Fatalf("GetUTXO: %v", err)
		}
		if utxo != nil {
			t.Errorf("rejected reapply recreated utxo_state for the orphan's own output: %+v", utxo)
		}

		genesisUTXO, err := s.GetUTXO(ctx, hash64("contG6tx"), 0)
		if err != nil {
			t.Fatalf("GetUTXO genesis: %v", err)
		}
		if genesisUTXO == nil || !genesisUTXO.Spent || genesisUTXO.SpendingTxID != hash64("contG6b1tx") {
			t.Errorf("genesis UTXO spend state disturbed by the rejected apply: %+v", genesisUTXO)
		}
	})
}

// ─── Review round: complete-block requirement (task item 2) ────────────

func TestApplyBlock_CompleteBlockRequirement(t *testing.T) {
	ctx := context.Background()

	t.Run("TxCount=1, Transactions=nil rejected, zero DB writes", func(t *testing.T) {
		s, pool := newTestStore(t)
		block := chain.Block{
			Hash: hash64("incompleteA"), Height: 100, PreviousHash: "",
			MerkleRoot: hash64("incompleteA"), Time: 1700000000, Bits: "1d00ffff",
			Difficulty: 1.0, Nonce: 0, Size: 100, Weight: 400,
			TxCount: 1, Transactions: nil,
		}
		err := s.ApplyBlock(ctx, block)
		if err == nil {
			t.Fatal("expected a header-only block to be rejected")
		}
		if !errors.Is(err, ErrIncompleteBlock) {
			t.Errorf("error = %v, want ErrIncompleteBlock", err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, hash64("incompleteA")).Scan(&count); err != nil {
			t.Fatalf("count blocks: %v", err)
		}
		if count != 0 {
			t.Errorf("header-only block persisted: count = %d", count)
		}
	})

	t.Run("TxCount=2, only one Transaction supplied rejected", func(t *testing.T) {
		s, pool := newTestStore(t)
		block := testBlock(hash64("incompleteB"), 100, "", coinbaseTx(hash64("incompleteBtx"), out(0, 1_000_000_000, "qAlice")))
		block.TxCount = 2
		err := s.ApplyBlock(ctx, block)
		if err == nil {
			t.Fatal("expected a TxCount/list mismatch to be rejected")
		}
		if !errors.Is(err, ErrIncompleteBlock) {
			t.Errorf("error = %v, want ErrIncompleteBlock", err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, hash64("incompleteB")).Scan(&count); err != nil {
			t.Fatalf("count blocks: %v", err)
		}
		if count != 0 {
			t.Errorf("mismatched block persisted: count = %d", count)
		}
	})

	t.Run("TxCount exactly equals full transaction list accepted", func(t *testing.T) {
		s, _ := newTestStore(t)
		block := testBlock(hash64("completeC"), 100, "", coinbaseTx(hash64("completeCtx"), out(0, 1_000_000_000, "qAlice")))
		if err := s.ApplyBlock(ctx, block); err != nil {
			t.Fatalf("expected a complete block to be accepted: %v", err)
		}
	})

	t.Run("checkpoint does not move on incomplete block", func(t *testing.T) {
		s, _ := newTestStore(t)
		g := testBlock(hash64("incompleteD"), 100, "", coinbaseTx(hash64("incompleteDtx"), out(0, 1_000_000_000, "qAlice")))
		mustApply(t, ctx, s, g)

		bad := testBlock(hash64("incompleteDchild"), 101, hash64("incompleteD"), coinbaseTx(hash64("incompleteDchildtx"), out(0, 1_000_000_000, "qBob")))
		bad.TxCount = 5
		if err := s.ApplyBlock(ctx, bad); err == nil {
			t.Fatal("expected an incomplete block to be rejected")
		}

		tip, err := s.Tip(ctx)
		if err != nil {
			t.Fatalf("Tip: %v", err)
		}
		if tip.Hash != hash64("incompleteD") {
			t.Errorf("Tip.Hash = %s, want %s (checkpoint must not move)", tip.Hash, hash64("incompleteD"))
		}
	})
}

// ─── Review round: immutable child-set completeness (task item 3) ──────

func TestApplyBlock_ImmutableChildSetCompleteness(t *testing.T) {
	ctx := context.Background()

	t.Run("G: same txid, added input rejected", func(t *testing.T) {
		s, _ := newTestStore(t)
		g0 := testBlock(hash64("compG0"), 100, "",
			coinbaseTx(hash64("compG0tx"), out(0, 10_000_000_000, "qAlice"), out(1, 10_000_000_000, "qAlice2")),
		)
		mustApply(t, ctx, s, g0)

		txGH := spendTx(hash64("compGtx"), hash64("compGtx"), 200,
			[]chain.Input{spendInput(0, hash64("compG0tx"), 0, nil)},
			[]chain.Output{out(0, 100, "qBob")},
		)
		a := testBlock(hash64("compGa"), 101, hash64("compG0"), txGH)
		mustApply(t, ctx, s, a)

		// Replay the same txid in an immediate-child block, but with a
		// second input added (spending the still-unspent compG0tx:1).
		txGHGrown := spendTx(hash64("compGtx"), hash64("compGtx"), 200,
			[]chain.Input{
				spendInput(0, hash64("compG0tx"), 0, nil),
				spendInput(1, hash64("compG0tx"), 1, nil),
			},
			[]chain.Output{out(0, 100, "qBob")},
		)
		b := testBlock(hash64("compGb"), 102, hash64("compGa"), txGHGrown)
		err := s.ApplyBlock(ctx, b)
		if err == nil {
			t.Fatal("expected an added input on txid replay to be rejected")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("H: same txid, omitted input rejected", func(t *testing.T) {
		s, _ := newTestStore(t)
		h0 := testBlock(hash64("compH0"), 100, "",
			coinbaseTx(hash64("compH0tx"), out(0, 10_000_000_000, "qAlice"), out(1, 10_000_000_000, "qAlice2")),
		)
		mustApply(t, ctx, s, h0)

		txH := spendTx(hash64("compHtx"), hash64("compHtx"), 200,
			[]chain.Input{
				spendInput(0, hash64("compH0tx"), 0, nil),
				spendInput(1, hash64("compH0tx"), 1, nil),
			},
			[]chain.Output{out(0, 100, "qBob")},
		)
		a := testBlock(hash64("compHa"), 101, hash64("compH0"), txH)
		mustApply(t, ctx, s, a)

		txHShrunk := spendTx(hash64("compHtx"), hash64("compHtx"), 200,
			[]chain.Input{spendInput(0, hash64("compH0tx"), 0, nil)},
			[]chain.Output{out(0, 100, "qBob")},
		)
		b := testBlock(hash64("compHb"), 102, hash64("compHa"), txHShrunk)
		err := s.ApplyBlock(ctx, b)
		if err == nil {
			t.Fatal("expected an omitted input on txid replay to be rejected")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("I: same txid, added output rejected", func(t *testing.T) {
		s, _ := newTestStore(t)
		i0 := testBlock(hash64("compI0"), 100, "",
			coinbaseTx(hash64("compI0tx"), out(0, 10_000_000_000, "qAlice")),
		)
		mustApply(t, ctx, s, i0)

		txI := spendTx(hash64("compItx"), hash64("compItx"), 200,
			[]chain.Input{spendInput(0, hash64("compI0tx"), 0, nil)},
			[]chain.Output{out(0, 100, "qBob")},
		)
		a := testBlock(hash64("compIa"), 101, hash64("compI0"), txI)
		mustApply(t, ctx, s, a)

		txIGrown := spendTx(hash64("compItx"), hash64("compItx"), 200,
			[]chain.Input{spendInput(0, hash64("compI0tx"), 0, nil)},
			[]chain.Output{out(0, 100, "qBob"), out(1, 100, "qCarol")},
		)
		b := testBlock(hash64("compIb"), 102, hash64("compIa"), txIGrown)
		err := s.ApplyBlock(ctx, b)
		if err == nil {
			t.Fatal("expected an added output on txid replay to be rejected")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("J: same txid, omitted output rejected", func(t *testing.T) {
		s, _ := newTestStore(t)
		j0 := testBlock(hash64("compJ0"), 100, "",
			coinbaseTx(hash64("compJ0tx"), out(0, 10_000_000_000, "qAlice")),
		)
		mustApply(t, ctx, s, j0)

		txJ := spendTx(hash64("compJtx"), hash64("compJtx"), 200,
			[]chain.Input{spendInput(0, hash64("compJ0tx"), 0, nil)},
			[]chain.Output{out(0, 100, "qBob"), out(1, 100, "qCarol")},
		)
		a := testBlock(hash64("compJa"), 101, hash64("compJ0"), txJ)
		mustApply(t, ctx, s, a)

		txJShrunk := spendTx(hash64("compJtx"), hash64("compJtx"), 200,
			[]chain.Input{spendInput(0, hash64("compJ0tx"), 0, nil)},
			[]chain.Output{out(0, 100, "qBob")},
		)
		b := testBlock(hash64("compJb"), 102, hash64("compJa"), txJShrunk)
		err := s.ApplyBlock(ctx, b)
		if err == nil {
			t.Fatal("expected an omitted output on txid replay to be rejected")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("K: same wtxid, added witness item rejected", func(t *testing.T) {
		s, _ := newTestStore(t)
		k0 := testBlock(hash64("compK0"), 100, "",
			coinbaseTx(hash64("compK0tx"), out(0, 10_000_000_000, "qAlice")),
		)
		mustApply(t, ctx, s, k0)

		txK := spendTx(hash64("compKtx"), hash64("compKwtx"), 200,
			[]chain.Input{spendInput(0, hash64("compK0tx"), 0, chain.WitnessStack{{0xaa}})},
			[]chain.Output{out(0, 100, "qBob")},
		)
		a := testBlock(hash64("compKa"), 101, hash64("compK0"), txK)
		mustApply(t, ctx, s, a)

		txKGrown := spendTx(hash64("compKtx"), hash64("compKwtx"), 200,
			[]chain.Input{spendInput(0, hash64("compK0tx"), 0, chain.WitnessStack{{0xaa}, {0xbb}})},
			[]chain.Output{out(0, 100, "qBob")},
		)
		b := testBlock(hash64("compKb"), 102, hash64("compKa"), txKGrown)
		err := s.ApplyBlock(ctx, b)
		if err == nil {
			t.Fatal("expected an added witness item on wtxid replay to be rejected")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("L: same wtxid, omitted witness item rejected", func(t *testing.T) {
		s, _ := newTestStore(t)
		l0 := testBlock(hash64("compL0"), 100, "",
			coinbaseTx(hash64("compL0tx"), out(0, 10_000_000_000, "qAlice")),
		)
		mustApply(t, ctx, s, l0)

		txL := spendTx(hash64("compLtx"), hash64("compLwtx"), 200,
			[]chain.Input{spendInput(0, hash64("compL0tx"), 0, chain.WitnessStack{{0xaa}, {0xbb}})},
			[]chain.Output{out(0, 100, "qBob")},
		)
		a := testBlock(hash64("compLa"), 101, hash64("compL0"), txL)
		mustApply(t, ctx, s, a)

		txLShrunk := spendTx(hash64("compLtx"), hash64("compLwtx"), 200,
			[]chain.Input{spendInput(0, hash64("compL0tx"), 0, chain.WitnessStack{{0xaa}})},
			[]chain.Output{out(0, 100, "qBob")},
		)
		b := testBlock(hash64("compLb"), 102, hash64("compLa"), txLShrunk)
		err := s.ApplyBlock(ctx, b)
		if err == nil {
			t.Fatal("expected an omitted witness item on wtxid replay to be rejected")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("M: changed vin/vout index set rejected", func(t *testing.T) {
		s, pool := newTestStore(t)
		txBad := spendTx(hash64("compMtx"), hash64("compMtx"), 200, nil, nil)
		txBad.Inputs = []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 4294967295},
			{Index: 2, Coinbase: []byte{0x52}, Sequence: 4294967295}, // should be Index 1
		}
		block := testBlock(hash64("compM"), 100, "", txBad)
		err := s.ApplyBlock(ctx, block)
		if err == nil {
			t.Fatal("expected a non-positional input index to be rejected")
		}
		if !errors.Is(err, ErrImmutableConflict) {
			t.Errorf("error = %v, want ErrImmutableConflict", err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, hash64("compM")).Scan(&count); err != nil {
			t.Fatalf("count blocks: %v", err)
		}
		if count != 0 {
			t.Errorf("rejected block persisted: count = %d", count)
		}
	})
}

// ─── Review round: safe orphan re-promotion (task item 4) ──────────────

func TestApplyBlock_OrphanRePromotion(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	a0 := testBlock(hash64("promoA0"), 100, "", coinbaseTx(hash64("promoA0tx"), out(0, 5_000_000_000, "qAlice")))
	mustApply(t, ctx, s, a0)

	spend := spendTx(hash64("promoA1tx"), hash64("promoA1tx"), 200,
		[]chain.Input{spendInput(0, hash64("promoA0tx"), 0, nil)},
		[]chain.Output{out(0, 4_999_000_000, "qBob")},
	)
	a1 := testBlock(hash64("promoA1"), 101, hash64("promoA0"), spend)
	mustApply(t, ctx, s, a1)

	if err := s.RollbackTo(ctx, hash64("promoA0")); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	var canonical bool
	var orphanedAt *string
	if err := pool.QueryRow(ctx, `SELECT canonical, orphaned_at::text FROM blocks WHERE hash = $1`, hash64("promoA1")).Scan(&canonical, &orphanedAt); err != nil {
		t.Fatalf("read orphaned block: %v", err)
	}
	if canonical || orphanedAt == nil {
		t.Fatalf("precondition failed: a1 should be orphaned, got canonical=%v orphaned_at=%v", canonical, orphanedAt)
	}

	// Re-apply the exact same a1 block: it is the immediate child of the
	// current tip (a0) again.
	if err := s.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("re-apply orphaned immediate child: %v", err)
	}

	if err := pool.QueryRow(ctx, `SELECT canonical, orphaned_at::text FROM blocks WHERE hash = $1`, hash64("promoA1")).Scan(&canonical, &orphanedAt); err != nil {
		t.Fatalf("read re-promoted block: %v", err)
	}
	if !canonical {
		t.Error("a1.canonical should be true after re-promotion")
	}
	if orphanedAt != nil {
		t.Error("a1.orphaned_at should be NULL after re-promotion")
	}

	utxo0, err := s.GetUTXO(ctx, hash64("promoA0tx"), 0)
	if err != nil {
		t.Fatalf("GetUTXO a0: %v", err)
	}
	if utxo0 == nil || !utxo0.Spent || utxo0.SpendingTxID != hash64("promoA1tx") {
		t.Errorf("a0's output should be spent by a1's tx again: %+v", utxo0)
	}
	utxo1, err := s.GetUTXO(ctx, hash64("promoA1tx"), 0)
	if err != nil {
		t.Fatalf("GetUTXO a1: %v", err)
	}
	if utxo1 == nil || utxo1.Spent {
		t.Errorf("a1's own output should be re-created as unspent: %+v", utxo1)
	}

	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != hash64("promoA1") {
		t.Errorf("Tip.Hash = %s, want %s", tip.Hash, hash64("promoA1"))
	}

	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance_satoshis FROM addresses WHERE address = 'qBob'`).Scan(&balance); err != nil {
		t.Fatalf("read qBob balance: %v", err)
	}
	if balance != 4_999_000_000 {
		t.Errorf("qBob balance = %d, want 4999000000", balance)
	}

	// The negative case — a non-immediate-child orphan must NOT be
	// promoted — is covered by TestApplyBlock_CanonicalTipContinuity's
	// "old orphan below current replacement tip" subtest.
}

// ─── Review round: concurrency / TOCTOU hardening (task item 5) ────────

func TestApplyBlock_ConcurrentCompetingChildrenSerialize(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("concG"), 100, "", coinbaseTx(hash64("concGtx"), out(0, 1_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)

	childA := testBlock(hash64("concChildA"), 101, hash64("concG"), coinbaseTx(hash64("concChildAtx"), out(0, 1_000_000_000, "qBob")))
	childB := testBlock(hash64("concChildB"), 101, hash64("concG"), coinbaseTx(hash64("concChildBtx"), out(0, 1_000_000_000, "qCarol")))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = s.ApplyBlock(ctx, childA) }()
	go func() { defer wg.Done(); errs[1] = s.ApplyBlock(ctx, childB) }()
	wg.Wait()

	succeeded, nonSequential := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			succeeded++
		case errors.Is(e, ErrNonSequentialBlock):
			nonSequential++
		}
	}
	// The canonical-mutation lock (sync_state FOR UPDATE) fully serializes
	// the two ApplyBlock calls: whichever commits second re-reads the
	// checkpoint fresh (only possible once the first has released the
	// lock) and its own continuity check cleanly rejects itself with
	// ErrNonSequentialBlock — never a raw, confusing database constraint
	// violation from racing directly against the unique canonical-height
	// index.
	if succeeded != 1 || nonSequential != 1 {
		t.Fatalf("expected exactly one success and one clean ErrNonSequentialBlock (proving the checkpoint lock serialized and re-validated), got succeeded=%d nonSequential=%d errs=%v", succeeded, nonSequential, errs)
	}

	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != hash64("concChildA") && tip.Hash != hash64("concChildB") {
		t.Fatalf("Tip.Hash = %s, want one of the two competing children", tip.Hash)
	}

	var canonicalCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE height = 101 AND canonical`).Scan(&canonicalCount); err != nil {
		t.Fatalf("count canonical blocks at height 101: %v", err)
	}
	if canonicalCount != 1 {
		t.Errorf("canonical blocks at height 101 = %d, want exactly 1", canonicalCount)
	}
}

func TestRollbackTo_SerializesAgainstApplyBlock(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("concRG"), 100, "", coinbaseTx(hash64("concRGtx"), out(0, 1_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)

	child := testBlock(hash64("concRChild"), 101, hash64("concRG"), coinbaseTx(hash64("concRChildtx"), out(0, 1_000_000_000, "qBob")))

	var wg sync.WaitGroup
	var applyErr, rollbackErr error
	wg.Add(2)
	go func() { defer wg.Done(); applyErr = s.ApplyBlock(ctx, child) }()
	go func() { defer wg.Done(); rollbackErr = s.RollbackTo(ctx, hash64("concRG")) }()
	wg.Wait()

	// RollbackTo(g) is valid regardless of ordering relative to
	// ApplyBlock(child): if it runs first, there's nothing above g yet
	// (a no-op); if it runs after, it orphans child. Both must succeed
	// without error under the canonical-mutation lock — the lock's job is
	// to serialize them, not to make either one fail.
	if applyErr != nil {
		t.Errorf("ApplyBlock: %v", applyErr)
	}
	if rollbackErr != nil {
		t.Errorf("RollbackTo: %v", rollbackErr)
	}

	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	switch tip.Hash {
	case hash64("concRChild"):
		var canonical bool
		if err := pool.QueryRow(ctx, `SELECT canonical FROM blocks WHERE hash = $1`, hash64("concRChild")).Scan(&canonical); err != nil {
			t.Fatalf("read child canonical status: %v", err)
		}
		if !canonical {
			t.Error("tip is child but child.canonical = false — inconsistent state")
		}
	case hash64("concRG"):
		// RollbackTo ran last (or child was never applied before it ran);
		// consistent either way.
	default:
		t.Fatalf("unexpected tip after concurrent Apply/Rollback: %s", tip.Hash)
	}
}

func TestMarkSpent_ZeroRowUpdateDetectsConflict(t *testing.T) {
	ctx, tx := txPool(t)

	mustExec(t, ctx, tx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, 100, NULL, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 2)
	`, hash64("markspentblock"))
	fixtureSimpleTransaction(t, ctx, tx, hash64("markspenttx"), true)
	fixtureBlockTransaction(t, ctx, tx, hash64("markspentblock"), hash64("markspenttx"), hash64("markspenttx"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("markspenttx"), 0, "p2pkh", 1000)
	mustExec(t, ctx, tx, `INSERT INTO utxo_state (txid, vout_index, creation_block_hash) VALUES ($1, 0, $2)`,
		hash64("markspenttx"), hash64("markspentblock"))

	fixtureSimpleTransaction(t, ctx, tx, hash64("spenderX"), false)
	fixtureTransactionInput(t, ctx, tx, hash64("spenderX"), 0, hash64("markspenttx"), 0)
	fixtureBlockTransaction(t, ctx, tx, hash64("markspentblock"), hash64("spenderX"), hash64("spenderX"), 1)
	mustExec(t, ctx, tx, `
		UPDATE utxo_state SET spent = true, spending_txid = $1, spending_vin_index = 0, spending_block_hash = $2
		WHERE txid = $3 AND vout_index = 0
	`, hash64("spenderX"), hash64("markspentblock"), hash64("markspenttx"))

	// A different spender Y attempting to mark the same (already-spent-by-
	// X) output must be rejected — the UPDATE affects 0 rows, and
	// markSpent must not treat that as silent success.
	_, err := markSpent(ctx, tx, hash64("markspenttx"), 0, hash64("spenderY"), 0, hash64("markspentblock"))
	if err == nil {
		t.Fatal("expected markSpent to reject a zero-row UPDATE caused by a different spender, got nil")
	}
	if !errors.Is(err, ErrDoubleSpend) {
		t.Errorf("error = %v, want ErrDoubleSpend", err)
	}

	// The exact existing spender (X) re-marking it is the idempotent
	// replay case — also a zero-row UPDATE, but must succeed, not error.
	if _, err := markSpent(ctx, tx, hash64("markspenttx"), 0, hash64("spenderX"), 0, hash64("markspentblock")); err != nil {
		t.Fatalf("expected markSpent to succeed for the exact existing spender (idempotent), got: %v", err)
	}
}
