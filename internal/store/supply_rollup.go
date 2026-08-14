package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/accounting"
	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/jackc/pgx/v5"
)

// ErrRollupParentMissing means ApplyBlock was about to extend a block whose
// immediate parent block DOES have a rollup lineage active (see
// genesisRollupExists) but whose own block_supply_rollup row is
// unexpectedly missing — a genuine integrity failure (external corruption,
// manual deletion, a bug), never ordinary chain state. ApplyBlock fails the
// whole transaction rather than inventing a fabricated zero-based row (see
// applySupplyRollup's doc comment, task item 19). This is deliberately NOT
// raised for the "arbitrary synthetic bootstrap" case (task item 18), which
// silently produces no rollup rows at all rather than an error — see
// applySupplyRollup.
var ErrRollupParentMissing = errors.New("store: supply rollup: parent block has no rollup row")

// ErrRollupInvariant means a freshly-computed cumulative rollup value
// violates an invariant this package's own checked arithmetic is supposed
// to make structurally impossible under any real chain state (currently:
// the derived cumulative_utxo_set_value_satoshis came out negative). This
// should never trigger from genuine chain data — see
// docs/ARCHITECTURE.md §27.
var ErrRollupInvariant = errors.New("store: supply rollup: computed cumulative value violates an invariant")

// BlockSupplyRollup is a read-only view of one block_supply_rollup row —
// see migrations/0005_block_supply_rollup.up.sql for the exact column
// definitions this mirrors.
type BlockSupplyRollup struct {
	BlockHash                         string
	ExcludedOutputSatoshis            int64
	CumulativeSubsidySatoshis         int64
	CumulativeFeeSatoshis             int64
	CumulativeCoinbaseOutputSatoshis  int64
	CumulativeUnclaimedRewardSatoshis int64
	CumulativeExcludedOutputSatoshis  int64
	CumulativeUTXOSetValueSatoshis    int64
}

// blockSupplyRollupInsertSQL / blockSupplyRollupVerifySQL are the exact
// INSERT ... ON CONFLICT DO NOTHING / verify-match pair
// insertOrVerifyIdempotent expects (see store.go) — shared verbatim between
// applySupplyRollup (live ApplyBlock path) and the backfill-supply-rollup
// command, so a block's rollup row is persisted identically regardless of
// which path writes it first.
const (
	blockSupplyRollupInsertSQL = `
		INSERT INTO block_supply_rollup (
			block_hash, excluded_output_satoshis,
			cumulative_subsidy_satoshis, cumulative_fee_satoshis,
			cumulative_coinbase_output_satoshis, cumulative_unclaimed_reward_satoshis,
			cumulative_excluded_output_satoshis, cumulative_utxo_set_value_satoshis
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (block_hash) DO NOTHING`
	blockSupplyRollupVerifySQL = `
		SELECT excluded_output_satoshis = $2
		    AND cumulative_subsidy_satoshis = $3 AND cumulative_fee_satoshis = $4
		    AND cumulative_coinbase_output_satoshis = $5 AND cumulative_unclaimed_reward_satoshis = $6
		    AND cumulative_excluded_output_satoshis = $7 AND cumulative_utxo_set_value_satoshis = $8
		FROM block_supply_rollup WHERE block_hash = $1`
)

// localExcludedOutputValue sums block's OWN (non-cumulative) Core-excluded
// output value — mirroring applyOutput's utxo_state creation exclusion
// EXACTLY (apply.go's "Core UTXO semantics"): every output value at height
// 0 (genesis — Core's ConnectBlock never connects the genesis block's
// transactions at all), or, at any other height, any output whose
// scriptPubKey is script.IsUnspendable. Checked accumulation: returns
// ErrAmountOverflow instead of silently wrapping.
func localExcludedOutputValue(block chain.Block) (int64, error) {
	isGenesis := block.Height == 0
	var total int64
	for _, txn := range block.Transactions {
		for _, out := range txn.Outputs {
			if !isGenesis && !script.IsUnspendable(out.ScriptPubKey) {
				continue
			}
			sum, ok := addChecked(total, out.Value.Satoshis())
			if !ok {
				return 0, fmt.Errorf("%w: block %s excluded output total", ErrAmountOverflow, block.Hash)
			}
			total = sum
		}
	}
	return total, nil
}

// getBlockSupplyRollup reads one block_supply_rollup row inside tx, or
// found=false if none exists.
func getBlockSupplyRollup(ctx context.Context, tx pgx.Tx, blockHash string) (BlockSupplyRollup, bool, error) {
	var r BlockSupplyRollup
	err := tx.QueryRow(ctx, `
		SELECT block_hash, excluded_output_satoshis,
		       cumulative_subsidy_satoshis, cumulative_fee_satoshis,
		       cumulative_coinbase_output_satoshis, cumulative_unclaimed_reward_satoshis,
		       cumulative_excluded_output_satoshis, cumulative_utxo_set_value_satoshis
		FROM block_supply_rollup WHERE block_hash = $1
	`, blockHash).Scan(
		&r.BlockHash, &r.ExcludedOutputSatoshis,
		&r.CumulativeSubsidySatoshis, &r.CumulativeFeeSatoshis,
		&r.CumulativeCoinbaseOutputSatoshis, &r.CumulativeUnclaimedRewardSatoshis,
		&r.CumulativeExcludedOutputSatoshis, &r.CumulativeUTXOSetValueSatoshis,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return BlockSupplyRollup{}, false, nil
	}
	if err != nil {
		return BlockSupplyRollup{}, false, fmt.Errorf("store: supply rollup: read %s: %w", blockHash, err)
	}
	return r, true, nil
}

// genesisRollupExists reports whether the current canonical genesis block
// (height 0) has a block_supply_rollup row — the single source of truth
// applySupplyRollup uses to decide whether "this chain has an active,
// complete-from-genesis rollup lineage" (task item 19) versus "this chain
// was bootstrapped from an arbitrary synthetic non-zero height and will
// never have rollup rows" (task item 18). Genesis itself is immutable and
// unique once applied (ApplyBlock's continuity rule permits at most one
// height-0 block ever, see checkCanonicalContinuity), so this single check
// is sufficient — there is no scenario where a LATER block regains rollup
// coverage after an earlier gap, or loses it after being covered.
func genesisRollupExists(ctx context.Context, tx pgx.Tx) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM block_supply_rollup r
			JOIN blocks b ON b.hash = r.block_hash
			WHERE b.height = 0 AND b.canonical
		)
	`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: supply rollup: check genesis rollup coverage: %w", err)
	}
	return exists, nil
}

// applySupplyRollup computes and idempotently persists block's
// block_supply_rollup row, inside the same transaction ApplyBlock is
// already running, strictly AFTER applyBlockAccounting and strictly BEFORE
// the sync_state checkpoint update (task item 13) — see ApplyBlock. facts
// is the EXACT accounting.BlockFacts applyBlockAccounting already computed
// and persisted for this same block, in this same call, never a second,
// independently re-derived figure (task item 14 — no second fee/subsidy
// algorithm). checkpointBefore is the checkpoint ApplyBlock read (via
// lockCheckpoint) BEFORE this block was applied — Checkpoint{}'s zero value
// (empty Hash) means the store was uninitialized before this call, i.e.
// this block is the store's bootstrap point.
//
// # Cumulative computation
//
// For block.Height == 0 (real genesis — task item 12/17), the parent's
// cumulative values are all zero. For block.Height > 0, this block's
// parent (by block.PreviousHash — task item 16, NEVER height-1 alone, so a
// reorg's replacement branch is threaded through the correct ancestry) must
// already have a rollup row; its cumulative values are this block's
// starting point. Every cumulative field (subsidy, fee, coinbase output,
// unclaimed reward, excluded output) is parent + this block's own
// immutable contribution, via checked addition (task item 15/50 — overflow
// fails the whole ApplyBlock transaction, never wraps).
//
// cumulative_utxo_set_value_satoshis is deliberately NOT accumulated
// independently — it is DERIVED from the other three cumulative totals via
// the UTXO algebra proven in docs/ARCHITECTURE.md §27 (task item 11):
//
//	cumulative_utxo_set_value = cumulative_coinbase_output
//	                             - cumulative_fee
//	                             - cumulative_excluded_output
//
// Reasoning (task item 11, restated): every non-coinbase transaction's
// input/output value telescopes through history — a spent output's value
// cancels exactly against the later input that spent it, since Core
// requires input value >= output value + fee for every non-coinbase
// transaction, and this codebase's fee is computed as exactly that
// difference (applyTransaction). What survives, summed across the whole
// canonical prefix, is: every satoshi coinbase transactions ever created,
// minus every satoshi paid as a fee (fees are pre-existing value moving
// from a spender's balance to a miner's coinbase claim, not new UTXO-set
// value — the fee itself was already subtracted out of the spending
// transaction's own outputs, so it never re-enters the UTXO set through
// that transaction; it only re-enters via the miner's coinbase, which is
// already counted in cumulative_coinbase_output), minus every satoshi ever
// encoded in an output Core's coin view never admitted at creation
// (cumulative_excluded_output — genesis and OP_RETURN/oversized-script
// outputs, task item 9/10). A block's own underclaimed reward is already
// reflected in cumulative_coinbase_output being smaller than
// cumulative_subsidy+cumulative_fee would allow — no separate adjustment
// is needed for it here (task item 42).
//
// Computing it this way (rather than accumulating an independent UTXO
// counter) makes the UTXO identity hold BY CONSTRUCTION — there is no
// possible rollup row where it doesn't — with
// block_supply_rollup_utxo_identity (migration 0005) as a second,
// redundant, DB-native guard, exactly mirroring how block_accounting's own
// reward identity is enforced in two independent layers.
//
// # Arbitrary bootstrap and missing-parent policy (task items 18-19)
//
//   - block.Height == 0: always builds/verifies a rollup (parent is the
//     zero value) — genesis always gets a rollup.
//   - block.Height > 0 and checkpointBefore.Hash == "" (this IS the
//     store's bootstrap point, an arbitrary non-zero synthetic start
//     height some tests deliberately use — apply.go's "Canonical tip
//     continuity" doc comment): no parent rollup can possibly exist yet,
//     and none is fabricated. applySupplyRollup returns nil, having
//     written nothing. Every later block appended on top of this same
//     bootstrap chain will also find no parent rollup and, seeing genesis
//     itself has none either (genesisRollupExists), silently propagate the
//     same "no lineage" state forever — this store's supply rollup simply
//     stays empty, and any future public reader built on top of it must
//     treat that as "unavailable," not "zero."
//   - block.Height > 0, checkpointBefore.Hash != "" (an ordinary sequential
//     append or an exact-tip replay), and the parent DOES have a rollup:
//     the ordinary case — build/verify this block's row from it.
//   - block.Height > 0, checkpointBefore.Hash != "", and the parent has NO
//     rollup: distinguished by genesisRollupExists. If genesis itself has
//     no rollup, this chain never had a lineage to begin with (the
//     bootstrap-propagation case above, now one or more blocks later) —
//     silently write nothing. If genesis DOES have a rollup, this is a
//     genuine gap somewhere in an otherwise-complete lineage —
//     ErrRollupParentMissing, and the whole ApplyBlock transaction rolls
//     back (never a partial/fabricated row).
//
// # Idempotency (task item 20)
//
// Uses the same insertOrVerifyIdempotent path as every other immutable
// identity in this package: recomputing an already-applied block (exact
// tip replay, or safe orphan re-promotion — task item 21) produces
// identical values from the same immutable parent+facts, verified to
// match, never re-derived by a second independent code path and never
// UPDATEd. A previously-orphaned block whose rollup row was never deleted
// by RollbackTo (task item 22 — RollbackTo never touches this table) is
// simply re-verified on re-promotion. A pre-0005 historical orphan with no
// rollup at all, legitimately re-promoted later, is inserted fresh here —
// this function does not distinguish "fresh append" from "re-promotion,"
// only "parent has a rollup or not."
func (s *Store) applySupplyRollup(ctx context.Context, tx pgx.Tx, block chain.Block, checkpointBefore Checkpoint, facts accounting.BlockFacts) error {
	isGenesis := block.Height == 0

	var parent BlockSupplyRollup
	if !isGenesis {
		p, found, err := getBlockSupplyRollup(ctx, tx, block.PreviousHash)
		if err != nil {
			return err
		}
		if !found {
			if checkpointBefore.Hash == "" {
				return nil // arbitrary synthetic bootstrap (task item 18): no rollup, no error
			}
			active, err := genesisRollupExists(ctx, tx)
			if err != nil {
				return err
			}
			if !active {
				return nil // this chain never had a genesis-rooted rollup lineage: propagate silently
			}
			return fmt.Errorf("%w: block %s (height %d) parent %s", ErrRollupParentMissing, block.Hash, block.Height, block.PreviousHash)
		}
		parent = p
	}

	excluded, err := localExcludedOutputValue(block)
	if err != nil {
		return err
	}

	cumSubsidy, ok := addChecked(parent.CumulativeSubsidySatoshis, facts.SubsidySatoshis)
	if !ok {
		return fmt.Errorf("%w: block %s cumulative subsidy", ErrAmountOverflow, block.Hash)
	}
	cumFee, ok := addChecked(parent.CumulativeFeeSatoshis, facts.FeeSatoshis)
	if !ok {
		return fmt.Errorf("%w: block %s cumulative fee", ErrAmountOverflow, block.Hash)
	}
	cumCoinbase, ok := addChecked(parent.CumulativeCoinbaseOutputSatoshis, facts.CoinbaseOutputSatoshis)
	if !ok {
		return fmt.Errorf("%w: block %s cumulative coinbase output", ErrAmountOverflow, block.Hash)
	}
	cumUnclaimed, ok := addChecked(parent.CumulativeUnclaimedRewardSatoshis, facts.UnclaimedRewardSatoshis)
	if !ok {
		return fmt.Errorf("%w: block %s cumulative unclaimed reward", ErrAmountOverflow, block.Hash)
	}
	cumExcluded, ok := addChecked(parent.CumulativeExcludedOutputSatoshis, excluded)
	if !ok {
		return fmt.Errorf("%w: block %s cumulative excluded output", ErrAmountOverflow, block.Hash)
	}

	cumUTXO, ok := subChecked(cumCoinbase, cumFee)
	if !ok {
		return fmt.Errorf("%w: block %s cumulative utxo value (coinbase - fee)", ErrAmountOverflow, block.Hash)
	}
	cumUTXO, ok = subChecked(cumUTXO, cumExcluded)
	if !ok {
		return fmt.Errorf("%w: block %s cumulative utxo value (- excluded)", ErrAmountOverflow, block.Hash)
	}
	if cumUTXO < 0 {
		return fmt.Errorf("%w: block %s cumulative utxo value %d < 0", ErrRollupInvariant, block.Hash, cumUTXO)
	}

	_, err = insertOrVerifyIdempotent(ctx, tx, "block_supply_rollup "+block.Hash,
		blockSupplyRollupInsertSQL, blockSupplyRollupVerifySQL,
		block.Hash, excluded, cumSubsidy, cumFee, cumCoinbase, cumUnclaimed, cumExcluded, cumUTXO,
	)
	return err
}

// subChecked subtracts b from a, returning ok=false instead of silently
// wrapping on int64 overflow/underflow — same checked-arithmetic policy as
// addChecked (apply.go).
func subChecked(a, b int64) (int64, bool) {
	diff := a - b
	if (b < 0 && diff < a) || (b > 0 && diff > a) {
		return 0, false
	}
	return diff, true
}

// GetBlockSupplyRollup returns the block_supply_rollup row for blockHash,
// or nil if none exists — either because this block hasn't been applied,
// or (task item 18) because it belongs to a chain that was bootstrapped
// from an arbitrary non-genesis height and therefore has no rollup lineage
// at all.
func (s *Store) GetBlockSupplyRollup(ctx context.Context, blockHash string) (*BlockSupplyRollup, error) {
	var r BlockSupplyRollup
	err := s.pool.QueryRow(ctx, `
		SELECT block_hash, excluded_output_satoshis,
		       cumulative_subsidy_satoshis, cumulative_fee_satoshis,
		       cumulative_coinbase_output_satoshis, cumulative_unclaimed_reward_satoshis,
		       cumulative_excluded_output_satoshis, cumulative_utxo_set_value_satoshis
		FROM block_supply_rollup WHERE block_hash = $1
	`, blockHash).Scan(
		&r.BlockHash, &r.ExcludedOutputSatoshis,
		&r.CumulativeSubsidySatoshis, &r.CumulativeFeeSatoshis,
		&r.CumulativeCoinbaseOutputSatoshis, &r.CumulativeUnclaimedRewardSatoshis,
		&r.CumulativeExcludedOutputSatoshis, &r.CumulativeUTXOSetValueSatoshis,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get block supply rollup %s: %w", blockHash, err)
	}
	return &r, nil
}
