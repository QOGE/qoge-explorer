package decode

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
)

func TestDecodeAmount_Valid(t *testing.T) {
	tests := []struct {
		in   string
		want chain.Amount
	}{
		{"0", 0},
		{"0.00000001", 1},
		{"1", 100_000_000},
		{"1.00000000", 100_000_000},
		{"6.25", 625_000_000},
		{"100", 10_000_000_000},
		{"0.1", 10_000_000},
		{"0.00000000", 0},
		{".5", 50_000_000},                  // no leading digit before '.'
		{"21000000", 2_100_000_000_000_000}, // plausible large supply-scale value, no overflow
		{"0.99999999", 99_999_999},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := DecodeAmount(json.Number(tt.in))
			if err != nil {
				t.Fatalf("DecodeAmount(%q): unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("DecodeAmount(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecodeAmount_Rejected(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty string", ""},
		{"non-numeric garbage", "abc"},
		{"two decimal points", "1.2.3"},
		{"scientific notation", "1e5"},
		{"leading plus", "+1"},
		{"negative integer", "-1"},
		{"negative fraction", "-0.5"},
		{"negative zero", "-0"},
		{"bare minus", "-"},
		{"bare dot", "."},
		{"thousands separator", "1,000"},
		{"whitespace", " 1"},
		{"trailing whitespace", "1 "},
		{"nine fractional digits", "0.123456789"},
		{"nine fractional digits all but last zero", "0.123456780"},
		{"overflow: absurdly large integer", "999999999999999999999"},
		{"overflow: large with fraction", "99999999999999999999.99999999"},
		{"hex-looking", "0x1"},
		{"multiple minus signs", "--1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeAmount(json.Number(tt.in))
			if err == nil {
				t.Fatalf("DecodeAmount(%q): expected an error, got nil", tt.in)
			}
		})
	}
}

func TestDecodeAmount_NeverRounds(t *testing.T) {
	// A value with exactly 8 fractional digits, all significant, must
	// decode to the exact integer — proving no float64 rounding occurred
	// anywhere in the path (0.1 + 0.2 style errors would corrupt this).
	got, err := DecodeAmount(json.Number("0.30000001"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 30_000_001 {
		t.Errorf("got %d, want 30000001 (exact, no float rounding)", got)
	}
}

func TestDecodeAmount_MaxRepresentablePrecision(t *testing.T) {
	// Exactly 8 fractional digits is the boundary of what satoshis can
	// represent exactly — must be accepted, not merely "not overflow."
	got, err := DecodeAmount(json.Number("20999999.99999999"))
	if err != nil {
		t.Fatalf("unexpected error at the 8-decimal boundary: %v", err)
	}
	want := chain.Amount(20_999_999*100_000_000 + 99_999_999)
	if got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

func TestDecodeAmount_ErrorIsNotNilErrorType(t *testing.T) {
	_, err := DecodeAmount(json.Number("bad"))
	if err == nil || errors.Is(err, nil) {
		t.Fatalf("expected a non-nil error for a malformed amount")
	}
}
