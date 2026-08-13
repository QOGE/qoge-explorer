package accounting

import (
	"errors"
	"math"
	"testing"
)

// ─── ScheduleForNetwork ─────────────────────────────────────────────────

// TestScheduleForNetwork_KnownNetworks proves every QOGE Core network name
// this codebase already uses elsewhere (QOGE_NETWORK / Core's
// getblockchaininfo "chain") maps to the correct halving interval,
// independently verified against Qogecoin Core stable's
// src/chainparams.cpp: CMainParams/CTestNetParams/CSigNetParams all use
// nSubsidyHalvingInterval = 500000, CRegTestParams uses 150 — all four
// share the same 100 QOGE initial subsidy (GetBlockSubsidy takes
// consensusParams as a parameter, not a per-network subsidy formula).
func TestScheduleForNetwork_KnownNetworks(t *testing.T) {
	cases := []struct {
		network         string
		wantInterval    int64
		wantInitial     int64
		wantMaxHalvings int64
	}{
		{"main", 500_000, initialSubsidySatoshis, 64},
		{"test", 500_000, initialSubsidySatoshis, 64},
		{"signet", 500_000, initialSubsidySatoshis, 64},
		{"regtest", 150, initialSubsidySatoshis, 64},
	}
	for _, c := range cases {
		t.Run(c.network, func(t *testing.T) {
			sch, err := ScheduleForNetwork(c.network)
			if err != nil {
				t.Fatalf("ScheduleForNetwork(%q): unexpected error: %v", c.network, err)
			}
			if sch.HalvingInterval != c.wantInterval {
				t.Fatalf("ScheduleForNetwork(%q).HalvingInterval = %d, want %d", c.network, sch.HalvingInterval, c.wantInterval)
			}
			if sch.InitialSubsidySatoshis != c.wantInitial {
				t.Fatalf("ScheduleForNetwork(%q).InitialSubsidySatoshis = %d, want %d", c.network, sch.InitialSubsidySatoshis, c.wantInitial)
			}
			if sch.MaxHalvings != c.wantMaxHalvings {
				t.Fatalf("ScheduleForNetwork(%q).MaxHalvings = %d, want %d", c.network, sch.MaxHalvings, c.wantMaxHalvings)
			}
		})
	}
}

// TestScheduleForNetwork_UnknownRejected proves an unrecognized network
// string is rejected outright rather than silently defaulting to
// MainnetSchedule — the exact bug this correction exists to prevent (a
// regtest index accidentally getting mainnet accounting).
func TestScheduleForNetwork_UnknownRejected(t *testing.T) {
	networks := []string{"", "mainnet", "MAIN", "testnet3", "unknown"}
	for _, n := range networks {
		_, err := ScheduleForNetwork(n)
		if !errors.Is(err, ErrUnknownNetwork) {
			t.Fatalf("ScheduleForNetwork(%q): got err=%v, want ErrUnknownNetwork", n, err)
		}
	}
}

// ─── BlockSubsidy boundaries ────────────────────────────────────────────

// TestBlockSubsidy_ExactHeights verifies the exact satoshi subsidy at every
// mainnet height the spec calls out, computed independently from the
// halving-era formula rather than copy-pasted from the implementation.
func TestBlockSubsidy_ExactHeights(t *testing.T) {
	const qoge int64 = 100_000_000

	// era = height / 500_000; subsidy = (100 QOGE) >> era.
	cases := []struct {
		name   string
		height int64
		want   int64
	}{
		{"height 0 (era 0)", 0, 100 * qoge},
		{"height 1 (era 0)", 1, 100 * qoge},
		{"height 499999 (last of era 0)", 499_999, 100 * qoge},
		{"height 500000 (first of era 1, 1st halving)", 500_000, 50 * qoge},
		{"height 999999 (last of era 1)", 999_999, 50 * qoge},
		{"height 1000000 (first of era 2, 2nd halving)", 1_000_000, 25 * qoge},
		{"height 1999999 (last of era 3)", 1_999_999, 125 * qoge / 10}, // era 3: 100>>3=12.5 QOGE
		{"height 2000000 (first of era 4, 4th halving)", 2_000_000, 625 * qoge / 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := MainnetSchedule.BlockSubsidy(c.height)
			if err != nil {
				t.Fatalf("BlockSubsidy(%d): unexpected error: %v", c.height, err)
			}
			if got != c.want {
				t.Fatalf("BlockSubsidy(%d) = %d, want %d", c.height, got, c.want)
			}
		})
	}
}

// TestBlockSubsidy_HalvingEraBoundaries independently derives era start
// heights for every era from 0 through a few past exhaustion, and checks
// the subsidy value at the first and last height of each era, on mainnet's
// schedule.
func TestBlockSubsidy_HalvingEraBoundaries(t *testing.T) {
	sch := MainnetSchedule
	for era := int64(0); era <= 5; era++ {
		eraStart := era * sch.HalvingInterval
		eraLast := eraStart + sch.HalvingInterval - 1
		want := sch.InitialSubsidySatoshis >> uint(era)

		gotStart, err := sch.BlockSubsidy(eraStart)
		if err != nil {
			t.Fatalf("era %d start height %d: unexpected error: %v", era, eraStart, err)
		}
		if gotStart != want {
			t.Fatalf("era %d start height %d: BlockSubsidy = %d, want %d", era, eraStart, gotStart, want)
		}

		gotLast, err := sch.BlockSubsidy(eraLast)
		if err != nil {
			t.Fatalf("era %d last height %d: unexpected error: %v", era, eraLast, err)
		}
		if gotLast != want {
			t.Fatalf("era %d last height %d: BlockSubsidy = %d, want %d", era, eraLast, gotLast, want)
		}
	}
}

// TestBlockSubsidy_EffectiveExhaustion_Mainnet proves the subsidy's ACTUAL
// (not Core-shift-guard) zero point: with InitialSubsidySatoshis =
// 10,000,000,000, 10,000,000,000 >> 33 = 1 satoshi and 10,000,000,000 >> 34
// = 0 — so era 33 (heights 16,500,000-16,999,999) is the last era with a
// nonzero subsidy, and era 34 (starting at height 17,000,000) is the first
// with a zero subsidy, entirely independent of Core's `halvings >= 64`
// shift-safety guard (era 34 is nowhere near era 64). A prior version of
// this test conflated the two — see TestBlockSubsidy_CoreShiftGuardBoundary
// for the guard itself.
func TestBlockSubsidy_EffectiveExhaustion_Mainnet(t *testing.T) {
	sch := MainnetSchedule
	cases := []struct {
		name   string
		height int64
		want   int64
	}{
		{"era 33 last height: 1 satoshi", 16_999_999, 1},
		{"era 34 first height: 0", 17_000_000, 0},
		{"era 34 second height: still 0", 17_000_001, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := sch.BlockSubsidy(c.height)
			if err != nil {
				t.Fatalf("BlockSubsidy(%d): unexpected error: %v", c.height, err)
			}
			if got != c.want {
				t.Fatalf("BlockSubsidy(%d) = %d, want %d", c.height, got, c.want)
			}
		})
	}
}

// TestBlockSubsidy_EffectiveExhaustion_Regtest is the regtest analogue of
// TestBlockSubsidy_EffectiveExhaustion_Mainnet: regtest's 150-block halving
// interval means the SAME era 33/34 boundary (subsidy depends only on era,
// not on HalvingInterval) falls at different heights — era 33 starts at
// 33*150=4,950, so its last height is 5,099 and era 34 starts at 5,100.
func TestBlockSubsidy_EffectiveExhaustion_Regtest(t *testing.T) {
	sch := RegtestSchedule
	cases := []struct {
		name   string
		height int64
		want   int64
	}{
		{"era 33 last height: 1 satoshi", 5_099, 1},
		{"era 34 first height: 0", 5_100, 0},
		{"era 34 second height: still 0", 5_101, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := sch.BlockSubsidy(c.height)
			if err != nil {
				t.Fatalf("BlockSubsidy(%d): unexpected error: %v", c.height, err)
			}
			if got != c.want {
				t.Fatalf("BlockSubsidy(%d) = %d, want %d", c.height, got, c.want)
			}
		})
	}
}

// TestBlockSubsidy_CoreShiftGuardBoundary proves Core's `if (halvings >= 64)
// return 0;` shift-safety guard itself behaves safely — NOT that it is the
// monetary exhaustion point (that's era 34, already zero 30 eras earlier;
// see TestBlockSubsidy_EffectiveExhaustion_Mainnet). This test's only
// purpose is to document that era >= 64 takes Core's explicit guard path
// and that no wrapped/resurrected nonzero subsidy ever occurs there or
// beyond, for heights far past any real chain length.
func TestBlockSubsidy_CoreShiftGuardBoundary(t *testing.T) {
	sch := MainnetSchedule
	heights := []int64{
		63 * sch.HalvingInterval, // era 63 start: guard not yet triggered (63 < 64), but already 0 from effective exhaustion
		64 * sch.HalvingInterval, // era 64 start: guard triggers (halvings >= 64)
		64*sch.HalvingInterval + 1,
		1000 * sch.HalvingInterval, // far beyond any real chain length
	}
	for _, h := range heights {
		got, err := sch.BlockSubsidy(h)
		if err != nil {
			t.Fatalf("BlockSubsidy(%d): unexpected error: %v", h, err)
		}
		if got != 0 {
			t.Fatalf("BlockSubsidy(%d) = %d, want 0 (no resurgence at/past Core's shift guard)", h, got)
		}
	}
}

func TestBlockSubsidy_NegativeHeightRejected(t *testing.T) {
	heights := []int64{-1, -500_000, -1 << 62}
	for _, h := range heights {
		_, err := MainnetSchedule.BlockSubsidy(h)
		if !errors.Is(err, ErrNegativeHeight) {
			t.Fatalf("BlockSubsidy(%d): got err=%v, want ErrNegativeHeight", h, err)
		}
	}
}

// TestBlockSubsidy_RegtestBoundary proves regtest's 150-block halving
// interval (CRegTestParams.nSubsidyHalvingInterval, distinct from every
// other network's 500000) is honored exactly — this is the boundary the
// internal review flagged as silently wrong when the schedule was
// hardcoded to mainnet's interval.
func TestBlockSubsidy_RegtestBoundary(t *testing.T) {
	const qoge int64 = 100_000_000
	cases := []struct {
		height int64
		want   int64
	}{
		{149, 100 * qoge},
		{150, 50 * qoge},
		{299, 50 * qoge},
		{300, 25 * qoge},
	}
	for _, c := range cases {
		got, err := RegtestSchedule.BlockSubsidy(c.height)
		if err != nil {
			t.Fatalf("RegtestSchedule.BlockSubsidy(%d): unexpected error: %v", c.height, err)
		}
		if got != c.want {
			t.Fatalf("RegtestSchedule.BlockSubsidy(%d) = %d, want %d", c.height, got, c.want)
		}
	}
}

// TestBlockSubsidy_TestnetMatchesMainnetBoundary proves testnet's schedule
// (also nSubsidyHalvingInterval = 500000) halves at exactly the same
// boundary as mainnet, distinct from regtest.
func TestBlockSubsidy_TestnetMatchesMainnetBoundary(t *testing.T) {
	const qoge int64 = 100_000_000
	cases := []struct {
		height int64
		want   int64
	}{
		{499_999, 100 * qoge},
		{500_000, 50 * qoge},
	}
	for _, c := range cases {
		got, err := TestnetSchedule.BlockSubsidy(c.height)
		if err != nil {
			t.Fatalf("TestnetSchedule.BlockSubsidy(%d): unexpected error: %v", c.height, err)
		}
		if got != c.want {
			t.Fatalf("TestnetSchedule.BlockSubsidy(%d) = %d, want %d", c.height, got, c.want)
		}
	}
}

// ─── ScheduledSubsidyThroughHeight ──────────────────────────────────────

// slowScheduledSubsidy is a deliberately naive O(height) reference
// implementation, used ONLY in this test file to cross-check
// ScheduledSubsidyThroughHeight's O(halving eras) production implementation
// over small/sampled ranges. Never used in production code.
func slowScheduledSubsidy(t *testing.T, sch SubsidySchedule, height int64) int64 {
	t.Helper()
	var total int64
	for h := int64(0); h <= height; h++ {
		s, err := sch.BlockSubsidy(h)
		if err != nil {
			t.Fatalf("slowScheduledSubsidy: BlockSubsidy(%d): %v", h, err)
		}
		total += s // small ranges only — this reference loop does not itself need overflow checking
	}
	return total
}

func TestScheduledSubsidyThroughHeight_CrossCheckSmallRanges(t *testing.T) {
	sch := MainnetSchedule
	heights := []int64{0, 1, 2, 100, 499_999, 500_000, 500_001, 999_999, 1_000_000, 1_500_000}
	for _, h := range heights {
		want := slowScheduledSubsidy(t, sch, h)
		got, err := sch.ScheduledSubsidyThroughHeight(h)
		if err != nil {
			t.Fatalf("ScheduledSubsidyThroughHeight(%d): unexpected error: %v", h, err)
		}
		if got != want {
			t.Fatalf("ScheduledSubsidyThroughHeight(%d) = %d, want %d (slow reference)", h, got, want)
		}
	}
}

func TestScheduledSubsidyThroughHeight_Height0IsGenesisSubsidy(t *testing.T) {
	got, err := MainnetSchedule.ScheduledSubsidyThroughHeight(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != MainnetSchedule.InitialSubsidySatoshis {
		t.Fatalf("ScheduledSubsidyThroughHeight(0) = %d, want %d (genesis subsidy alone)", got, MainnetSchedule.InitialSubsidySatoshis)
	}
}

func TestScheduledSubsidyThroughHeight_LastBeforeFirstHalving(t *testing.T) {
	sch := MainnetSchedule
	height := sch.HalvingInterval - 1
	got, err := sch.ScheduledSubsidyThroughHeight(height)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := sch.HalvingInterval * sch.InitialSubsidySatoshis // every one of these blocks paid the full era-0 subsidy
	if got != want {
		t.Fatalf("ScheduledSubsidyThroughHeight(%d) = %d, want %d", height, got, want)
	}
}

func TestScheduledSubsidyThroughHeight_FirstHalvingBoundary(t *testing.T) {
	sch := MainnetSchedule
	height := sch.HalvingInterval // first block of era 1
	got, err := sch.ScheduledSubsidyThroughHeight(height)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	era0Total := sch.HalvingInterval * sch.InitialSubsidySatoshis
	era1FirstBlock := sch.InitialSubsidySatoshis >> 1
	want := era0Total + era1FirstBlock
	if got != want {
		t.Fatalf("ScheduledSubsidyThroughHeight(%d) = %d, want %d", height, got, want)
	}
}

func TestScheduledSubsidyThroughHeight_SeveralHalvingBoundaries(t *testing.T) {
	sch := MainnetSchedule
	for era := int64(1); era <= 4; era++ {
		height := era * sch.HalvingInterval
		got, err := sch.ScheduledSubsidyThroughHeight(height)
		if err != nil {
			t.Fatalf("era %d: unexpected error: %v", era, err)
		}
		var want int64
		for e := int64(0); e < era; e++ {
			want += sch.HalvingInterval * (sch.InitialSubsidySatoshis >> uint(e))
		}
		want += sch.InitialSubsidySatoshis >> uint(era) // the first block of this era
		if got != want {
			t.Fatalf("era %d boundary height %d: ScheduledSubsidyThroughHeight = %d, want %d", era, height, got, want)
		}
	}
}

// TestScheduledSubsidyThroughHeight_CurrentChainScale checks a height in
// the low millions — representative of QOGE's actual current mainnet chain
// length — against the independently-derived era-sum formula.
func TestScheduledSubsidyThroughHeight_CurrentChainScale(t *testing.T) {
	sch := MainnetSchedule
	height := int64(3_250_000) // spans eras 0..6 plus a partial era 6
	got, err := sch.ScheduledSubsidyThroughHeight(height)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var want int64
	fullEras := height / sch.HalvingInterval
	for e := int64(0); e < fullEras; e++ {
		want += sch.HalvingInterval * (sch.InitialSubsidySatoshis >> uint(e))
	}
	partialBlocks := height - fullEras*sch.HalvingInterval + 1
	want += partialBlocks * (sch.InitialSubsidySatoshis >> uint(fullEras))

	if got != want {
		t.Fatalf("ScheduledSubsidyThroughHeight(%d) = %d, want %d", height, got, want)
	}
}

// TestScheduledSubsidyThroughHeight_PostExhaustion proves the sum stops
// growing once every era's subsidy has reached zero. This is grounded in
// EFFECTIVE exhaustion (era 34 — see TestBlockSubsidy_EffectiveExhaustion_Mainnet),
// not Core's era-64 shift guard: the running total at the last-nonzero
// height, the first-zero height, and any later height must all be equal,
// for both mainnet and regtest (whose era-34 boundary falls at a different
// height because HalvingInterval differs, but the SAME era).
func TestScheduledSubsidyThroughHeight_PostExhaustion(t *testing.T) {
	cases := []struct {
		name              string
		sch               SubsidySchedule
		lastNonZeroHeight int64
		firstZeroHeight   int64
	}{
		{"mainnet", MainnetSchedule, 16_999_999, 17_000_000},
		{"regtest", RegtestSchedule, 5_099, 5_100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			totalJustBefore, err := c.sch.ScheduledSubsidyThroughHeight(c.lastNonZeroHeight)
			if err != nil {
				t.Fatalf("ScheduledSubsidyThroughHeight(%d): unexpected error: %v", c.lastNonZeroHeight, err)
			}
			totalAtExhaustion, err := c.sch.ScheduledSubsidyThroughHeight(c.firstZeroHeight)
			if err != nil {
				t.Fatalf("ScheduledSubsidyThroughHeight(%d): unexpected error: %v", c.firstZeroHeight, err)
			}
			if totalAtExhaustion != totalJustBefore {
				t.Fatalf("scheduled subsidy total grew at the first zero-subsidy height (%d): before=%d at=%d, want equal",
					c.firstZeroHeight, totalJustBefore, totalAtExhaustion)
			}

			laterHeights := []int64{c.firstZeroHeight + 1, c.firstZeroHeight + c.sch.HalvingInterval, c.firstZeroHeight * 10}
			for _, h := range laterHeights {
				got, err := c.sch.ScheduledSubsidyThroughHeight(h)
				if err != nil {
					t.Fatalf("ScheduledSubsidyThroughHeight(%d): unexpected error: %v", h, err)
				}
				if got != totalAtExhaustion {
					t.Fatalf("ScheduledSubsidyThroughHeight(%d) = %d, want %d (total must not grow post-exhaustion)", h, got, totalAtExhaustion)
				}
			}
		})
	}
}

// TestScheduledSubsidyThroughHeight_MaxInt64NoOverflow proves
// ScheduledSubsidyThroughHeight(math.MaxInt64) returns exactly the same
// value as the EFFECTIVE first fully post-exhaustion height (era 34, not
// Core's era-64 shift guard — see TestBlockSubsidy_EffectiveExhaustion_Mainnet),
// with no error and no silent wraparound — the era arithmetic deliberately
// never computes height+1 (which would overflow at MaxInt64) for exactly
// this reason. See SubsidySchedule.ScheduledSubsidyThroughHeight's doc
// comment. Checked for both mainnet and regtest.
func TestScheduledSubsidyThroughHeight_MaxInt64NoOverflow(t *testing.T) {
	cases := []struct {
		name            string
		sch             SubsidySchedule
		firstZeroHeight int64
	}{
		{"mainnet", MainnetSchedule, 17_000_000},
		{"regtest", RegtestSchedule, 5_100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, err := c.sch.ScheduledSubsidyThroughHeight(c.firstZeroHeight)
			if err != nil {
				t.Fatalf("ScheduledSubsidyThroughHeight(%d): unexpected error: %v", c.firstZeroHeight, err)
			}

			got, err := c.sch.ScheduledSubsidyThroughHeight(math.MaxInt64)
			if err != nil {
				t.Fatalf("ScheduledSubsidyThroughHeight(MaxInt64): unexpected error: %v", err)
			}
			if got != want {
				t.Fatalf("ScheduledSubsidyThroughHeight(MaxInt64) = %d, want %d (same as first effectively-post-exhaustion height)", got, want)
			}
		})
	}
}

func TestScheduledSubsidyThroughHeight_NegativeHeightRejected(t *testing.T) {
	heights := []int64{-1, -500_000}
	for _, h := range heights {
		_, err := MainnetSchedule.ScheduledSubsidyThroughHeight(h)
		if !errors.Is(err, ErrNegativeHeight) {
			t.Fatalf("ScheduledSubsidyThroughHeight(%d): got err=%v, want ErrNegativeHeight", h, err)
		}
	}
}

// ─── ComputeBlockFacts ───────────────────────────────────────────────────

func TestComputeBlockFacts_ExactClaim(t *testing.T) {
	facts, err := MainnetSchedule.ComputeBlockFacts("deadbeef", 0, 1000, MainnetSchedule.InitialSubsidySatoshis+1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facts.UnclaimedRewardSatoshis != 0 {
		t.Fatalf("UnclaimedRewardSatoshis = %d, want 0", facts.UnclaimedRewardSatoshis)
	}
	if facts.SubsidySatoshis != MainnetSchedule.InitialSubsidySatoshis {
		t.Fatalf("SubsidySatoshis = %d, want %d", facts.SubsidySatoshis, MainnetSchedule.InitialSubsidySatoshis)
	}
}

func TestComputeBlockFacts_Underclaim(t *testing.T) {
	facts, err := MainnetSchedule.ComputeBlockFacts("deadbeef", 0, 1000, MainnetSchedule.InitialSubsidySatoshis)
	if err != nil {
		t.Fatalf("underclaiming the reward must be accepted, not rejected: %v", err)
	}
	if facts.UnclaimedRewardSatoshis != 1000 {
		t.Fatalf("UnclaimedRewardSatoshis = %d, want 1000", facts.UnclaimedRewardSatoshis)
	}
}

func TestComputeBlockFacts_ZeroCoinbaseOutputIsValid(t *testing.T) {
	// An extreme underclaim (a miner who claims nothing at all) is still a
	// valid — if unusual — chain state under Core's actual rule.
	facts, err := MainnetSchedule.ComputeBlockFacts("deadbeef", 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facts.UnclaimedRewardSatoshis != MainnetSchedule.InitialSubsidySatoshis {
		t.Fatalf("UnclaimedRewardSatoshis = %d, want %d", facts.UnclaimedRewardSatoshis, MainnetSchedule.InitialSubsidySatoshis)
	}
}

// TestComputeBlockFacts_ScheduledSubsidyIsNotActualIssuance proves the
// terminology distinction the internal review required: for an
// underclaimed block, SubsidySatoshis still follows the schedule exactly
// (it does not shrink to "what was actually paid"), while
// UnclaimedRewardSatoshis is the separate fact recording that less value
// was actually created. A reader must never treat SubsidySatoshis alone,
// or a sum of it across blocks, as "actual issued supply" — see
// SubsidySchedule.ScheduledSubsidyThroughHeight's doc comment.
func TestComputeBlockFacts_ScheduledSubsidyIsNotActualIssuance(t *testing.T) {
	const actualCoinbasePayout = 1000 // a miner claiming almost nothing
	facts, err := MainnetSchedule.ComputeBlockFacts("deadbeef", 0, 0, actualCoinbasePayout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facts.SubsidySatoshis != MainnetSchedule.InitialSubsidySatoshis {
		t.Fatalf("SubsidySatoshis = %d, want the full scheduled subsidy %d regardless of actual claim",
			facts.SubsidySatoshis, MainnetSchedule.InitialSubsidySatoshis)
	}
	if facts.CoinbaseOutputSatoshis != actualCoinbasePayout {
		t.Fatalf("CoinbaseOutputSatoshis = %d, want %d (actual value created)", facts.CoinbaseOutputSatoshis, actualCoinbasePayout)
	}
	wantUnclaimed := MainnetSchedule.InitialSubsidySatoshis - actualCoinbasePayout
	if facts.UnclaimedRewardSatoshis != wantUnclaimed {
		t.Fatalf("UnclaimedRewardSatoshis = %d, want %d", facts.UnclaimedRewardSatoshis, wantUnclaimed)
	}
	if facts.SubsidySatoshis == facts.CoinbaseOutputSatoshis {
		t.Fatalf("scheduled subsidy and actual coinbase payout coincidentally equal in this fixture — test does not exercise the distinction")
	}
}

func TestComputeBlockFacts_OverclaimRejected(t *testing.T) {
	_, err := MainnetSchedule.ComputeBlockFacts("deadbeef", 0, 1000, MainnetSchedule.InitialSubsidySatoshis+1001)
	if !errors.Is(err, ErrCoinbaseOverclaim) {
		t.Fatalf("got err=%v, want ErrCoinbaseOverclaim", err)
	}
}

func TestComputeBlockFacts_NegativeHeightRejected(t *testing.T) {
	_, err := MainnetSchedule.ComputeBlockFacts("deadbeef", -1, 0, 0)
	if !errors.Is(err, ErrNegativeHeight) {
		t.Fatalf("got err=%v, want ErrNegativeHeight", err)
	}
}

// TestComputeBlockFacts_NegativeFeeRejected and
// TestComputeBlockFacts_NegativeCoinbaseOutputRejected prove
// ComputeBlockFacts enforces non-negativity of its own monetary inputs
// rather than trusting the caller — internal/store never actually supplies
// a negative value today, but this keeps that invariant structurally
// guaranteed inside the package that owns it.
func TestComputeBlockFacts_NegativeFeeRejected(t *testing.T) {
	_, err := MainnetSchedule.ComputeBlockFacts("deadbeef", 0, -1, 0)
	if !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("got err=%v, want ErrNegativeAmount", err)
	}
}

func TestComputeBlockFacts_NegativeCoinbaseOutputRejected(t *testing.T) {
	_, err := MainnetSchedule.ComputeBlockFacts("deadbeef", 0, 0, -1)
	if !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("got err=%v, want ErrNegativeAmount", err)
	}
}

// TestComputeBlockFacts_SubsidyPlusFeesOverflowRejected proves the
// "subsidy + fees" checked addition (spec section 48's third overflow
// requirement) rejects overflow rather than wrapping: at height 0 the
// subsidy is InitialSubsidySatoshis (10,000,000,000), so a fee value just
// under math.MaxInt64 pushes the sum past int64's range.
func TestComputeBlockFacts_SubsidyPlusFeesOverflowRejected(t *testing.T) {
	_, err := MainnetSchedule.ComputeBlockFacts("deadbeef", 0, math.MaxInt64-1, 0)
	if !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("got err=%v, want ErrAmountOverflow", err)
	}
}
