package script

import (
	"bytes"
	"testing"
)

// buildWitnessScript constructs a raw witness-program scriptPubKey byte
// sequence for testing: <versionOpcode> <pushLenByte> <program...>.
// It does NOT validate that versionOpcode/pushLenByte are "correct" for the
// program length — that's exactly what lets it build intentionally invalid
// vectors (wrong length byte, trailing/truncated bytes) for negative tests.
func buildWitnessScript(versionOpcode byte, pushLenByte byte, program []byte) []byte {
	out := make([]byte, 0, 2+len(program))
	out = append(out, versionOpcode, pushLenByte)
	out = append(out, program...)
	return out
}

func program(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i + 1)
	}
	return p
}

func TestParseWitnessProgram_Valid(t *testing.T) {
	tests := []struct {
		name        string
		versionByte byte
		wantVersion int
		progLen     int
	}{
		{"v0/20 (P2WPKH shape)", opFalse, 0, 20},
		{"v0/32 (P2WSH shape)", opFalse, 0, 32},
		{"v1/32 (Taproot shape)", op1, 1, 32},
		{"v2/32 (P2QPK shape)", op1 + 1, 2, 32},
		{"v16/40 (max version, max length)", op16, 16, 40},
		{"v0/2 (min length)", opFalse, 0, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := program(tt.progLen)
			script := buildWitnessScript(tt.versionByte, byte(tt.progLen), p)
			wp, ok := ParseWitnessProgram(script)
			if !ok {
				t.Fatalf("expected valid witness program, got ok=false for script %x", script)
			}
			if wp.Version != tt.wantVersion {
				t.Errorf("version = %d, want %d", wp.Version, tt.wantVersion)
			}
			if !bytes.Equal(wp.Program, p) {
				t.Errorf("program = %x, want %x", wp.Program, p)
			}
		})
	}
}

func TestParseWitnessProgram_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		script []byte
	}{
		{"nil script", nil},
		{"empty script", []byte{}},
		{"too short (1 byte)", []byte{opFalse}},
		{"too short (3 bytes total, program len 1)", buildWitnessScript(opFalse, 1, program(1))},
		{"program length 1 (below min 2)", buildWitnessScript(opFalse, 1, program(1))},
		{"program length 41 (above max 40)", buildWitnessScript(op16, 41, program(41))},
		{"length byte says 32 but only 31 bytes follow (truncated)", buildWitnessScript(op1+1, 32, program(31))},
		{"length byte says 20 but 21 bytes follow (trailing byte)", buildWitnessScript(opFalse, 20, program(21))},
		{"version byte 0x50 (not OP_0 or OP_1-16)", buildWitnessScript(0x50, 32, program(32))},
		{"version byte 0x61 (above OP_16)", buildWitnessScript(0x61, 32, program(32))},
		{"not a push opcode at all (OP_DUP as version)", buildWitnessScript(opDup, 32, program(32))},
		{"P2PKH script (not a witness program)", []byte{opDup, opHash160, 20, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, opEqualVerify, opCheckSig}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := ParseWitnessProgram(tt.script)
			if ok {
				t.Fatalf("expected ok=false for %x, got ok=true", tt.script)
			}
		})
	}
}

// TestParseWitnessProgram_NeverPanics exercises a range of adversarial byte
// lengths and short/garbage inputs to confirm no slice-bounds panic occurs.
func TestParseWitnessProgram_NeverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ParseWitnessProgram panicked: %v", r)
		}
	}()
	for l := 0; l < 50; l++ {
		s := make([]byte, l)
		for i := range s {
			s[i] = byte(i * 7 % 256)
		}
		ParseWitnessProgram(s)
	}
	ParseWitnessProgram(nil)
	ParseWitnessProgram([]byte{0x00})
	ParseWitnessProgram([]byte{0x51, 0xff})
}
