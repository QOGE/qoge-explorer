// Package accounting computes per-block monetary facts — block subsidy,
// scheduled subsidy totals — as pure, checked-arithmetic functions of
// height and network. This is DISPLAY/ACCOUNTING logic layered on top of an
// already-Core-validated chain: it never decides whether Core should have
// accepted a block, and it is never consulted by internal/store's
// consensus-adjacent shape checks. Core remains the sole authority on chain
// validity. See docs/ARCHITECTURE.md §6/§26 for the full monetary-terms
// rationale.
package accounting

import (
	"errors"
	"fmt"
)

// ErrUnknownNetwork means ScheduleForNetwork was asked for a network string
// that doesn't match any QOGE Core network this package knows the subsidy
// schedule for. Callers must never fall back to a default schedule on this
// error — see docs/ARCHITECTURE.md §26 "Network-aware subsidy schedule".
var ErrUnknownNetwork = errors.New("accounting: unknown network")

// SubsidySchedule holds the consensus subsidy parameters for one QOGE
// network. Every QOGE network shares the same 100 QOGE initial subsidy and
// the same MaxHalvings=64 Core shift-safety guard; only HalvingInterval
// differs between them. This was verified directly against Qogecoin Core
// stable's src/chainparams.cpp (not merely assumed from mainnet — a prior
// version of this package hardcoded mainnet's interval as if it were
// universal, which was wrong for regtest):
//
//   - CMainParams:    nSubsidyHalvingInterval = 500000  (strNetworkID "main")
//   - CTestNetParams: nSubsidyHalvingInterval = 500000  (strNetworkID "test")
//   - CSigNetParams:  nSubsidyHalvingInterval = 500000  (strNetworkID "signet")
//   - CRegTestParams: nSubsidyHalvingInterval = 150     (strNetworkID "regtest")
//
// The initial subsidy and halving formula themselves come from
// src/validation.cpp's GetBlockSubsidy, which takes consensusParams (and
// therefore the halving interval) as a parameter but otherwise applies
// identically across networks: `CAmount nSubsidy = 100 * COIN; nSubsidy >>=
// halvings;` with `if (halvings >= 64) return 0;` (Core's own comment on
// that guard: "Force block reward to zero when right shift is undefined").
// src/consensus/amount.h: COIN = 100000000 (100,000,000 satoshis/QOGE).
//
// IMPORTANT — two distinct concepts, do not conflate them:
//
//   - Core's `halvings >= 64` guard exists ONLY because right-shifting a
//     64-bit value by 64 or more bit positions is undefined behavior in
//     C++. It is a shift-safety guard, not a statement about when the
//     subsidy runs out.
//   - QOGE's EFFECTIVE monetary exhaustion happens far earlier, as a direct
//     consequence of finite satoshi precision: InitialSubsidySatoshis is
//     10,000,000,000 (100 QOGE), and 10,000,000,000 >> 33 = 1 satoshi while
//     10,000,000,000 >> 34 = 0. So the subsidy is already, naturally, zero
//     from era 34 onward — 30 full eras before the shift guard would ever
//     matter. BlockSubsidy needs no special-case for this: integer
//     right-shift already produces 0 at era 34 on its own; the `>= 64`
//     guard is simply never the reason era-34-and-later blocks pay zero.
type SubsidySchedule struct {
	InitialSubsidySatoshis int64
	HalvingInterval        int64

	// MaxHalvings mirrors Core's `if (halvings >= 64) return 0;` — a right
	// shift by 64 or more of a 64-bit value is undefined in Core's C++, so
	// Core forces the result to zero explicitly rather than ever
	// performing that shift. This is the same 64 for every network: it is
	// a property of the C++ shift-width guard, not of HalvingInterval.
	MaxHalvings int64
}

const (
	initialSubsidySatoshis int64 = 100 * 100_000_000 // 100 QOGE = 100 * COIN, identical on every network
	maxHalvings            int64 = 64
)

// MainnetSchedule, TestnetSchedule, SignetSchedule, and RegtestSchedule are
// QOGE Core's four network subsidy schedules, keyed by ScheduleForNetwork.
// MainnetSchedule is also exposed directly as a documented, explicitly
// mainnet-scoped convenience for callers that are known not to be
// network-sensitive (see internal/store.New's doc comment) — it must never
// be reached implicitly by a production code path that has an actual
// network to select on.
var (
	MainnetSchedule = SubsidySchedule{InitialSubsidySatoshis: initialSubsidySatoshis, HalvingInterval: 500_000, MaxHalvings: maxHalvings}
	TestnetSchedule = SubsidySchedule{InitialSubsidySatoshis: initialSubsidySatoshis, HalvingInterval: 500_000, MaxHalvings: maxHalvings}
	SignetSchedule  = SubsidySchedule{InitialSubsidySatoshis: initialSubsidySatoshis, HalvingInterval: 500_000, MaxHalvings: maxHalvings}
	RegtestSchedule = SubsidySchedule{InitialSubsidySatoshis: initialSubsidySatoshis, HalvingInterval: 150, MaxHalvings: maxHalvings}
)

// ScheduleForNetwork returns the SubsidySchedule for network, matching
// exactly the Core getblockchaininfo "chain" string this codebase already
// requires elsewhere (QOGE_NETWORK / indexer.ValidateStartup) — "main",
// "test", "signet", or "regtest". network is never inferred (e.g. from an
// address HRP — see docs/ARCHITECTURE.md §18); an unrecognized string is
// rejected as ErrUnknownNetwork rather than silently defaulting to
// MainnetSchedule.
func ScheduleForNetwork(network string) (SubsidySchedule, error) {
	switch network {
	case "main":
		return MainnetSchedule, nil
	case "test":
		return TestnetSchedule, nil
	case "signet":
		return SignetSchedule, nil
	case "regtest":
		return RegtestSchedule, nil
	default:
		return SubsidySchedule{}, fmt.Errorf("%w: %q", ErrUnknownNetwork, network)
	}
}

// BlockSubsidy returns the consensus block subsidy for height under sch, in
// integer satoshis, computed by the exact formula verified against Core's
// GetBlockSubsidy: the initial subsidy right-shifted once per
// sch.HalvingInterval-block halving era, forced to zero once
// sch.MaxHalvings halvings have elapsed (Core's shift-safety guard — see
// SubsidySchedule's doc comment). In practice the plain right-shift already
// reaches 0 at era 34 (10,000,000,000 >> 34 == 0) on every QOGE network,
// long before the sch.MaxHalvings=64 guard would ever trigger; that guard
// exists for shift-safety, not because era 34 alone wouldn't already be
// zero. height must be non-negative; a negative height is rejected rather
// than silently treated as height 0 (which would misreport a caller's bug
// as valid consensus data).
func (sch SubsidySchedule) BlockSubsidy(height int64) (int64, error) {
	if height < 0 {
		return 0, fmt.Errorf("%w: block subsidy height %d", ErrNegativeHeight, height)
	}
	halvings := height / sch.HalvingInterval
	if halvings >= sch.MaxHalvings {
		return 0, nil
	}
	return sch.InitialSubsidySatoshis >> uint(halvings), nil
}

// ScheduledSubsidyThroughHeight returns the sum of sch.BlockSubsidy(h) for
// every h from 0 through height inclusive, in integer satoshis. It runs in
// O(number of halving eras) (at most sch.MaxHalvings iterations), never
// O(height): each era contributes (blocks in that era) * (that era's
// subsidy) as a single multiply, rather than iterating every individual
// height.
//
// This is the SCHEDULED subsidy total, i.e. the maximum a fully-claiming
// chain could ever have issued through height under this consensus
// schedule — it is deliberately NOT called "issued supply." Because a
// miner's coinbase output total is only required to be <= subsidy + fees
// (see accounting.ErrCoinbaseOverclaim / block_accounting's
// unclaimed_reward_satoshis), a positive unclaimed reward on any block
// means less value was actually created by that block's coinbase than its
// scheduled entitlement — so SUM(BlockSubsidy) can systematically overstate
// actual chain issuance. See docs/ARCHITECTURE.md §26 "Scheduled subsidy is
// not issued supply." Phase 2H.2 will define the exact public metrics that
// account for underclaimed reward/fees; this function intentionally stops
// at the pure, schedule-only quantity.
//
// height must be non-negative. Genesis (height 0) is included in the sum:
// its scheduled entitlement is unaffected by Core's UTXO-set bookkeeping
// choice to never insert the genesis coinbase as a spendable coin (see
// docs/ARCHITECTURE.md §6/§16).
//
// height may be any non-negative int64, including math.MaxInt64: the
// per-era arithmetic below is written to avoid ever adding 1 to height
// itself (which would overflow at MaxInt64) — comparisons are phrased
// against the era's own bounds instead, which stay small (at most
// sch.MaxHalvings * sch.HalvingInterval) regardless of how large height is.
func (sch SubsidySchedule) ScheduledSubsidyThroughHeight(height int64) (int64, error) {
	if height < 0 {
		return 0, fmt.Errorf("%w: scheduled subsidy height %d", ErrNegativeHeight, height)
	}

	var total int64
	for era := int64(0); era < sch.MaxHalvings; era++ {
		eraStart := era * sch.HalvingInterval
		if eraStart > height {
			break
		}
		eraEndExclusive := eraStart + sch.HalvingInterval

		// Full era unless height falls strictly inside it. Phrased as
		// "height < eraEndExclusive-1" (equivalent to the more obvious
		// "height+1 < eraEndExclusive" but without ever computing height+1,
		// which overflows when height == math.MaxInt64). eraEndExclusive-1
		// is always small (bounded by sch.MaxHalvings * sch.HalvingInterval),
		// so this comparison and the blocksInEra subtraction below never
		// overflow even for a huge height.
		blocksInEra := sch.HalvingInterval
		if height < eraEndExclusive-1 {
			blocksInEra = height - eraStart + 1
		}

		subsidy := sch.InitialSubsidySatoshis >> uint(era)
		if subsidy == 0 {
			// Effective monetary exhaustion (era 34 for QOGE's 100 QOGE
			// initial subsidy — see SubsidySchedule's doc comment): every
			// later era's right-shifted subsidy is also 0, so no further
			// era can contribute. This is purely a loop-count optimization,
			// not a correctness dependency — the loop would reach the same
			// total by running all the way to sch.MaxHalvings, since a
			// zero-subsidy era's contribution is zero either way.
			break
		}

		contribution, ok := checkedMul(subsidy, blocksInEra)
		if !ok {
			return 0, fmt.Errorf("%w: scheduled subsidy era %d contribution overflow", ErrAmountOverflow, era)
		}
		sum, ok := checkedAdd(total, contribution)
		if !ok {
			return 0, fmt.Errorf("%w: scheduled subsidy running total overflow at era %d", ErrAmountOverflow, era)
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
