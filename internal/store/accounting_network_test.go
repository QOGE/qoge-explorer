package store

import (
	"context"
	"errors"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/accounting"
	"github.com/QOGE/qoge-explorer/internal/chain"
)

// ─── Phase 2H.1 internal review correction: network-aware subsidy ───────
//
// A prior version of this package's Store always used mainnet's 500,000-
// block halving interval, regardless of which network it was actually
// indexing. Because underclaiming a block's reward is valid chain state,
// that bug did not fail loudly: a real 50 QOGE regtest coinbase at height
// 150 would have been silently recorded as subsidy=100 QOGE,
// unclaimed=50 QOGE — a plausible-looking but false accounting fact. These
// tests prove Store.ApplyBlock and Store.BackfillBlockAccounting both use
// the Store's own accountingSchedule (bound explicitly via NewForNetwork),
// not a hardcoded mainnet interval, and that the two paths agree.

// regtestQOGE mirrors accounting.RegtestSchedule.InitialSubsidySatoshis —
// 100 QOGE in satoshis, identical to every other network's initial
// subsidy; only the halving interval differs for regtest.
var regtestQOGE = accounting.RegtestSchedule.InitialSubsidySatoshis

func TestApplyBlock_RegtestSubsidyBoundary(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	s, err := NewForNetwork(pool, "regtest")
	if err != nil {
		t.Fatalf("NewForNetwork(regtest): %v", err)
	}

	// Bootstrap at height 149 — still era 0 (regtest halving interval is
	// 150, so height 149 is the LAST block of era 0, subsidy 100 QOGE).
	g := testBlock(hash64("regtestG"), 149, "", coinbaseTx(hash64("regtestGtx"), out(0, regtestQOGE, "qAlice")))
	mustApply(t, ctx, s, g)

	accG, err := s.GetBlockAccounting(ctx, g.Hash)
	if err != nil || accG == nil {
		t.Fatalf("GetBlockAccounting(height 149): acc=%+v err=%v", accG, err)
	}
	if accG.SubsidySatoshis != regtestQOGE {
		t.Fatalf("height 149 SubsidySatoshis = %d, want %d (still era 0 on regtest)", accG.SubsidySatoshis, regtestQOGE)
	}

	// Height 150 is regtest's FIRST halving boundary — Core subsidy here
	// is 50 QOGE, NOT mainnet's 100 QOGE (which would still apply on
	// mainnet at this height, since mainnet's halving interval is
	// 500,000). A miner claiming exactly the correct 50 QOGE must record
	// SubsidySatoshis=50 QOGE and UnclaimedRewardSatoshis=0 — the bug this
	// correction fixes would have recorded SubsidySatoshis=100 QOGE and
	// UnclaimedRewardSatoshis=50 QOGE (an INVENTED, false unclaimed
	// reward), because underclaim is normally valid and so the mismatch
	// would never have surfaced as an error.
	halfSubsidy := regtestQOGE / 2
	b := testBlock(hash64("regtest150"), 150, hash64("regtestG"), coinbaseTx(hash64("regtest150tx"), out(0, halfSubsidy, "qBob")))
	mustApply(t, ctx, s, b)

	accB, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || accB == nil {
		t.Fatalf("GetBlockAccounting(height 150): acc=%+v err=%v", accB, err)
	}
	if accB.SubsidySatoshis != halfSubsidy {
		t.Fatalf("height 150 SubsidySatoshis = %d, want %d (regtest's first halving, NOT mainnet's 500000-block interval)",
			accB.SubsidySatoshis, halfSubsidy)
	}
	if accB.UnclaimedRewardSatoshis != 0 {
		t.Fatalf("height 150 UnclaimedRewardSatoshis = %d, want 0 (miner claimed exactly the correct regtest subsidy) — "+
			"a nonzero value here would mean an invented, false unclaimed reward from using the wrong schedule",
			accB.UnclaimedRewardSatoshis)
	}
}

// TestBackfillAccounting_RegtestSchedule proves BackfillBlockAccounting
// uses the SAME per-network schedule the live ApplyBlock path used: a
// block backfilled under an explicitly regtest-scoped Store must reproduce
// byte-identical accounting to what ApplyBlock originally computed live,
// including across regtest's 150-block halving boundary.
func TestBackfillAccounting_RegtestSchedule(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	s, err := NewForNetwork(pool, "regtest")
	if err != nil {
		t.Fatalf("NewForNetwork(regtest): %v", err)
	}

	g := testBlock(hash64("regtestBfG"), 149, "", coinbaseTx(hash64("regtestBfGtx"), out(0, regtestQOGE, "qAlice")))
	mustApply(t, ctx, s, g)
	halfSubsidy := regtestQOGE / 2
	b := testBlock(hash64("regtestBf150"), 150, hash64("regtestBfG"), coinbaseTx(hash64("regtestBf150tx"), out(0, halfSubsidy, "qBob")))
	mustApply(t, ctx, s, b)

	liveAcc, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || liveAcc == nil {
		t.Fatalf("GetBlockAccounting (live): acc=%+v err=%v", liveAcc, err)
	}
	if liveAcc.SubsidySatoshis != halfSubsidy {
		t.Fatalf("fixture bug: live SubsidySatoshis = %d, want %d", liveAcc.SubsidySatoshis, halfSubsidy)
	}

	// Simulate a legacy database (block_accounting missing), then
	// reconstruct it through the SAME regtest-scoped Store's backfill path.
	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting WHERE block_hash = $1`, b.Hash); err != nil {
		t.Fatalf("simulate legacy state: %v", err)
	}

	fresh, err := s.BackfillBlockAccounting(ctx, b.Hash)
	if err != nil {
		t.Fatalf("BackfillBlockAccounting: %v", err)
	}
	if !fresh {
		t.Fatal("expected a freshly inserted row")
	}

	backfilledAcc, err := s.GetBlockAccounting(ctx, b.Hash)
	if err != nil || backfilledAcc == nil {
		t.Fatalf("GetBlockAccounting (backfilled): acc=%+v err=%v", backfilledAcc, err)
	}
	if backfilledAcc.SubsidySatoshis != halfSubsidy {
		t.Fatalf("backfilled SubsidySatoshis = %d, want %d (must match the regtest schedule, not mainnet's)",
			backfilledAcc.SubsidySatoshis, halfSubsidy)
	}
	if *backfilledAcc != *liveAcc {
		t.Fatalf("backfilled accounting disagrees with live accounting: live=%+v backfilled=%+v — live and backfill paths must use the same schedule",
			liveAcc, backfilledAcc)
	}
}

// ─── Backfill source-data completeness (NULL fee / missing coinbase
// outputs) ─────────────────────────────────────────────────────────────

// TestBackfillBlockAccounting_NullFeeRejected proves a NULL
// transactions.fee_satoshis for a non-coinbase block occurrence (permitted
// by the frozen migration 0001 schema) is detected and rejected rather
// than silently summed as zero by SQL's SUM(), which would otherwise
// produce a plausible-looking but wrong (understated) fee total and a
// correspondingly inflated unclaimed_reward_satoshis.
func TestBackfillBlockAccounting_NullFeeRejected(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("nullfeeG"), 100, "", coinbaseTx(hash64("nullfeeGtx"), out(0, 5_000_000_000, "qAlice")))
	mustApply(t, ctx, s, g)

	const fee = int64(1000)
	spend := spendTx(hash64("nullfeeSpend"), hash64("nullfeeSpend"), 200,
		[]chain.Input{spendInput(0, hash64("nullfeeGtx"), 0, nil)},
		[]chain.Output{out(0, 5_000_000_000-fee, "qBob")},
	)
	cb := coinbaseTx(hash64("nullfeeCb"), out(0, era0Subsidy+fee, "qMiner"))
	block1 := testBlock(hash64("nullfee1"), 101, hash64("nullfeeG"), cb, spend)
	mustApply(t, ctx, s, block1)

	// Simulate a legacy/incomplete indexed database: drop the (correct)
	// live accounting row, then null out the non-coinbase transaction's
	// fee — exactly what an older indexer run that failed to compute fees
	// for some transactions could have left behind.
	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting WHERE block_hash = $1`, block1.Hash); err != nil {
		t.Fatalf("delete accounting row: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE transactions SET fee_satoshis = NULL WHERE txid = $1`, spend.TxID); err != nil {
		t.Fatalf("null out fee: %v", err)
	}

	_, err := s.BackfillBlockAccounting(ctx, block1.Hash)
	if !errors.Is(err, ErrIncompleteAccountingSource) {
		t.Fatalf("error = %v, want ErrIncompleteAccountingSource", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_accounting WHERE block_hash = $1`, block1.Hash).Scan(&count); err != nil {
		t.Fatalf("count block_accounting: %v", err)
	}
	if count != 0 {
		t.Fatalf("block_accounting row count = %d, want 0 (no row must be produced from incomplete fee data)", count)
	}

	// Restoring the valid fee makes the backfill succeed.
	if _, err := pool.Exec(ctx, `UPDATE transactions SET fee_satoshis = $2 WHERE txid = $1`, spend.TxID, fee); err != nil {
		t.Fatalf("restore fee: %v", err)
	}
	fresh, err := s.BackfillBlockAccounting(ctx, block1.Hash)
	if err != nil {
		t.Fatalf("BackfillBlockAccounting after restoring fee: %v", err)
	}
	if !fresh {
		t.Fatal("expected a freshly inserted row after restoring the valid fee")
	}
	acc, err := s.GetBlockAccounting(ctx, block1.Hash)
	if err != nil || acc == nil {
		t.Fatalf("GetBlockAccounting: acc=%+v err=%v", acc, err)
	}
	if acc.FeeSatoshis != fee {
		t.Fatalf("FeeSatoshis = %d, want %d", acc.FeeSatoshis, fee)
	}
}

// TestBackfillBlockAccounting_MissingCoinbaseOutputsRejected proves a
// coinbase transaction with zero transaction_outputs rows (indistinguishable
// from a "the miner claimed nothing" observation under a bare
// COALESCE(SUM(...), 0)) is detected as incomplete source data and
// rejected, rather than silently treated as a legitimate zero-value
// coinbase.
func TestBackfillBlockAccounting_MissingCoinbaseOutputsRejected(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	coinbaseTxid := hash64("missingoutG0tx")
	g := testBlock(hash64("missingoutG"), 100, "", coinbaseTx(coinbaseTxid, out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting WHERE block_hash = $1`, g.Hash); err != nil {
		t.Fatalf("delete accounting row: %v", err)
	}
	// Simulate incomplete indexed source data: the coinbase transaction
	// itself is indexed, but none of its outputs are. Clear the tables
	// that FK-reference transaction_outputs first (output_addresses,
	// output_participants, utxo_state) — this fixture never spends the
	// output, so removing utxo_state's row for it doesn't corrupt any
	// other invariant this test cares about.
	for _, table := range []string{"output_addresses", "output_participants", "utxo_state"} {
		if _, err := pool.Exec(ctx, `DELETE FROM `+table+` WHERE txid = $1`, coinbaseTxid); err != nil {
			t.Fatalf("delete %s: %v", table, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM transaction_outputs WHERE txid = $1`, coinbaseTxid); err != nil {
		t.Fatalf("delete coinbase outputs: %v", err)
	}

	_, err := s.BackfillBlockAccounting(ctx, g.Hash)
	if !errors.Is(err, ErrIncompleteAccountingSource) {
		t.Fatalf("error = %v, want ErrIncompleteAccountingSource", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_accounting WHERE block_hash = $1`, g.Hash).Scan(&count); err != nil {
		t.Fatalf("count block_accounting: %v", err)
	}
	if count != 0 {
		t.Fatalf("block_accounting row count = %d, want 0 (no row must be produced from a coinbase with no indexed outputs)", count)
	}
}
