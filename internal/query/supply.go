package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrSupplyRollupUnavailable means the database is initialized
// (sync_state.indexed_height >= 0) but no block_supply_rollup row exists
// for the exact indexed tip — either because Phase 2H.2a's rollup
// migration/backfill hasn't run yet against this database (including the
// pre-0005-schema case, where the table itself doesn't exist:
// PostgreSQL's undefined_table, SQLSTATE 42P01), or because the tip's own
// row is missing for some other reason (manual deletion, an incompletely
// applied backfill). internal/api maps this to HTTP 503
// ("supply_rollup_unavailable"), internal/web to an explanatory HTML 503
// pointing at `qoge-explorer backfill-supply-rollup` — see
// docs/ARCHITECTURE.md §28.
var ErrSupplyRollupUnavailable = errors.New("query: block_supply_rollup is unavailable for the indexed tip")

// ErrSupplyIntegrity means one of two things, both defense in depth on top
// of block_supply_rollup's own CHECK constraints
// (block_supply_rollup_reward_identity / _utxo_identity in
// migrations/0005_block_supply_rollup.up.sql), and both indicating
// database corruption or a bug — never ordinary chain state:
//
//   - sync_state reports indexed_height >= 0 but indexed_block_hash is
//     NULL — a state the sync_state_validate_checkpoint_trigger
//     (migrations/0001_initial.up.sql) should make unreachable through
//     supported writes.
//   - the reward identity (coinbase_outputs + unclaimed_reward ==
//     scheduled_subsidy + transaction_fees) or the UTXO identity
//     (utxo_set_value + transaction_fees + excluded_output ==
//     coinbase_outputs) fails when re-verified in Go with checked int64
//     arithmetic, from the one rollup row already read — see
//     checkSupplyRollupIdentities.
var ErrSupplyIntegrity = errors.New("query: supply integrity check failed")

// SupplyOverview is Phase 2H.2b's public monetary read surface: five
// precise metrics, read in O(1) from the indexed tip's single immutable
// block_supply_rollup row (Phase 2H.2a) inside one read-only REPEATABLE
// READ snapshot (readTx) — same snapshot-consistency property
// ExplorerOverview/DeploymentOverview already give their own composite
// responses. None of these fields is, or is described as, "circulating
// supply" — see docs/ARCHITECTURE.md §28 for the full rationale:
//
//   - ScheduledSubsidy is the consensus subsidy entitlement accumulated
//     through the indexed canonical tip — a ceiling, not necessarily
//     actual issuance, because a miner may validly underclaim.
//   - TransactionFees is the total input-minus-output value paid as fees
//     by canonical non-coinbase transactions — existing value, never
//     newly issued coins.
//   - CoinbaseOutputs is the total value encoded in canonical coinbase
//     transaction outputs, including claimed transaction fees and the
//     genesis coinbase — deliberately "encoded in", not "actually paid":
//     genesis is counted here even though Core never places its coinbase
//     output in the spendable UTXO set.
//   - UnclaimedReward is the residual `subsidy + fees - coinbase_output`;
//     it cannot generally be partitioned into unclaimed subsidy vs.
//     unclaimed fees, and is never called "burn" or "unissued subsidy".
//   - UTXOSetValue is the value currently held by the explorer's
//     Core-equivalent canonical UTXO set at the indexed tip — it
//     deliberately excludes the genesis coinbase output and any output
//     Core itself never treats as spendable, so it is NOT expected to
//     equal CoinbaseOutputs.
//
// cumulative_excluded_output_satoshis, the sixth column
// block_supply_rollup stores, is deliberately never exposed here — it
// exists purely to support the UTXO identity check and internal/store's
// own UTXO cross-check (§27), not as a public metric.
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

// supplyRollupValues is the raw six-column shape of one block_supply_rollup
// row — private, since only SupplyOverview's public five-metric shape and
// checkSupplyRollupIdentities' arithmetic ever need it.
type supplyRollupValues struct {
	subsidy   int64
	fees      int64
	coinbase  int64
	unclaimed int64
	excluded  int64
	utxo      int64
}

// pgUndefinedTable is PostgreSQL's SQLSTATE for "relation does not exist"
// (undefined_table) — used below to distinguish "this database predates
// migration 0005" from any other, genuinely unexpected error.
const pgUndefinedTable = "42P01"

// supplyRollupValuesFrom is SupplyOverview's ONLY monetary data source: an
// exact primary-key lookup on block_supply_rollup by the indexed tip's own
// hash — O(1) relative to chain length, never a SUM/COUNT/JOIN over
// blocks, block_accounting, utxo_state, or transaction_outputs (see
// docs/ARCHITECTURE.md §28). A missing row (pgx.ErrNoRows) and a missing
// TABLE (pre-0005 schema, SQLSTATE 42P01) are both
// ErrSupplyRollupUnavailable — the public read surface does not
// distinguish "not backfilled yet" from "not migrated yet"; both point an
// operator at the same remediation.
func supplyRollupValuesFrom(ctx context.Context, q querier, blockHash string) (supplyRollupValues, error) {
	var v supplyRollupValues
	err := q.QueryRow(ctx, `
		SELECT
			cumulative_subsidy_satoshis,
			cumulative_fee_satoshis,
			cumulative_coinbase_output_satoshis,
			cumulative_unclaimed_reward_satoshis,
			cumulative_excluded_output_satoshis,
			cumulative_utxo_set_value_satoshis
		FROM block_supply_rollup
		WHERE block_hash = $1
	`, blockHash).Scan(&v.subsidy, &v.fees, &v.coinbase, &v.unclaimed, &v.excluded, &v.utxo)
	if err == nil {
		return v, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return supplyRollupValues{}, fmt.Errorf("%w: no block_supply_rollup row for indexed tip %s", ErrSupplyRollupUnavailable, blockHash)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedTable {
		return supplyRollupValues{}, fmt.Errorf("%w: block_supply_rollup table does not exist (pre-0005 schema)", ErrSupplyRollupUnavailable)
	}
	return supplyRollupValues{}, fmt.Errorf("query: supply rollup lookup: %w", err)
}

// checkedAddInt64 mirrors internal/accounting's own checked-addition
// policy (internal/accounting/subsidy.go's checkedAdd) and
// internal/store's addChecked — duplicated rather than imported because
// internal/query must not depend on either write-adjacent package for a
// two-line arithmetic helper. block_supply_rollup's own CHECK constraints
// already guard the stored row; this guards the SEPARATE additions
// checkSupplyRollupIdentities performs in Go once the row is in hand.
func checkedAddInt64(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// checkSupplyRollupIdentities is a small, pure, DB-independent helper
// (deliberately factored out so overflow/mismatch edge cases are testable
// directly, without corrupting a real block_supply_rollup row past its own
// CHECK constraints — see supply_identity_test.go) re-verifying, in Go
// with checked int64 arithmetic, the same two identities
// migrations/0005_block_supply_rollup.up.sql already enforces at the
// database level:
//
//   - reward identity: coinbase_outputs + unclaimed_reward ==
//     scheduled_subsidy + transaction_fees
//   - UTXO identity: utxo_set_value + transaction_fees + excluded_output
//     == coinbase_outputs
//
// This is cheap defense in depth, not a redundant restatement: a
// privileged raw-SQL writer could in principle construct a row that
// individually satisfies both CHECK constraints from a source-false
// combination of fields, and re-deriving the arithmetic here (rather than
// only trusting the constraints exist) catches at least the case where
// even that arithmetic doesn't hold. Either failure returns
// ErrSupplyIntegrity; SupplyOverview returns no data rather than a value
// it cannot vouch for.
func checkSupplyRollupIdentities(v supplyRollupValues) error {
	rewardLHS, ok := checkedAddInt64(v.coinbase, v.unclaimed)
	if !ok {
		return fmt.Errorf("%w: coinbase_outputs(%d) + unclaimed_reward(%d) overflows int64", ErrSupplyIntegrity, v.coinbase, v.unclaimed)
	}
	rewardRHS, ok := checkedAddInt64(v.subsidy, v.fees)
	if !ok {
		return fmt.Errorf("%w: scheduled_subsidy(%d) + transaction_fees(%d) overflows int64", ErrSupplyIntegrity, v.subsidy, v.fees)
	}
	if rewardLHS != rewardRHS {
		return fmt.Errorf("%w: reward identity violated: coinbase_outputs+unclaimed_reward=%d, scheduled_subsidy+transaction_fees=%d",
			ErrSupplyIntegrity, rewardLHS, rewardRHS)
	}

	utxoLHS, ok := checkedAddInt64(v.utxo, v.fees)
	if !ok {
		return fmt.Errorf("%w: utxo_set_value(%d) + transaction_fees(%d) overflows int64", ErrSupplyIntegrity, v.utxo, v.fees)
	}
	utxoLHS, ok = checkedAddInt64(utxoLHS, v.excluded)
	if !ok {
		return fmt.Errorf("%w: utxo_set_value+transaction_fees(%d) + excluded_output(%d) overflows int64", ErrSupplyIntegrity, utxoLHS, v.excluded)
	}
	if utxoLHS != v.coinbase {
		return fmt.Errorf("%w: utxo identity violated: utxo_set_value+transaction_fees+excluded_output=%d, coinbase_outputs=%d",
			ErrSupplyIntegrity, utxoLHS, v.coinbase)
	}
	return nil
}

// zeroSupplyOverview is the valid HTTP 200 response for an uninitialized
// explorer (sync_state.indexed_height == -1, no blocks at all): every
// total at zero, never confused with "chain supply of zero" — no rollup
// row is required or looked up in this case (docs/ARCHITECTURE.md §28,
// "Uninitialized database").
func zeroSupplyOverview(status Status) SupplyOverview {
	zero := chain.Amount(0).String()
	return SupplyOverview{
		IndexedHeight: status.IndexedHeight,
		IndexedHash:   status.IndexedHash,

		ScheduledSubsidySatoshis: 0,
		ScheduledSubsidyQOGE:     zero,
		TransactionFeesSatoshis:  0,
		TransactionFeesQOGE:      zero,
		CoinbaseOutputsSatoshis:  0,
		CoinbaseOutputsQOGE:      zero,
		UnclaimedRewardSatoshis:  0,
		UnclaimedRewardQOGE:      zero,
		UTXOSetValueSatoshis:     0,
		UTXOSetValueQOGE:         zero,
	}
}

// SupplyOverview reads Phase 2H.2b's five public monetary metrics from one
// snapshot: the sync_state anchor, then (for an initialized explorer) an
// exact block_supply_rollup primary-key lookup on that anchor's own
// hash — O(1) relative to chain length, never a request-time
// block_accounting/UTXO aggregate, canonical scan, or ancestry scan (see
// docs/ARCHITECTURE.md §28). A concurrent ApplyBlock/RollbackTo committing
// between these two reads can therefore never produce a response mixing
// height/hash from one canonical state with monetary totals from
// another — same REPEATABLE-READ guarantee ExplorerOverview/
// DeploymentOverview already give their own composite responses, and,
// independently, because block_supply_rollup rows are immutable and never
// deleted on rollback: even outside a transaction, the row a captured
// indexed hash names always still exists and still means the same thing.
//
// An uninitialized explorer (sync_state.indexed_height = -1, no blocks at
// all) is a valid HTTP 200 response with every total at zero
// (zeroSupplyOverview) — never confused with "chain supply of zero". An
// initialized explorer whose indexed tip has no block_supply_rollup row
// (not yet backfilled, or a pre-0005 schema) returns
// ErrSupplyRollupUnavailable. sync_state reporting indexed_height >= 0
// with a NULL indexed_block_hash — a state the database's own
// sync_state_validate_checkpoint_trigger should make unreachable — returns
// ErrSupplyIntegrity rather than being treated as ordinary uninitialized
// state.
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

	if status.IndexedHeight == -1 {
		return zeroSupplyOverview(status), nil
	}
	if status.IndexedHash == nil {
		return SupplyOverview{}, fmt.Errorf("%w: indexed_height=%d but indexed_block_hash is NULL", ErrSupplyIntegrity, status.IndexedHeight)
	}

	v, err := supplyRollupValuesFrom(ctx, tx, *status.IndexedHash)
	if err != nil {
		return SupplyOverview{}, err
	}
	if err := checkSupplyRollupIdentities(v); err != nil {
		return SupplyOverview{}, err
	}

	return SupplyOverview{
		IndexedHeight: status.IndexedHeight,
		IndexedHash:   status.IndexedHash,

		ScheduledSubsidySatoshis: v.subsidy,
		ScheduledSubsidyQOGE:     chain.Amount(v.subsidy).String(),

		TransactionFeesSatoshis: v.fees,
		TransactionFeesQOGE:     chain.Amount(v.fees).String(),

		CoinbaseOutputsSatoshis: v.coinbase,
		CoinbaseOutputsQOGE:     chain.Amount(v.coinbase).String(),

		UnclaimedRewardSatoshis: v.unclaimed,
		UnclaimedRewardQOGE:     chain.Amount(v.unclaimed).String(),

		UTXOSetValueSatoshis: v.utxo,
		UTXOSetValueQOGE:     chain.Amount(v.utxo).String(),
	}, nil
}
