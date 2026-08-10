// Package decode is the strict boundary between Qogecoin Core's untyped
// JSON-RPC wire format (internal/rpc's raw DTOs) and the canonical,
// Core-shape-independent model (internal/chain) internal/store persists.
//
// This package is a representation boundary, not a second Qogecoin
// consensus validator: it never verifies proof-of-work, recomputes merkle
// roots, verifies signatures (ECDSA or SLH-DSA), or executes scripts. Core
// remains the sole authority on chain truth. The structural checks it does
// perform exist only to prevent a partial or malformed RPC response from
// silently entering the canonical chain model as if it were complete and
// well-formed data — the same spirit as internal/store's own
// representation-integrity checks (see docs/ARCHITECTURE.md §16).
package decode

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/QOGE/qoge-explorer/internal/chain"
)

// DecodeAmount converts a Core RPC decimal-QOGE JSON number (as emitted for
// every output/fee/balance value — e.g. "6.25000000") into exact integer
// satoshis. It never passes the value through float64/float32 at any
// point — raw is decoded as text (json.Number preserves Core's original
// decimal text unmodified; see internal/rpc.RawVout.Value) and converted
// via pure integer/string arithmetic, so no double-precision rounding
// error can ever enter a monetary value anywhere in this codebase.
//
// 1 QOGE = 100,000,000 satoshis (chain.SatoshisPerQOGE). Accepts an
// optional leading '-' (rejected below), an integer part, and an optional
// '.' followed by 1-8 fractional digits — exactly the shape
// Core/UniValue's ValueFromAmount ever emits, and nothing looser: a bare
// '+' sign, scientific notation, thousands separators, or any other
// character immediately fails as a malformed number.
//
// Rejected, always as an error rather than a substituted/rounded value:
//   - a malformed number (not this exact shape)
//   - a negative amount
//   - more than 8 fractional digits (cannot be represented exactly in
//     satoshis without rounding — this package never rounds)
//   - a magnitude that overflows int64 satoshis
func DecodeAmount(raw json.Number) (chain.Amount, error) {
	s := string(raw)
	if s == "" {
		return 0, fmt.Errorf("decode amount: empty value")
	}

	i := 0
	negative := false
	switch s[0] {
	case '-':
		negative = true
		i = 1
	case '+':
		return 0, fmt.Errorf("decode amount %q: malformed number (leading '+' not accepted)", s)
	}
	if i >= len(s) {
		return 0, fmt.Errorf("decode amount %q: malformed number", s)
	}

	var intPart, fracPart []byte
	seenDot := false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if seenDot {
				fracPart = append(fracPart, c)
			} else {
				intPart = append(intPart, c)
			}
		case c == '.' && !seenDot:
			seenDot = true
		default:
			return 0, fmt.Errorf("decode amount %q: malformed number", s)
		}
	}
	if len(intPart) == 0 && len(fracPart) == 0 {
		return 0, fmt.Errorf("decode amount %q: malformed number", s)
	}
	if negative {
		return 0, fmt.Errorf("decode amount %q: negative amount not accepted", s)
	}
	if len(fracPart) > 8 {
		return 0, fmt.Errorf("decode amount %q: %d fractional digits, cannot be represented exactly in satoshis (max 8)", s, len(fracPart))
	}
	if len(intPart) == 0 {
		intPart = []byte{'0'}
	}
	for len(fracPart) < 8 {
		fracPart = append(fracPart, '0')
	}

	combined := string(intPart) + string(fracPart)
	satoshis, err := strconv.ParseInt(combined, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode amount %q: overflow: %w", s, err)
	}
	return chain.Amount(satoshis), nil
}
