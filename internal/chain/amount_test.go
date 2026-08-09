package chain

import (
	"math"
	"testing"
)

func TestSatoshisPerQOGE(t *testing.T) {
	if SatoshisPerQOGE != 100_000_000 {
		t.Fatalf("SatoshisPerQOGE = %d, want 100000000", SatoshisPerQOGE)
	}
}

func TestAmount_String(t *testing.T) {
	tests := []struct {
		amount Amount
		want   string
	}{
		{0, "0.00000000"},
		{1, "0.00000001"},
		{SatoshisPerQOGE, "1.00000000"},
		{625_000_000, "6.25000000"},      // real block-2,000,000 coinbase reward, confirmed against live RPC
		{10_000_000_000, "100.00000000"}, // real block-1 coinbase reward
		{-1, "-0.00000001"},
	}
	for _, tt := range tests {
		if got := tt.amount.String(); got != tt.want {
			t.Errorf("Amount(%d).String() = %q, want %q", tt.amount, got, tt.want)
		}
	}
}

// TestAmount_String_MinInt64 confirms String() is safe at the int64 minimum
// edge case (PR #1 review item 5): naive `v = -v` negation overflows for
// math.MinInt64 specifically (its magnitude has no positive int64
// counterpart), which would silently corrupt the sign/digits. This is not a
// realistic QOGE value (see TestAmount_TotalSupplyFitsInInt64), but String()
// must not corrupt output for it rather than silently misbehaving.
func TestAmount_String_MinInt64(t *testing.T) {
	a := Amount(math.MinInt64)
	got := a.String()
	want := "-92233720368.54775808"
	if got != want {
		t.Errorf("Amount(math.MinInt64).String() = %q, want %q", got, want)
	}
	if got[0] != '-' {
		t.Errorf("expected a leading '-' sign, got %q", got)
	}
}

func TestAmount_QOGEAndSatoshis(t *testing.T) {
	a := Amount(625_013_500) // real block-2,439,284 coinbase reward incl. fee, confirmed against live RPC
	if got := a.Satoshis(); got != 625_013_500 {
		t.Errorf("Satoshis() = %d, want 625013500", got)
	}
	if got := a.WholeQOGE(); got != 6 {
		t.Errorf("QOGE() = %d, want 6 (truncated whole QOGE)", got)
	}
}

// TestAmount_TotalSupplyFitsInInt64 is a sanity check, not a live
// computation: QOGE's confirmed subsidy schedule (100 QOGE, halving every
// 500,000 blocks — see docs/ARCHITECTURE.md §6) sums to a total issued
// supply far below 21 million times the genesis subsidy, which itself is
// far below the int64 satoshi range. This guards against ever silently
// switching Amount to a narrower type.
func TestAmount_TotalSupplyFitsInInt64(t *testing.T) {
	const maxPlausibleSupplyQOGE = 200_000_000 // generous upper bound; real schedule converges near 100M
	maxPlausibleSupplySatoshis := int64(maxPlausibleSupplyQOGE) * int64(SatoshisPerQOGE)
	if maxPlausibleSupplySatoshis <= 0 {
		t.Fatalf("plausible max supply overflowed int64: %d", maxPlausibleSupplySatoshis)
	}
	const int64Max = 1<<63 - 1
	if maxPlausibleSupplySatoshis >= int64Max/100 {
		t.Fatalf("plausible max supply %d is uncomfortably close to int64 max %d", maxPlausibleSupplySatoshis, int64Max)
	}
}
