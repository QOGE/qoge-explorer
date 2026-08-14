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

// TestCheckSupplyRollupIdentities_UTXOAdditionOverflow is spec item 26:
// the UTXO identity's own additions (utxo+fees, then +excluded) must be
// checked independently of the reward identity's additions.
func TestCheckSupplyRollupIdentities_UTXOAdditionOverflow(t *testing.T) {
	huge := int64(math.MaxInt64) - 3
	v := supplyRollupValues{
		subsidy:   0,
		fees:      huge,
		coinbase:  1,
		unclaimed: 1, // reward identity would fail anyway, but overflow must be caught first
		excluded:  0,
		utxo:      huge, // utxo+fees overflows int64
	}
	err := checkSupplyRollupIdentities(v)
	if !errors.Is(err, ErrSupplyIntegrity) {
		t.Fatalf("checkSupplyRollupIdentities(utxo addition overflow) = %v, want ErrSupplyIntegrity", err)
	}
}
