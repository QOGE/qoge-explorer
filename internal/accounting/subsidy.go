// Package accounting computes per-block monetary facts — block subsidy,
// issued supply — as pure, checked-arithmetic functions of height. This is
// DISPLAY/ACCOUNTING logic layered on top of an already-Core-validated
// chain: it never decides whether Core should have accepted a block, and it
// is never consulted by internal/store's consensus-adjacent shape checks.
// Core remains the sole authority on chain validity. See
// docs/ARCHITECTURE.md §6 for the full monetary-terms rationale.
package accounting

import "fmt"

// InitialSubsidySatoshis and SubsidyHalvingInterval are QOGE mainnet's
// actual consensus constants, verified directly against Qogecoin Core
// stable (not merely trusted from prior documentation — see
// docs/ARCHITECTURE.md §6):
//
//   - src/validation.cpp, GetBlockSubsidy: `CAmount nSubsidy = 100 * COIN;
//     nSubsidy >>= halvings;` with `if (halvings >= 64) return 0;`.
//   - src/consensus/amount.h: `COIN = 100000000` (100,000,000 satoshis/QOGE).
//   - src/chainparams.cpp, CMainParams: `nSubsidyHalvingInterval = 500000`.
const (
	InitialSubsidySatoshis int64 = 100 * 100_000_000 // 100 QOGE = 100 * COIN
	SubsidyHalvingInterval int64 = 500_000           // mainnet nSubsidyHalvingInterval

	// MaxHalvings mirrors Core's `if (halvings >= 64) return 0;` — a right
	// shift by 64 or more of a 64-bit value is undefined in Core's C++, so
	// Core forces the result to zero explicitly rather than ever
	// performing that shift. This function mirrors that exact boundary,
	// not a Go-idiomatic "shift until zero" derivation.
	MaxHalvings int64 = 64
)

// BlockSubsidy returns the consensus block subsidy for height, in integer
// satoshis, computed by the exact formula verified against Core's
// GetBlockSubsidy: 100 QOGE right-shifted once per 500,000-block halving
// era, forced to zero once 64 halvings have elapsed. height must be
// non-negative; a negative height is rejected rather than silently treated
// as height 0 (which would misreport a caller's bug as valid consensus
// data).
func BlockSubsidy(height int64) (int64, error) {
	if height < 0 {
		return 0, fmt.Errorf("%w: block subsidy height %d", ErrNegativeHeight, height)
	}
	halvings := height / SubsidyHalvingInterval
	if halvings >= MaxHalvings {
		return 0, nil
	}
	return InitialSubsidySatoshis >> uint(halvings), nil
}

// IssuedSupplyThroughHeight returns the total consensus issue — the sum of
// BlockSubsidy(h) for every h from 0 through height inclusive — in integer
// satoshis. It runs in O(number of halving eras) (at most MaxHalvings = 64
// iterations), never O(height): each era contributes
// (blocks in that era) * (that era's subsidy) as a single multiply, rather
// than iterating every individual height. This is what keeps a future
// GET /api/v1/supply cheap regardless of chain length.
//
// height must be non-negative. Genesis (height 0) is included in the sum:
// the project's supply definition is issued supply — what the consensus
// schedule entitles a height to — which is unaffected by Core's UTXO-set
// bookkeeping choice to never insert the genesis coinbase as a spendable
// coin (see docs/ARCHITECTURE.md §6/§16). Issued supply is therefore NOT
// the same metric as currently-spendable UTXO value.
func IssuedSupplyThroughHeight(height int64) (int64, error) {
	if height < 0 {
		return 0, fmt.Errorf("%w: issued supply height %d", ErrNegativeHeight, height)
	}

	var total int64
	for era := int64(0); era < MaxHalvings; era++ {
		eraStart := era * SubsidyHalvingInterval
		if eraStart > height {
			break
		}
		eraEndExclusive := eraStart + SubsidyHalvingInterval

		blocksInEra := SubsidyHalvingInterval
		if height+1 < eraEndExclusive {
			blocksInEra = height + 1 - eraStart
		}

		subsidy := InitialSubsidySatoshis >> uint(era)

		contribution, ok := checkedMul(subsidy, blocksInEra)
		if !ok {
			return 0, fmt.Errorf("%w: issued supply era %d contribution overflow", ErrAmountOverflow, era)
		}
		sum, ok := checkedAdd(total, contribution)
		if !ok {
			return 0, fmt.Errorf("%w: issued supply running total overflow at era %d", ErrAmountOverflow, era)
		}
		total = sum
	}
	return total, nil
}

// checkedAdd adds b to a, returning ok=false instead of silently wrapping on
// int64 overflow. Mirrors internal/store's addChecked (apply.go) — kept as
// a separate, unexported copy here rather than a shared dependency, since
// this package must not import internal/store (store depends on
// accounting, not the reverse).
func checkedAdd(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// checkedMul multiplies a by b, returning ok=false instead of silently
// wrapping on int64 overflow. Both operands are always non-negative in this
// package's actual call sites (a subsidy value and a block count), but the
// check itself is written generally rather than assuming that.
func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	product := a * b
	if product/b != a {
		return 0, false
	}
	return product, true
}
