package chain

import "fmt"

// Amount represents a QOGE value in integer satoshis. Every monetary value
// in this codebase — outputs, fees, subsidy, supply — is an Amount. Never
// use float64/float32 for a QOGE value anywhere; Core's own RPC returns
// float QOGE for display purposes only, and callers translating from RPC
// must convert to satoshis at the boundary (a future indexer concern, not
// this package's).
type Amount int64

// SatoshisPerQOGE is the fixed-point scale: 1 QOGE = 100,000,000 satoshis.
const SatoshisPerQOGE Amount = 100_000_000

// QOGE returns the amount expressed as whole QOGE, truncating any
// sub-satoshi remainder (there is none — satoshis are already the atomic
// unit). This is provided for display formatting only; do not use the
// result for further arithmetic.
func (a Amount) QOGE() int64 {
	return int64(a) / int64(SatoshisPerQOGE)
}

// Satoshis returns the amount as a plain int64 count of satoshis.
func (a Amount) Satoshis() int64 {
	return int64(a)
}

// String renders the amount as a fixed 8-decimal QOGE string, e.g.
// "6.25000000" or "-0.00000001". Display only.
func (a Amount) String() string {
	neg := a < 0
	v := int64(a)
	if neg {
		v = -v
	}
	whole := v / int64(SatoshisPerQOGE)
	frac := v % int64(SatoshisPerQOGE)
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%08d", sign, whole, frac)
}
