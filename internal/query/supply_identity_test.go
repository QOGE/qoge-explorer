package query

import (
	"errors"
	"math"
	"testing"
)

// TestCheckSupplyRollupIdentities_Valid is the baseline: a rollup row
// satisfying both identities passes.
func TestCheckSupplyRollupIdentities_Valid(t *testing.T) {
	v := supplyRollupValues{
		subsidy:   100,
		fees:      5,
		coinbase:  95,
		unclaimed: 10, // reward identity: 95+10 == 100+5
		excluded:  0,
		utxo:      90, // utxo identity: 90+5+0 == 95
	}
	if err := checkSupplyRollupIdentities(v); err != nil {
		t.Fatalf("checkSupplyRollupIdentities(valid) = %v, want nil", err)
	}
}

// TestCheckSupplyRollupIdentities_RewardMismatch is spec item 26: the
// reward identity (coinbase + unclaimed == subsidy + fees) failing must be
// caught without any PostgreSQL involvement or CHECK-constraint bypass.
func TestCheckSupplyRollupIdentities_RewardMismatch(t *testing.T) {
	v := supplyRollupValues{
		subsidy:   100,
		fees:      5,
		coinbase:  95,
		unclaimed: 5, // 95+5=100 != 100+5=105
		excluded:  0,
		utxo:      90,
	}
	err := checkSupplyRollupIdentities(v)
	if !errors.Is(err, ErrSupplyIntegrity) {
		t.Fatalf("checkSupplyRollupIdentities(reward mismatch) = %v, want ErrSupplyIntegrity", err)
	}
}

// TestCheckSupplyRollupIdentities_UTXOMismatch is spec item 26: the UTXO
// identity (utxo + fees + excluded == coinbase) failing must be caught
// independently of the reward identity.
func TestCheckSupplyRollupIdentities_UTXOMismatch(t *testing.T) {
	v := supplyRollupValues{
		subsidy:   100,
		fees:      5,
		coinbase:  95,
		unclaimed: 10, // reward identity holds: 95+10 == 100+5
		excluded:  0,
		utxo:      1, // utxo identity fails: 1+5+0=6 != 95
	}
	err := checkSupplyRollupIdentities(v)
	if !errors.Is(err, ErrSupplyIntegrity) {
		t.Fatalf("checkSupplyRollupIdentities(utxo mismatch) = %v, want ErrSupplyIntegrity", err)
	}
}

// TestCheckSupplyRollupIdentities_RewardAdditionOverflow is spec item 26:
// an addition that would overflow int64 must fail loudly rather than
// silently wrap into a bogus (possibly negative-looking) comparison.
func TestCheckSupplyRollupIdentities_RewardAdditionOverflow(t *testing.T) {
	huge := int64(math.MaxInt64) - 3
	v := supplyRollupValues{
		subsidy:   huge,
		fees:      huge,
		coinbase:  huge,
		unclaimed: huge, // coinbase+unclaimed overflows int64
		excluded:  0,
		utxo:      0,
	}
	err := checkSupplyRollupIdentities(v)
	if !errors.Is(err, ErrSupplyIntegrity) {
		t.Fatalf("checkSupplyRollupIdentities(reward addition overflow) = %v, want ErrSupplyIntegrity", err)
	}
}

// TestCheckSupplyRollupIdentities_UTXOAdditionOverflow is spec item 26: the
// UTXO identity's first addition (utxo+fees) must be caught even when the
// reward identity independently passes — so the failure genuinely comes
// from the UTXO overflow check, not from the reward mismatch that would be
// hit first if the fixture didn't keep the reward identity valid.
func TestCheckSupplyRollupIdentities_UTXOAdditionOverflow(t *testing.T) {
	v := supplyRollupValues{
		subsidy:   10,
		fees:      10,
		coinbase:  20,
		unclaimed: 0, // reward identity holds: 20+0 == 10+10
		excluded:  0,
		utxo:      math.MaxInt64, // utxo+fees overflows int64
	}
	err := checkSupplyRollupIdentities(v)
	if !errors.Is(err, ErrSupplyIntegrity) {
		t.Fatalf("checkSupplyRollupIdentities(utxo addition overflow) = %v, want ErrSupplyIntegrity", err)
	}
}

// TestCheckSupplyRollupIdentities_UTXOSecondAdditionOverflow is spec item
// 26's second half: the UTXO identity's SECOND addition (+excluded, applied
// to the already-computed utxo+fees) must be checked independently — a
// fixture where utxo+fees fits in int64 but adding excluded on top does not.
func TestCheckSupplyRollupIdentities_UTXOSecondAdditionOverflow(t *testing.T) {
	v := supplyRollupValues{
		subsidy:   10,
		fees:      0,
		coinbase:  10,
		unclaimed: 0, // reward identity holds: 10+0 == 10+0
		excluded:  10,
		utxo:      math.MaxInt64 - 5, // utxo+fees fits; (utxo+fees)+excluded overflows
	}
	err := checkSupplyRollupIdentities(v)
	if !errors.Is(err, ErrSupplyIntegrity) {
		t.Fatalf("checkSupplyRollupIdentities(utxo second addition overflow) = %v, want ErrSupplyIntegrity", err)
	}
}

// TestCheckSupplyRollupIdentities_RewardRHSAdditionOverflow is spec item 3:
// the reward identity's RIGHT-hand addition (subsidy+fees) must be checked
// independently of its left-hand addition (coinbase+unclaimed), which here
// does not overflow.
func TestCheckSupplyRollupIdentities_RewardRHSAdditionOverflow(t *testing.T) {
	huge := int64(math.MaxInt64) - 3
	v := supplyRollupValues{
		subsidy:   huge,
		fees:      huge, // subsidy+fees overflows int64
		coinbase:  5,
		unclaimed: 5, // coinbase+unclaimed = 10, does not overflow
		excluded:  0,
		utxo:      0,
	}
	err := checkSupplyRollupIdentities(v)
	if !errors.Is(err, ErrSupplyIntegrity) {
		t.Fatalf("checkSupplyRollupIdentities(reward RHS addition overflow) = %v, want ErrSupplyIntegrity", err)
	}
}
