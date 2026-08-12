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
// docs/ARCHITECTURE.md §6 for the full definitions this type mirrors:
//
//   - SubsidySatoshis: the height-derived new issuance entitlement (this
//     block's BlockSubsidy).
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
//     for the one direction that IS rejected.
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

// ComputeBlockFacts derives BlockFacts for one block from its height and
// two already-computed sums — feeSatoshis (this block's total non-coinbase
// transaction fees) and coinbaseOutputSatoshis (the sum of transaction
// index 0's outputs) — both of which the caller (internal/store) must
// derive from the SAME already-decoded/indexed data it is persisting, not
// from a second independent computation that could disagree with it (see
// docs/ARCHITECTURE.md §16 "Fee computation": transactions.fee_satoshis is
// the one fee algorithm this codebase has).
//
// Returns ErrCoinbaseOverclaim if coinbaseOutputSatoshis exceeds subsidy +
// fees — the only rejection this function performs; any lesser value
// (including zero) is accepted and reported as a positive
// UnclaimedRewardSatoshis, per Core's actual reward-limit rule (see
// ErrCoinbaseOverclaim's doc comment). Returns ErrAmountOverflow if
// subsidy + fees would overflow int64, and ErrNegativeHeight if height is
// negative (propagated from BlockSubsidy) — both are pure input-shape
// failures, never a signal to clamp.
func ComputeBlockFacts(blockHash string, height, feeSatoshis, coinbaseOutputSatoshis int64) (BlockFacts, error) {
	subsidy, err := BlockSubsidy(height)
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
	// maxReward was just proven above, and both are non-negative by
	// construction (callers derive them from summed satoshi values, never
	// from a signed difference) — the result can never exceed maxReward or
	// go negative.
	unclaimed := maxReward - coinbaseOutputSatoshis

	return BlockFacts{
		BlockHash:               blockHash,
		SubsidySatoshis:         subsidy,
		FeeSatoshis:             feeSatoshis,
		CoinbaseOutputSatoshis:  coinbaseOutputSatoshis,
		UnclaimedRewardSatoshis: unclaimed,
	}, nil
}
