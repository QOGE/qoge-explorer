package store

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/accounting"
	"github.com/QOGE/qoge-explorer/internal/chain"
)

// era0Subsidy is the block subsidy for every height these tests use (all
// well below the first halving at 500,000) — accounting.InitialSubsidySatoshis,
// referenced by name rather than a magic number so the intent (era-0
// subsidy) stays obvious at every call site.
const era0Subsidy = accounting.InitialSubsidySatoshis

// ─── A: per-block basic accounting (spec section 43) ────────────────────

func TestApplyBlock_AccountingPerBlockBasic(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("acctA0"), 100, "", coinbaseTx(hash64("acctA0tx"), out(0, 5_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)

	const fee = int64(100_000_000)
	spend := spendTx(hash64("acctASpend"), hash64("acctASpend"), 200,
		[]chain.Input{spendInput(0, hash64("acctA0tx"), 0, nil)},
		[]chain.Output{out(0, 5_000_000_000-fee, "qBob")},
	)
	cb := coinbaseTx(hash64("acctACb"), out(0, era0Subsidy+fee, "qMiner"))
	block1 := testBlock(hash64("acctA1"), 101, hash64("acctA0"), cb, spend)
	mustApply(t, ctx, s, block1)

	acc, err := s.GetBlockAccounting(ctx, block1.Hash)
	if err != nil {
		t.Fatalf("GetBlockAccounting: %v", err)
	}
	if acc == nil {
		t.Fatal("expected a block_accounting row")
	}
	if acc.SubsidySatoshis != era0Subsidy {
		t.Errorf("SubsidySatoshis = %d, want %d", acc.SubsidySatoshis, era0Subsidy)
	}
	if acc.FeeSatoshis != fee {
		t.Errorf("FeeSatoshis = %d, want %d", acc.FeeSatoshis, fee)
	}
	if acc.CoinbaseOutputSatoshis != era0Subsidy+fee {
		t.Errorf("CoinbaseOutputSatoshis = %d, want %d", acc.CoinbaseOutputSatoshis, era0Subsidy+fee)
	}
	if acc.UnclaimedRewardSatoshis != 0 {
		t.Errorf("UnclaimedRewardSatoshis = %d, want 0 (exact claim)", acc.UnclaimedRewardSatoshis)
	}
}

// ─── B: underclaimed reward is valid (spec section 44) ──────────────────

func TestApplyBlock_AccountingUnderclaimedRewardIsValid(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("acctB0"), 100, "", coinbaseTx(hash64("acctB0tx"), out(0, 5_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)

	const fee = int64(100_000_000)
	const shortfall = int64(12_345)
	spend := spendTx(hash64("acctBSpend"), hash64("acctBSpend"), 200,
		[]chain.Input{spendInput(0, hash64("acctB0tx"), 0, nil)},
		[]chain.Output{out(0, 5_000_000_000-fee, "qBob")},
	)
	cb := coinbaseTx(hash64("acctBCb"), out(0, era0Subsidy+fee-shortfall, "qMiner"))
	block1 := testBlock(hash64("acctB1"), 101, hash64("acctB0"), cb, spend)
	if err := s.ApplyBlock(ctx, block1); err != nil {
		t.Fatalf("underclaiming the reward must be accepted, not rejected: %v", err)
	}

	acc, err := s.GetBlockAccounting(ctx, block1.Hash)
	if err != nil || acc == nil {
		t.Fatalf("GetBlockAccounting: acc=%+v err=%v", acc, err)
	}
	if acc.UnclaimedRewardSatoshis != shortfall {
		t.Errorf("UnclaimedRewardSatoshis = %d, want %d", acc.UnclaimedRewardSatoshis, shortfall)
	}
}

// ─── C: overclaim rejects the whole block (spec section 45) ─────────────

func TestApplyBlock_AccountingOverclaimRejectsWholeBlock(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("acctC0"), 100, "", coinbaseTx(hash64("acctC0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	tipBefore, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip (before): %v", err)
	}

	// Claims one satoshi more than subsidy+fees(0) — Core's own
	// ConnectBlock would reject this exact shape ("bad-cb-amount").
	cb := coinbaseTx(hash64("acctCCb"), out(0, era0Subsidy+1, "qMiner"))
	block1 := testBlock(hash64("acctC1"), 101, hash64("acctC0"), cb)
	err = s.ApplyBlock(ctx, block1)
	if err == nil {
		t.Fatal("expected an overclaiming coinbase to be rejected")
	}
	if !errors.Is(err, accounting.ErrCoinbaseOverclaim) {
		t.Errorf("error = %v, want accounting.ErrCoinbaseOverclaim", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, block1.Hash).Scan(&count); err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if count != 0 {
		t.Errorf("rejected block persisted: count = %d", count)
	}

	tipAfter, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip (after): %v", err)
	}
	if tipAfter != tipBefore {
		t.Errorf("checkpoint moved despite rejected block: before=%+v after=%+v", tipBefore, tipAfter)
	}
}

// ─── D: zero fee (spec section 46) ───────────────────────────────────────

func TestApplyBlock_AccountingZeroFee(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("acctD0"), 100, "", coinbaseTx(hash64("acctD0tx"), out(0, 5_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)

	// inputSum == outputSum: fee is exactly zero.
	spend := spendTx(hash64("acctDSpend"), hash64("acctDSpend"), 200,
		[]chain.Input{spendInput(0, hash64("acctD0tx"), 0, nil)},
		[]chain.Output{out(0, 5_000_000_000, "qBob")},
	)
	cb := coinbaseTx(hash64("acctDCb"), out(0, era0Subsidy, "qMiner"))
	block1 := testBlock(hash64("acctD1"), 101, hash64("acctD0"), cb, spend)
	mustApply(t, ctx, s, block1)

	acc, err := s.GetBlockAccounting(ctx, block1.Hash)
	if err != nil || acc == nil {
		t.Fatalf("GetBlockAccounting: acc=%+v err=%v", acc, err)
	}
	if acc.FeeSatoshis != 0 {
		t.Errorf("FeeSatoshis = %d, want 0", acc.FeeSatoshis)
	}
}

// ─── E: multiple transaction fees sum exactly (spec section 47) ─────────

func TestApplyBlock_AccountingMultipleTxFeesSumExactly(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("acctE0"), 100, "",
		coinbaseTx(hash64("acctE0tx"), out(0, 5_000_000_000, "qAlice"), out(1, 3_000_000_000, "qCarl")),
	)
	mustApply(t, ctx, s, g)

	const fee1 = int64(50_000_000)
	const fee2 = int64(20_000_000)
	spend1 := spendTx(hash64("acctESpend1"), hash64("acctESpend1"), 200,
		[]chain.Input{spendInput(0, hash64("acctE0tx"), 0, nil)},
		[]chain.Output{out(0, 5_000_000_000-fee1, "qBob")},
	)
	spend2 := spendTx(hash64("acctESpend2"), hash64("acctESpend2"), 200,
		[]chain.Input{spendInput(0, hash64("acctE0tx"), 1, nil)},
		[]chain.Output{out(0, 3_000_000_000-fee2, "qDave")},
	)
	cb := coinbaseTx(hash64("acctECb"), out(0, era0Subsidy+fee1+fee2, "qMiner"))
	block1 := testBlock(hash64("acctE1"), 101, hash64("acctE0"), cb, spend1, spend2)
	mustApply(t, ctx, s, block1)

	acc, err := s.GetBlockAccounting(ctx, block1.Hash)
	if err != nil || acc == nil {
		t.Fatalf("GetBlockAccounting: acc=%+v err=%v", acc, err)
	}
	if acc.FeeSatoshis != fee1+fee2 {
		t.Errorf("FeeSatoshis = %d, want %d", acc.FeeSatoshis, fee1+fee2)
	}
}

// ─── F: checked-arithmetic overflow (spec section 48) ───────────────────
//
// A full ApplyBlock economic scenario cannot legitimately construct
// near-int64-max values (every satoshi ultimately traces back to some
// coinbase, and every coinbase is now bounded by its block's subsidy —
// see TestApplyBlock_AccountingOverclaimRejectsWholeBlock), so these
// target the actual checked-arithmetic primitives directly: sumOutputValues
// backs the coinbase-output accumulation in applyBlockAccounting, and
// addChecked backs BOTH the block-level fee accumulation loop in
// ApplyBlock and sumOutputValues itself. See
// TestComputeBlockFacts_SubsidyPlusFeesOverflowRejected
// (internal/accounting) for the third leg, "subsidy + fees".

func TestSumOutputValues_OverflowRejected(t *testing.T) {
	outputs := []chain.Output{
		{Index: 0, Value: chain.Amount(math.MaxInt64)},
		{Index: 1, Value: chain.Amount(1)},
	}
	if _, ok := sumOutputValues(outputs); ok {
		t.Fatal("sumOutputValues: expected overflow to be rejected")
	}
}

func TestAddChecked_OverflowRejected(t *testing.T) {
	if _, ok := addChecked(math.MaxInt64, 1); ok {
		t.Fatal("addChecked: expected positive overflow to be rejected")
	}
	if _, ok := addChecked(math.MinInt64, -1); ok {
		t.Fatal("addChecked: expected negative overflow to be rejected")
	}
	if sum, ok := addChecked(100, 200); !ok || sum != 300 {
		t.Fatalf("addChecked(100, 200) = %d, %v, want 300, true", sum, ok)
	}
}

// ─── G: replay is a safe no-op (spec section 49) ─────────────────────────

func TestApplyBlock_AccountingReplayIsNoOp(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("acctG0"), 100, "", coinbaseTx(hash64("acctG0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	// Exact tip replay is an explicitly supported ApplyBlock case.
	if err := s.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("replay: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_accounting WHERE block_hash = $1`, g.Hash).Scan(&count); err != nil {
		t.Fatalf("count block_accounting: %v", err)
	}
	if count != 1 {
		t.Errorf("block_accounting row count = %d, want 1 (no duplicate from replay)", count)
	}

	acc, err := s.GetBlockAccounting(ctx, g.Hash)
	if err != nil || acc == nil {
		t.Fatalf("GetBlockAccounting: acc=%+v err=%v", acc, err)
	}
	if acc.SubsidySatoshis != era0Subsidy || acc.CoinbaseOutputSatoshis != era0Subsidy || acc.UnclaimedRewardSatoshis != 0 {
		t.Errorf("unexpected accounting after replay: %+v", acc)
	}
}

// ─── H: contradictory accounting is rejected, never overwritten (spec
// section 50) ─────────────────────────────────────────────────────────────

func TestApplyBlock_AccountingImmutableConflict(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("acctH0"), 100, "", coinbaseTx(hash64("acctH0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	// There is no organic ApplyBlock path that produces a contradiction
	// for an already-persisted block_hash (the block's own content
	// deterministically determines its accounting) — this directly
	// corrupts the already-correct row as the test seam, exactly as spec
	// section 55 prescribes for the equivalent backfill scenario. The
	// reward identity (coinbase + unclaimed == subsidy + fee) is kept
	// satisfied by constructing the corruption to preserve it, isolating
	// this test to the idempotent-verify comparison alone.
	if _, err := pool.Exec(ctx, `
		UPDATE block_accounting SET fee_satoshis = fee_satoshis + 1, coinbase_output_satoshis = coinbase_output_satoshis + 1
		WHERE block_hash = $1
	`, g.Hash); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}

	_, err := s.BackfillBlockAccounting(ctx, g.Hash)
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("error = %v, want ErrImmutableConflict", err)
	}
}

// ─── I: reorg keeps accounting as audit history (spec section 51) ───────

func TestRollbackTo_AccountingSurvivesReorg(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	a := testBlock(hash64("acctIA"), 100, "", coinbaseTx(hash64("acctIAtx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, a)
	b := testBlock(hash64("acctIB"), 101, hash64("acctIA"), coinbaseTx(hash64("acctIBtx"), out(0, era0Subsidy, "qBob")))
	mustApply(t, ctx, s, b)
	c := testBlock(hash64("acctIC"), 102, hash64("acctIB"), coinbaseTx(hash64("acctICtx"), out(0, era0Subsidy, "qCarol")))
	mustApply(t, ctx, s, c)

	accBBefore, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || accBBefore == nil {
		t.Fatalf("GetBlockAccounting(B) before: acc=%+v err=%v", accBBefore, err)
	}
	accCBefore, err := s.GetBlockAccounting(ctx, c.Hash)
	if err != nil || accCBefore == nil {
		t.Fatalf("GetBlockAccounting(C) before: acc=%+v err=%v", accCBefore, err)
	}

	if err := s.RollbackTo(ctx, a.Hash); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	accBAfter, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || accBAfter == nil {
		t.Fatalf("blockB accounting should survive rollback: acc=%+v err=%v", accBAfter, err)
	}
	if *accBAfter != *accBBefore {
		t.Errorf("blockB accounting changed across rollback: before=%+v after=%+v", accBBefore, accBAfter)
	}
	accCAfter, err := s.GetBlockAccounting(ctx, c.Hash)
	if err != nil || accCAfter == nil {
		t.Fatalf("blockC accounting should survive rollback: acc=%+v err=%v", accCAfter, err)
	}
	if *accCAfter != *accCBefore {
		t.Errorf("blockC accounting changed across rollback: before=%+v after=%+v", accCBefore, accCAfter)
	}

	// Replacement branch: A -> D -> E.
	d := testBlock(hash64("acctID"), 101, hash64("acctIA"), coinbaseTx(hash64("acctIDtx"), out(0, era0Subsidy, "qDave")))
	mustApply(t, ctx, s, d)
	e := testBlock(hash64("acctIE"), 102, hash64("acctID"), coinbaseTx(hash64("acctIEtx"), out(0, era0Subsidy, "qEve")))
	mustApply(t, ctx, s, e)

	accD, err := s.GetBlockAccounting(ctx, d.Hash)
	if err != nil || accD == nil {
		t.Fatalf("blockD should have its own accounting row: acc=%+v err=%v", accD, err)
	}
	accE, err := s.GetBlockAccounting(ctx, e.Hash)
	if err != nil || accE == nil {
		t.Fatalf("blockE should have its own accounting row: acc=%+v err=%v", accE, err)
	}

	// Old B/C accounting remains as audit history, untouched.
	accBFinal, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || accBFinal == nil || *accBFinal != *accBBefore {
		t.Fatalf("blockB accounting should remain unchanged audit history: acc=%+v err=%v", accBFinal, err)
	}
	accCFinal, err := s.GetBlockAccounting(ctx, c.Hash)
	if err != nil || accCFinal == nil || *accCFinal != *accCBefore {
		t.Fatalf("blockC accounting should remain unchanged audit history: acc=%+v err=%v", accCFinal, err)
	}
}

// ─── J: orphan re-promotion never rewrites accounting (spec section 52) ──

func TestApplyBlock_OrphanRepromotionAccountingUnchanged(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	a := testBlock(hash64("acctJA"), 100, "", coinbaseTx(hash64("acctJAtx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, a)
	b := testBlock(hash64("acctJB"), 101, hash64("acctJA"), coinbaseTx(hash64("acctJBtx"), out(0, era0Subsidy, "qBob")))
	mustApply(t, ctx, s, b)

	accBefore, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || accBefore == nil {
		t.Fatalf("GetBlockAccounting before: acc=%+v err=%v", accBefore, err)
	}

	if err := s.RollbackTo(ctx, a.Hash); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	// Re-apply the SAME block B: immediate child of the current tip A,
	// same hash, same content — ApplyBlock's documented orphan
	// re-promotion path.
	if err := s.ApplyBlock(ctx, b); err != nil {
		t.Fatalf("re-promote B: %v", err)
	}

	var canonical bool
	if err := pool.QueryRow(ctx, `SELECT canonical FROM blocks WHERE hash = $1`, b.Hash).Scan(&canonical); err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	if !canonical {
		t.Fatal("fixture bug: block B should be canonical again after re-promotion")
	}

	accAfter, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || accAfter == nil {
		t.Fatalf("GetBlockAccounting after: acc=%+v err=%v", accAfter, err)
	}
	if *accAfter != *accBefore {
		t.Errorf("block_accounting changed across orphan re-promotion: before=%+v after=%+v", accBefore, accAfter)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_accounting WHERE block_hash = $1`, b.Hash).Scan(&count); err != nil {
		t.Fatalf("count block_accounting: %v", err)
	}
	if count != 1 {
		t.Errorf("block_accounting row count = %d, want 1 (no replacement/update)", count)
	}
}

// ─── K: backfill reconstructs a legacy database (spec section 53) ───────

func TestBackfillAccounting_LegacyDatabase(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	a := testBlock(hash64("acctKA"), 100, "", coinbaseTx(hash64("acctKAtx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, a)
	b := testBlock(hash64("acctKB"), 101, hash64("acctKA"), coinbaseTx(hash64("acctKBtx"), out(0, era0Subsidy, "qBob")))
	mustApply(t, ctx, s, b)

	// Orphan b, then apply a replacement — b remains a stored, orphaned
	// block; the backfill must cover it too (spec section 33/53: canonical
	// AND orphan).
	if err := s.RollbackTo(ctx, a.Hash); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	bReplacement := testBlock(hash64("acctKB2"), 101, hash64("acctKA"), coinbaseTx(hash64("acctKB2tx"), out(0, era0Subsidy, "qBobReplacement")))
	mustApply(t, ctx, s, bReplacement)

	// Simulate a legacy pre-2H.1 database: no block_accounting rows at all.
	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting`); err != nil {
		t.Fatalf("simulate legacy state: %v", err)
	}

	before, err := CheckAccountingCompleteness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckAccountingCompleteness (before): %v", err)
	}
	if before.MissingCount != 3 {
		t.Fatalf("fixture bug: MissingCount = %d, want 3 (a, orphaned b, bReplacement)", before.MissingCount)
	}

	result, err := s.BackfillAccounting(ctx)
	if err != nil {
		t.Fatalf("BackfillAccounting: %v", err)
	}
	if result.TotalBlocks != 3 || result.Inserted != 3 || result.Verified != 0 {
		t.Errorf("BackfillAccounting result = %+v, want {TotalBlocks:3 Inserted:3 Verified:0}", result)
	}

	after, err := CheckAccountingCompleteness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckAccountingCompleteness (after): %v", err)
	}
	if after.MissingCount != 0 {
		t.Errorf("MissingCount after backfill = %d, want 0", after.MissingCount)
	}

	// The orphaned block was backfilled too, not skipped.
	accOrphan, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || accOrphan == nil {
		t.Fatalf("orphaned block b should have been backfilled: acc=%+v err=%v", accOrphan, err)
	}

	// Idempotent: running it again changes nothing and re-verifies every row.
	result2, err := s.BackfillAccounting(ctx)
	if err != nil {
		t.Fatalf("second BackfillAccounting: %v", err)
	}
	if result2.TotalBlocks != 3 || result2.Inserted != 0 || result2.Verified != 3 {
		t.Errorf("second BackfillAccounting result = %+v, want {TotalBlocks:3 Inserted:0 Verified:3}", result2)
	}
}

// ─── L: backfill resumes after interruption (spec section 54) ───────────

func TestBackfillAccounting_ResumesAfterInterruption(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	a := testBlock(hash64("acctLA"), 100, "", coinbaseTx(hash64("acctLAtx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, a)
	b := testBlock(hash64("acctLB"), 101, hash64("acctLA"), coinbaseTx(hash64("acctLBtx"), out(0, era0Subsidy, "qBob")))
	mustApply(t, ctx, s, b)

	// Simulate an interrupted prior run: only b's row is missing.
	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting WHERE block_hash = $1`, b.Hash); err != nil {
		t.Fatalf("simulate interruption: %v", err)
	}

	result, err := s.BackfillAccounting(ctx)
	if err != nil {
		t.Fatalf("BackfillAccounting: %v", err)
	}
	if result.TotalBlocks != 2 {
		t.Errorf("TotalBlocks = %d, want 2", result.TotalBlocks)
	}
	if result.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1 (only b was missing)", result.Inserted)
	}
	if result.Verified != 1 {
		t.Errorf("Verified = %d, want 1 (a was already correct)", result.Verified)
	}

	after, err := CheckAccountingCompleteness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckAccountingCompleteness: %v", err)
	}
	if after.MissingCount != 0 {
		t.Errorf("MissingCount = %d, want 0", after.MissingCount)
	}
}

// ─── M: backfill detects, never overwrites, a corrupted row (spec section
// 55) ──────────────────────────────────────────────────────────────────

func TestBackfillAccounting_ConflictDetected(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	a := testBlock(hash64("acctMA"), 100, "", coinbaseTx(hash64("acctMAtx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, a)

	if _, err := pool.Exec(ctx, `
		UPDATE block_accounting SET fee_satoshis = fee_satoshis + 1, coinbase_output_satoshis = coinbase_output_satoshis + 1
		WHERE block_hash = $1
	`, a.Hash); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}

	_, err := s.BackfillAccounting(ctx)
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("error = %v, want ErrImmutableConflict", err)
	}

	// The corrupted row must not have been silently overwritten.
	var feeSatoshis int64
	if err := pool.QueryRow(ctx, `SELECT fee_satoshis FROM block_accounting WHERE block_hash = $1`, a.Hash).Scan(&feeSatoshis); err != nil {
		t.Fatalf("read corrupted row: %v", err)
	}
	if feeSatoshis != 1 {
		t.Errorf("corrupted row was modified: fee_satoshis = %d, want 1 (the corrupted value)", feeSatoshis)
	}
}

// ─── N: completeness reporting (spec section 56) ─────────────────────────

func TestCheckAccountingCompleteness_ReportsMissingThenComplete(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	a := testBlock(hash64("acctNA"), 100, "", coinbaseTx(hash64("acctNAtx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, a)
	b := testBlock(hash64("acctNB"), 101, hash64("acctNA"), coinbaseTx(hash64("acctNBtx"), out(0, era0Subsidy, "qBob")))
	mustApply(t, ctx, s, b)

	full, err := CheckAccountingCompleteness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckAccountingCompleteness: %v", err)
	}
	if full.MissingCount != 0 {
		t.Fatalf("fixture bug: MissingCount = %d, want 0 (ApplyBlock already wrote both rows)", full.MissingCount)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting WHERE block_hash = $1`, b.Hash); err != nil {
		t.Fatalf("delete row: %v", err)
	}

	missing, err := CheckAccountingCompleteness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckAccountingCompleteness: %v", err)
	}
	if missing.MissingCount != 1 {
		t.Errorf("MissingCount = %d, want 1", missing.MissingCount)
	}
	if missing.BlockCount != 2 {
		t.Errorf("BlockCount = %d, want 2", missing.BlockCount)
	}
	if missing.AccountingCount != 1 {
		t.Errorf("AccountingCount = %d, want 1", missing.AccountingCount)
	}

	if _, err := s.BackfillAccounting(ctx); err != nil {
		t.Fatalf("BackfillAccounting: %v", err)
	}

	complete, err := CheckAccountingCompleteness(ctx, pool)
	if err != nil {
		t.Fatalf("CheckAccountingCompleteness: %v", err)
	}
	if complete.MissingCount != 0 {
		t.Errorf("MissingCount after backfill = %d, want 0", complete.MissingCount)
	}
}

// ─── O: the advisory lock rejects a concurrent backfill run ─────────────

func TestBackfillAccounting_ConcurrentRunRejected(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, backfillAccountingAdvisoryLockKey).Scan(&locked); err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}
	if !locked {
		t.Fatal("fixture bug: could not acquire the advisory lock to simulate a concurrent run")
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, backfillAccountingAdvisoryLockKey)
	}()

	_, err = s.BackfillAccounting(ctx)
	if !errors.Is(err, ErrBackfillAlreadyRunning) {
		t.Fatalf("error = %v, want ErrBackfillAlreadyRunning", err)
	}
}
