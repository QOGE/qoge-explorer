package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
)

// ErrAccountingIncomplete means the indexed canonical chain has one or more
// blocks with no corresponding block_accounting row — a database upgraded
// from before Phase 2H.1 whose `backfill-accounting` has not (yet) run to
// completion. Returning understated monetary totals in that situation would
// be worse than refusing to answer at all: internal/api maps this to HTTP
// 503 ("accounting_incomplete"), internal/web to an explanatory HTML 503
// page (docs/ARCHITECTURE.md's Phase 2H.2 section).
var ErrAccountingIncomplete = errors.New("query: canonical block accounting is incomplete for the indexed chain")

// ErrSupplyIntegrity means the aggregate cross-check
// (coinbase_outputs + unclaimed_reward == scheduled_subsidy + fees), or the
// checked-arithmetic addition it depends on, failed — despite every
// individual block_accounting row already satisfying
// block_accounting_reward_identity on its own. This is cheap defense in
// depth (docs/ARCHITECTURE.md §26 "internal/accounting: pure, checked-
// arithmetic functions"): it can only result from database corruption or an
// aggregation bug, never from ordinary valid chain state, and must never
// happen in production.
var ErrSupplyIntegrity = errors.New("query: supply aggregate identity check failed")

// SupplyOverview is Phase 2H.2's public monetary read surface: five precise
// metrics, read from ONE read-only REPEATABLE READ snapshot (readTx) —
// same snapshot-consistency property ExplorerOverview/DeploymentOverview
// already give their own composite responses. None of these fields is, or
// is described as, "circulating supply" — see docs/ARCHITECTURE.md's Phase
// 2H.2 section for the full rationale:
//
//   - ScheduledSubsidy is the consensus subsidy entitlement accumulated by
//     the indexed canonical chain — a ceiling, not necessarily actual
//     issuance, because a miner may validly underclaim.
//   - TransactionFees is existing value transferred by non-coinbase
//     transactions, never newly issued coins.
//   - CoinbaseOutputs is what canonical coinbase transactions actually
//     paid out (subsidy plus whatever fees were claimed), including
//     genesis.
//   - UnclaimedReward is the residual `subsidy + fees - coinbase_output`;
//     it cannot generally be partitioned into unclaimed subsidy vs.
//     unclaimed fees, and is never called "burn" or "unissued subsidy".
//   - UTXOSetValue is the value currently held by the explorer's
//     Core-equivalent canonical UTXO set — it deliberately excludes the
//     genesis coinbase output and any output Core itself never treats as
//     spendable (see internal/store's applyOutput doc comment), so it is
//     NOT expected to equal CoinbaseOutputs.
//
// Every metric is computed exclusively from the CANONICAL chain
// (blocks.canonical = true); an orphaned block's immutable accounting
// remains queryable as audit history but never contributes here.
type SupplyOverview struct {
	IndexedHeight int64   `json:"indexed_height"`
	IndexedHash   *string `json:"indexed_block_hash"`

	ScheduledSubsidySatoshis int64  `json:"scheduled_subsidy_sats"`
	ScheduledSubsidyQOGE     string `json:"scheduled_subsidy_qoge"`

	TransactionFeesSatoshis int64  `json:"transaction_fees_sats"`
	TransactionFeesQOGE     string `json:"transaction_fees_qoge"`

	CoinbaseOutputsSatoshis int64  `json:"coinbase_outputs_sats"`
	CoinbaseOutputsQOGE     string `json:"coinbase_outputs_qoge"`

	UnclaimedRewardSatoshis int64  `json:"unclaimed_reward_sats"`
	UnclaimedRewardQOGE     string `json:"unclaimed_reward_qoge"`

	UTXOSetValueSatoshis int64  `json:"utxo_set_value_sats"`
	UTXOSetValueQOGE     string `json:"utxo_set_value_qoge"`
}

// canonicalAccountingAggregate is the raw result of the canonical
// block_accounting aggregate query, before the completeness check and the
// QOGE-string formatting SupplyOverview applies to it.
type canonicalAccountingAggregate struct {
	canonicalBlocks int64
	accountedBlocks int64
	subsidy         int64
	fees            int64
	coinbase        int64
	unclaimed       int64
}

// canonicalAccountingAggregateFrom reads, in one statement, both the
// canonical block count and the canonical block_accounting sums — via a
// LEFT JOIN so a canonical block with no accounting row is still counted
// (as a non-NULL blocks row joined against NULL block_accounting columns),
// never silently absent from canonicalBlocks. This is what lets the caller
// structurally compare canonicalBlocks against accountedBlocks
// (count(ba.block_hash), which — ordinary SQL COUNT(column) semantics —
// only counts non-NULL join hits) rather than trusting a bare
// COALESCE(SUM(...), 0) over whatever accounting rows happen to already
// exist (docs/ARCHITECTURE.md §26 "Source-data completeness, not just
// presence" applies the identical principle to backfill).
func canonicalAccountingAggregateFrom(ctx context.Context, q querier) (canonicalAccountingAggregate, error) {
	var a canonicalAccountingAggregate
	err := q.QueryRow(ctx, `
		SELECT
			count(*),
			count(ba.block_hash),
			COALESCE(sum(ba.subsidy_satoshis), 0),
			COALESCE(sum(ba.fee_satoshis), 0),
			COALESCE(sum(ba.coinbase_output_satoshis), 0),
			COALESCE(sum(ba.unclaimed_reward_satoshis), 0)
		FROM blocks b
		LEFT JOIN block_accounting ba ON ba.block_hash = b.hash
		WHERE b.canonical
	`).Scan(&a.canonicalBlocks, &a.accountedBlocks, &a.subsidy, &a.fees, &a.coinbase, &a.unclaimed)
	if err != nil {
		return canonicalAccountingAggregate{}, fmt.Errorf("query: canonical accounting aggregate: %w", err)
	}
	return a, nil
}

// utxoSetValueFrom sums the value of every currently-unspent canonical
// output — utxo_state joined to transaction_outputs for the value, exactly
// as docs/ARCHITECTURE.md §6/§26 specify. This deliberately does not use
// addresses.balance_satoshis (internal/store's derived balance cache
// intentionally excludes bare-multisig outputs — see output_participants
// vs. output_addresses in migrations/0001_initial.up.sql) and does not
// require an output to have any recognized owning address at all: an
// eligible unknown/non-addressed output still has a utxo_state row and
// still contributes here.
func utxoSetValueFrom(ctx context.Context, q querier) (int64, error) {
	var v int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(o.value_satoshis), 0)
		FROM utxo_state u
		JOIN transaction_outputs o ON o.txid = u.txid AND o.vout_index = u.vout_index
		WHERE NOT u.spent
	`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("query: utxo set value: %w", err)
	}
	return v, nil
}

// checkedAddInt64 mirrors internal/accounting's own checked-addition
// policy (internal/accounting/subsidy.go's checkedAdd) — duplicated rather
// than imported because internal/query must not depend on
// internal/accounting's write-adjacent package for a two-line arithmetic
// helper; PostgreSQL's SUM(BIGINT) already returns NUMERIC (not int8), and
// pgx's numeric-to-int64 scan itself fails loudly (never silently wraps)
// if a corrupted/extreme aggregate does not fit int64 — see
// TestSupplyOverview_AggregateOverflowFailsLoudly. This function guards the
// SEPARATE addition SupplyOverview performs in Go once the two aggregate
// sums are already in hand.
func checkedAddInt64(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// SupplyOverview reads Phase 2H.2's five public monetary metrics from one
// snapshot, in the order docs/ARCHITECTURE.md's Phase 2H.2 section
// specifies: the sync_state anchor, then the canonical accounting
// aggregate (with its completeness check), then the current UTXO-set
// value. A concurrent ApplyBlock/RollbackTo committing between these reads
// can therefore never produce a response mixing height/hash from one
// canonical state with monetary totals from another — same
// REPEATABLE-READ guarantee ExplorerOverview/DeploymentOverview already
// give their own composite responses.
//
// An uninitialized explorer (sync_state.indexed_height = -1, no blocks at
// all) is a valid HTTP 200 response with every total at zero — the
// canonical-block count and accounted-block count are both 0 in that case,
// so the completeness check trivially passes; this is never confused with
// "chain supply of zero" (docs/ARCHITECTURE.md's Phase 2H.2 section,
// "Uninitialized database").
func (s *Store) SupplyOverview(ctx context.Context) (SupplyOverview, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return SupplyOverview{}, err
	}
	defer done()

	status, err := statusFrom(ctx, tx)
	if err != nil {
		return SupplyOverview{}, err
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first statement

	agg, err := canonicalAccountingAggregateFrom(ctx, tx)
	if err != nil {
		return SupplyOverview{}, err
	}
	if agg.accountedBlocks != agg.canonicalBlocks {
		return SupplyOverview{}, fmt.Errorf("%w: %d of %d canonical blocks have a block_accounting row",
			ErrAccountingIncomplete, agg.accountedBlocks, agg.canonicalBlocks)
	}

	utxoValue, err := utxoSetValueFrom(ctx, tx)
	if err != nil {
		return SupplyOverview{}, err
	}

	// Defense in depth (docs/ARCHITECTURE.md §26): every individual
	// block_accounting row already satisfies
	// block_accounting_reward_identity, but the aggregate is independently
	// re-verified here, in Go, with checked arithmetic — never trusting
	// that summing already-valid rows can't itself go wrong.
	lhs, ok := checkedAddInt64(agg.coinbase, agg.unclaimed)
	if !ok {
		return SupplyOverview{}, fmt.Errorf("%w: coinbase_outputs(%d) + unclaimed_reward(%d) overflows int64",
			ErrSupplyIntegrity, agg.coinbase, agg.unclaimed)
	}
	rhs, ok := checkedAddInt64(agg.subsidy, agg.fees)
	if !ok {
		return SupplyOverview{}, fmt.Errorf("%w: scheduled_subsidy(%d) + transaction_fees(%d) overflows int64",
			ErrSupplyIntegrity, agg.subsidy, agg.fees)
	}
	if lhs != rhs {
		return SupplyOverview{}, fmt.Errorf("%w: coinbase_outputs + unclaimed_reward = %d, scheduled_subsidy + transaction_fees = %d",
			ErrSupplyIntegrity, lhs, rhs)
	}

	return SupplyOverview{
		IndexedHeight: status.IndexedHeight,
		IndexedHash:   status.IndexedHash,

		ScheduledSubsidySatoshis: agg.subsidy,
		ScheduledSubsidyQOGE:     chain.Amount(agg.subsidy).String(),

		TransactionFeesSatoshis: agg.fees,
		TransactionFeesQOGE:     chain.Amount(agg.fees).String(),

		CoinbaseOutputsSatoshis: agg.coinbase,
		CoinbaseOutputsQOGE:     chain.Amount(agg.coinbase).String(),

		UnclaimedRewardSatoshis: agg.unclaimed,
		UnclaimedRewardQOGE:     chain.Amount(agg.unclaimed).String(),

		UTXOSetValueSatoshis: utxoValue,
		UTXOSetValueQOGE:     chain.Amount(utxoValue).String(),
	}, nil
}
