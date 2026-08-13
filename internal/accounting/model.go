package accounting

import (
	"errors"
	"fmt"
)

// Sentinel errors this package returns, wrapped with contextual detail via
// fmt.Errorf("%w: ...", ...) — callers should use errors.Is against these
// rather than matching error strings.
var (
	// ErrNegativeHeight means a height argument was negative.
	ErrNegativeHeight = errors.New("accounting: height must be non-negative")

	// ErrNegativeAmount means a fee or coinbase-output-total argument
	// passed to ComputeBlockFacts was negative. Every real caller derives
	// these from summed satoshi values (never a signed difference), so
	// this should never trigger from production data — it exists so a
	// caller bug produces a loud, typed error here instead of silently
	// propagating a nonsensical negative monetary fact into
	// block_accounting (whose own CHECK constraints would then be the
	// only remaining guard).
	ErrNegativeAmount = errors.New("accounting: monetary amount must be non-negative")

	// ErrAmountOverflow means a checked satoshi accumulation would exceed
	// int64's range. Real QOGE quantities are far below this, but silent
	// wraparound is kept structurally impossible rather than merely
	// improbable — see internal/store's identical policy for per-
	// transaction fee/output accumulation.
	ErrAmountOverflow = errors.New("accounting: satoshi amount overflow")

	// ErrCoinbaseOverclaim means a block's actual coinbase output total
	// exceeds subsidy + fees — the one reward-limit invariant Core's
	// ConnectBlock actually enforces (src/validation.cpp:
	// `if (block.vtx[0]->GetValueOut() > blockReward) ... "bad-cb-amount"`).
	// Because Store only ever calls ComputeBlockFacts on a block Core has
	// already validated, seeing this error means the decoded model
	// disagrees with what Core would have accepted — a data-shape/decoding
	// bug, not a signal to clamp or silently tolerate.
	ErrCoinbaseOverclaim = errors.New("accounting: coinbase output total exceeds subsidy + fees")
)

// BlockFacts is one block's own immutable monetary facts — see
// docs/ARCHITECTURE.md §6/§26 for the full definitions this type mirrors:
//
//   - SubsidySatoshis: the height-and-network-derived new issuance
//     entitlement (this block's SubsidySchedule.BlockSubsidy) — the
//     SCHEDULED subsidy, not necessarily what the coinbase actually paid.
//   - FeeSatoshis: the exact sum of this block's non-coinbase transaction
//     fees.
//   - CoinbaseOutputSatoshis: the exact sum of every output of this
//     block's transaction index 0 — OP_RETURN, unspendable, unknown,
//     P2QPK, genesis, all included; this is "actual value paid by the
//     coinbase transaction's outputs," not "spendable miner reward."
//   - UnclaimedRewardSatoshis: SubsidySatoshis + FeeSatoshis -
//     CoinbaseOutputSatoshis. A positive value is valid, ordinary chain
//     state — a miner may claim less than the maximum available reward —
//     never evidence of corruption on its own. See ErrCoinbaseOverclaim
//     for the one direction that IS rejected. A positive value here also
//     means SubsidySatoshis overstates what this block's coinbase actually
//     created — see SubsidySchedule.ScheduledSubsidyThroughHeight's doc
//     comment for why a sum of SubsidySatoshis across blocks is not
//     "issued supply."
//
// BlockFacts deliberately carries no "canonical" flag — blocks.canonical
// (internal/store) is already the single source of truth for which chain a
// block belongs to; duplicating it here would let two canonical flags
// drift apart. A block's monetary facts don't change when a reorg demotes
// it off the canonical chain — only its canonical status does.
type BlockFacts struct {
	BlockHash               string
	SubsidySatoshis         int64
	FeeSatoshis             int64
	CoinbaseOutputSatoshis  int64
	UnclaimedRewardSatoshis int64
}

// ComputeBlockFacts derives BlockFacts for one block under sch (this
// block's network's subsidy schedule — see ScheduleForNetwork) from its
// height and two already-computed sums — feeSatoshis (this block's total
// non-coinbase transaction fees) and coinbaseOutputSatoshis (the sum of
// transaction index 0's outputs) — both of which the caller
// (internal/store) must derive from the SAME already-decoded/indexed data
// it is persisting, not from a second independent computation that could
// disagree with it (see docs/ARCHITECTURE.md §16 "Fee computation":
// transactions.fee_satoshis is the one fee algorithm this codebase has).
//
// Returns ErrNegativeAmount if feeSatoshis or coinbaseOutputSatoshis is
// negative — both must already be non-negative satoshi sums by
// construction, so this is a caller-bug guard, not an expected outcome.
// Returns ErrCoinbaseOverclaim if coinbaseOutputSatoshis exceeds subsidy +
// fees — the only rejection this function performs based on chain content;
// any lesser value (including zero) is accepted and reported as a positive
// UnclaimedRewardSatoshis, per Core's actual reward-limit rule (see
// ErrCoinbaseOverclaim's doc comment). Returns ErrAmountOverflow if
// subsidy + fees would overflow int64, and ErrNegativeHeight if height is
// negative (propagated from sch.BlockSubsidy) — all are pure input-shape
// failures, never a signal to clamp.
func (sch SubsidySchedule) ComputeBlockFacts(blockHash string, height, feeSatoshis, coinbaseOutputSatoshis int64) (BlockFacts, error) {
	if feeSatoshis < 0 {
		return BlockFacts{}, fmt.Errorf("%w: block %s fee %d", ErrNegativeAmount, blockHash, feeSatoshis)
	}
	if coinbaseOutputSatoshis < 0 {
		return BlockFacts{}, fmt.Errorf("%w: block %s coinbase output total %d", ErrNegativeAmount, blockHash, coinbaseOutputSatoshis)
	}

	subsidy, err := sch.BlockSubsidy(height)
	if err != nil {
		return BlockFacts{}, err
	}

	maxReward, ok := checkedAdd(subsidy, feeSatoshis)
	if !ok {
		return BlockFacts{}, fmt.Errorf("%w: block %s subsidy(%d) + fees(%d)", ErrAmountOverflow, blockHash, subsidy, feeSatoshis)
	}

	if coinbaseOutputSatoshis > maxReward {
		return BlockFacts{}, fmt.Errorf("%w: block %s coinbase output total %d > subsidy+fees %d",
			ErrCoinbaseOverclaim, blockHash, coinbaseOutputSatoshis, maxReward)
	}

	// Safe without a checked subtraction: coinbaseOutputSatoshis <=
	// maxReward was just proven above, and both are non-negative by the
	// guard above — the result can never exceed maxReward or go negative.
	unclaimed := maxReward - coinbaseOutputSatoshis

	return BlockFacts{
		BlockHash:               blockHash,
		SubsidySatoshis:         subsidy,
		FeeSatoshis:             feeSatoshis,
		CoinbaseOutputSatoshis:  coinbaseOutputSatoshis,
		UnclaimedRewardSatoshis: unclaimed,
	}, nil
}
