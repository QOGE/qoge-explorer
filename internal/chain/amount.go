package chain

import "fmt"

// Amount represents a QOGE value in integer satoshis. Every monetary value
// in this codebase — outputs, fees, subsidy, supply — is an Amount. Never
// use float64/float32 for a QOGE value anywhere; Core's own RPC returns
// float QOGE for display purposes only, and callers translating from RPC
// must convert to satoshis at the boundary (a future indexer concern, not
// this package's).
//
// Amount is a plain int64: the full int64 range is a valid Go value, but
// only magnitudes that fit real QOGE quantities (see
// TestAmount_TotalSupplyFitsInInt64) are ever meaningful — nothing in this
// package clamps or validates that at construction time.
type Amount int64

// SatoshisPerQOGE is the fixed-point scale: 1 QOGE = 100,000,000 satoshis.
const SatoshisPerQOGE Amount = 100_000_000

// WholeQOGE returns the amount truncated down to whole QOGE, discarding any
// fractional QOGE (e.g. 625,000,000 sats -> 6, not 6.25). The truncation is
// real and lossy — this is deliberately not named QOGE(), which previously
// invited exactly that mistake in financial-adjacent code. Use String() for
// display and Satoshis() for the actual, precise integer value; only reach
// for WholeQOGE() when a caller genuinely wants the truncated whole-unit
// count (e.g. a coarse UI grouping), never for further arithmetic.
func (a Amount) WholeQOGE() int64 {
	return int64(a) / int64(SatoshisPerQOGE)
}

// Satoshis returns the amount as a plain int64 count of satoshis — the
// exact, non-lossy representation.
func (a Amount) Satoshis() int64 {
	return int64(a)
}

// String renders the amount as a fixed 8-decimal QOGE string, e.g.
// "6.25000000" or "-0.00000001". Display only.
//
// Safe for the full int64 range including math.MinInt64: naive negation
// (v = -v) overflows for that one value (int64's magnitude range is
// asymmetric), silently producing a wrong sign/digits. The absolute value
// is instead computed in uint64 via the standard -(a+1)+1 idiom, which
// never overflows for any int64 input.
func (a Amount) String() string {
	sign := ""
	mag := uint64(a)
	if a < 0 {
		sign = "-"
		mag = uint64(-(a + 1)) + 1
	}
	whole := mag / uint64(SatoshisPerQOGE)
	frac := mag % uint64(SatoshisPerQOGE)
	return fmt.Sprintf("%s%d.%08d", sign, whole, frac)
}
