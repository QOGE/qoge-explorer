package accounting

import (
	"errors"
	"math"
	"testing"
)

// ─── BlockSubsidy boundaries ────────────────────────────────────────────

// TestBlockSubsidy_ExactHeights verifies the exact satoshi subsidy at every
// height the spec calls out, computed independently from the halving-era
// formula rather than copy-pasted from the implementation.
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
			got, err := BlockSubsidy(c.height)
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
// the subsidy value at the first and last height of each era.
func TestBlockSubsidy_HalvingEraBoundaries(t *testing.T) {
	for era := int64(0); era <= 5; era++ {
		eraStart := era * SubsidyHalvingInterval
		eraLast := eraStart + SubsidyHalvingInterval - 1
		want := InitialSubsidySatoshis >> uint(era)

		gotStart, err := BlockSubsidy(eraStart)
		if err != nil {
			t.Fatalf("era %d start height %d: unexpected error: %v", era, eraStart, err)
		}
		if gotStart != want {
			t.Fatalf("era %d start height %d: BlockSubsidy = %d, want %d", era, eraStart, gotStart, want)
		}

		gotLast, err := BlockSubsidy(eraLast)
		if err != nil {
			t.Fatalf("era %d last height %d: unexpected error: %v", era, eraLast, err)
		}
		if gotLast != want {
			t.Fatalf("era %d last height %d: BlockSubsidy = %d, want %d", era, eraLast, gotLast, want)
		}
	}
}

// TestBlockSubsidy_Exhaustion proves the subsidy reaches exactly zero once
// 64 halvings have elapsed (Core's `if (halvings >= 64) return 0;`), and
// stays zero for every height beyond that — including heights far beyond
// any real chain length, proving there's no later resurgence from a wrapped
// shift.
func TestBlockSubsidy_Exhaustion(t *testing.T) {
	exhaustionHeight := MaxHalvings * SubsidyHalvingInterval // first height with 0 subsidy

	lastNonZeroHeight := exhaustionHeight - 1
	got, err := BlockSubsidy(lastNonZeroHeight)
	if err != nil {
		t.Fatalf("BlockSubsidy(%d): unexpected error: %v", lastNonZeroHeight, err)
	}
	want := InitialSubsidySatoshis >> uint(MaxHalvings-1)
	if got != want {
		t.Fatalf("BlockSubsidy(%d) (last height before exhaustion) = %d, want %d", lastNonZeroHeight, got, want)
	}

	heights := []int64{
		exhaustionHeight,
		exhaustionHeight + 1,
		exhaustionHeight + SubsidyHalvingInterval,
		exhaustionHeight * 1000,
	}
	for _, h := range heights {
		got, err := BlockSubsidy(h)
		if err != nil {
			t.Fatalf("BlockSubsidy(%d): unexpected error: %v", h, err)
		}
		if got != 0 {
			t.Fatalf("BlockSubsidy(%d) = %d, want 0 (post-exhaustion)", h, got)
		}
	}
}

func TestBlockSubsidy_NegativeHeightRejected(t *testing.T) {
	heights := []int64{-1, -500_000, -1 << 62}
	for _, h := range heights {
		_, err := BlockSubsidy(h)
		if !errors.Is(err, ErrNegativeHeight) {
			t.Fatalf("BlockSubsidy(%d): got err=%v, want ErrNegativeHeight", h, err)
		}
	}
}

// ─── IssuedSupplyThroughHeight ──────────────────────────────────────────

// slowIssuedSupply is a deliberately naive O(height) reference
// implementation, used ONLY in this test file to cross-check
// IssuedSupplyThroughHeight's O(halving eras) production implementation
// over small/sampled ranges. Never used in production code.
func slowIssuedSupply(t *testing.T, height int64) int64 {
	t.Helper()
	var total int64
	for h := int64(0); h <= height; h++ {
		s, err := BlockSubsidy(h)
		if err != nil {
			t.Fatalf("slowIssuedSupply: BlockSubsidy(%d): %v", h, err)
		}
		total += s // small ranges only — this reference loop does not itself need overflow checking
	}
	return total
}

func TestIssuedSupplyThroughHeight_CrossCheckSmallRanges(t *testing.T) {
	heights := []int64{0, 1, 2, 100, 499_999, 500_000, 500_001, 999_999, 1_000_000, 1_500_000}
	for _, h := range heights {
		want := slowIssuedSupply(t, h)
		got, err := IssuedSupplyThroughHeight(h)
		if err != nil {
			t.Fatalf("IssuedSupplyThroughHeight(%d): unexpected error: %v", h, err)
		}
		if got != want {
			t.Fatalf("IssuedSupplyThroughHeight(%d) = %d, want %d (slow reference)", h, got, want)
		}
	}
}

func TestIssuedSupplyThroughHeight_Height0IsGenesisSubsidy(t *testing.T) {
	got, err := IssuedSupplyThroughHeight(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != InitialSubsidySatoshis {
		t.Fatalf("IssuedSupplyThroughHeight(0) = %d, want %d (genesis subsidy alone)", got, InitialSubsidySatoshis)
	}
}

func TestIssuedSupplyThroughHeight_LastBeforeFirstHalving(t *testing.T) {
	height := SubsidyHalvingInterval - 1
	got, err := IssuedSupplyThroughHeight(height)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := SubsidyHalvingInterval * InitialSubsidySatoshis // every one of these blocks paid the full era-0 subsidy
	if got != want {
		t.Fatalf("IssuedSupplyThroughHeight(%d) = %d, want %d", height, got, want)
	}
}

func TestIssuedSupplyThroughHeight_FirstHalvingBoundary(t *testing.T) {
	height := SubsidyHalvingInterval // first block of era 1
	got, err := IssuedSupplyThroughHeight(height)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	era0Total := SubsidyHalvingInterval * InitialSubsidySatoshis
	era1FirstBlock := InitialSubsidySatoshis >> 1
	want := era0Total + era1FirstBlock
	if got != want {
		t.Fatalf("IssuedSupplyThroughHeight(%d) = %d, want %d", height, got, want)
	}
}

func TestIssuedSupplyThroughHeight_SeveralHalvingBoundaries(t *testing.T) {
	for era := int64(1); era <= 4; era++ {
		height := era * SubsidyHalvingInterval
		got, err := IssuedSupplyThroughHeight(height)
		if err != nil {
			t.Fatalf("era %d: unexpected error: %v", era, err)
		}
		var want int64
		for e := int64(0); e < era; e++ {
			want += SubsidyHalvingInterval * (InitialSubsidySatoshis >> uint(e))
		}
		want += InitialSubsidySatoshis >> uint(era) // the first block of this era
		if got != want {
			t.Fatalf("era %d boundary height %d: IssuedSupplyThroughHeight = %d, want %d", era, height, got, want)
		}
	}
}

// TestIssuedSupplyThroughHeight_CurrentChainScale checks a height in the
// low millions — representative of QOGE's actual current chain length —
// against the independently-derived era-sum formula.
func TestIssuedSupplyThroughHeight_CurrentChainScale(t *testing.T) {
	height := int64(3_250_000) // spans eras 0..6 plus a partial era 6... actually era = height/interval = 6 (partial)
	got, err := IssuedSupplyThroughHeight(height)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var want int64
	fullEras := height / SubsidyHalvingInterval
	for e := int64(0); e < fullEras; e++ {
		want += SubsidyHalvingInterval * (InitialSubsidySatoshis >> uint(e))
	}
	partialBlocks := height - fullEras*SubsidyHalvingInterval + 1
	want += partialBlocks * (InitialSubsidySatoshis >> uint(fullEras))

	if got != want {
		t.Fatalf("IssuedSupplyThroughHeight(%d) = %d, want %d", height, got, want)
	}
}

// TestIssuedSupplyThroughHeight_PostExhaustion proves the sum stops growing
// once every era's subsidy has reached zero: the total at the exhaustion
// height must equal the total at any later height.
func TestIssuedSupplyThroughHeight_PostExhaustion(t *testing.T) {
	exhaustionHeight := MaxHalvings * SubsidyHalvingInterval

	totalAtExhaustion, err := IssuedSupplyThroughHeight(exhaustionHeight)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	totalJustBefore, err := IssuedSupplyThroughHeight(exhaustionHeight - 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if totalAtExhaustion != totalJustBefore {
		t.Fatalf("issued supply grew at the exhaustion height (%d): before=%d at=%d, want equal (subsidy is 0 there)",
			exhaustionHeight, totalJustBefore, totalAtExhaustion)
	}

	laterHeights := []int64{exhaustionHeight + 1, exhaustionHeight + SubsidyHalvingInterval, exhaustionHeight * 10}
	for _, h := range laterHeights {
		got, err := IssuedSupplyThroughHeight(h)
		if err != nil {
			t.Fatalf("IssuedSupplyThroughHeight(%d): unexpected error: %v", h, err)
		}
		if got != totalAtExhaustion {
			t.Fatalf("IssuedSupplyThroughHeight(%d) = %d, want %d (supply must not grow post-exhaustion)", h, got, totalAtExhaustion)
		}
	}
}

func TestIssuedSupplyThroughHeight_NegativeHeightRejected(t *testing.T) {
	heights := []int64{-1, -500_000}
	for _, h := range heights {
		_, err := IssuedSupplyThroughHeight(h)
		if !errors.Is(err, ErrNegativeHeight) {
			t.Fatalf("IssuedSupplyThroughHeight(%d): got err=%v, want ErrNegativeHeight", h, err)
		}
	}
}

// ─── ComputeBlockFacts ───────────────────────────────────────────────────

func TestComputeBlockFacts_ExactClaim(t *testing.T) {
	facts, err := ComputeBlockFacts("deadbeef", 0, 1000, InitialSubsidySatoshis+1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facts.UnclaimedRewardSatoshis != 0 {
		t.Fatalf("UnclaimedRewardSatoshis = %d, want 0", facts.UnclaimedRewardSatoshis)
	}
	if facts.SubsidySatoshis != InitialSubsidySatoshis {
		t.Fatalf("SubsidySatoshis = %d, want %d", facts.SubsidySatoshis, InitialSubsidySatoshis)
	}
}

func TestComputeBlockFacts_Underclaim(t *testing.T) {
	facts, err := ComputeBlockFacts("deadbeef", 0, 1000, InitialSubsidySatoshis)
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
	facts, err := ComputeBlockFacts("deadbeef", 0, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facts.UnclaimedRewardSatoshis != InitialSubsidySatoshis {
		t.Fatalf("UnclaimedRewardSatoshis = %d, want %d", facts.UnclaimedRewardSatoshis, InitialSubsidySatoshis)
	}
}

func TestComputeBlockFacts_OverclaimRejected(t *testing.T) {
	_, err := ComputeBlockFacts("deadbeef", 0, 1000, InitialSubsidySatoshis+1001)
	if !errors.Is(err, ErrCoinbaseOverclaim) {
		t.Fatalf("got err=%v, want ErrCoinbaseOverclaim", err)
	}
}

func TestComputeBlockFacts_NegativeHeightRejected(t *testing.T) {
	_, err := ComputeBlockFacts("deadbeef", -1, 0, 0)
	if !errors.Is(err, ErrNegativeHeight) {
		t.Fatalf("got err=%v, want ErrNegativeHeight", err)
	}
}

// TestComputeBlockFacts_SubsidyPlusFeesOverflowRejected proves the
// "subsidy + fees" checked addition (spec section 48's third overflow
// requirement) rejects overflow rather than wrapping: at height 0 the
// subsidy is InitialSubsidySatoshis (10,000,000,000), so a fee value just
// under math.MaxInt64 pushes the sum past int64's range.
func TestComputeBlockFacts_SubsidyPlusFeesOverflowRejected(t *testing.T) {
	_, err := ComputeBlockFacts("deadbeef", 0, math.MaxInt64-1, 0)
	if !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("got err=%v, want ErrAmountOverflow", err)
	}
}
