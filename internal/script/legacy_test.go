package script

import (
	"bytes"
	"testing"
)

func fill(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

func TestMatchP2PKH(t *testing.T) {
	hash := fill(20, 1)
	valid := append([]byte{opDup, opHash160, 20}, append(append([]byte{}, hash...), opEqualVerify, opCheckSig)...)

	got, ok := matchP2PKH(valid)
	if !ok || !bytes.Equal(got, hash) {
		t.Fatalf("valid P2PKH: ok=%v got=%x want=%x", ok, got, hash)
	}

	invalid := [][]byte{
		nil,
		{},
		valid[:len(valid)-1],                     // truncated
		append(append([]byte{}, valid...), 0x00), // trailing byte
		{opHash160, opDup, 20, 1, 2, 3, opEqualVerify, opCheckSig}, // wrong opcode order
	}
	for i, s := range invalid {
		if _, ok := matchP2PKH(s); ok {
			t.Errorf("invalid[%d] unexpectedly matched as P2PKH: %x", i, s)
		}
	}
}

func TestMatchP2SH(t *testing.T) {
	hash := fill(20, 2)
	valid := append([]byte{opHash160, 20}, append(append([]byte{}, hash...), opEqual)...)

	got, ok := matchP2SH(valid)
	if !ok || !bytes.Equal(got, hash) {
		t.Fatalf("valid P2SH: ok=%v got=%x want=%x", ok, got, hash)
	}

	invalid := [][]byte{
		nil,
		valid[:len(valid)-1],
		append(append([]byte{}, valid...), 0x00),
		{opHash160, 20, 1, 2, 3, opEqualVerify}, // wrong terminal opcode
	}
	for i, s := range invalid {
		if _, ok := matchP2SH(s); ok {
			t.Errorf("invalid[%d] unexpectedly matched as P2SH: %x", i, s)
		}
	}
}

func TestMatchP2PK(t *testing.T) {
	compressed := fill(33, 3)
	valid := append([]byte{33}, append(append([]byte{}, compressed...), opCheckSig)...)
	got, ok := matchP2PK(valid)
	if !ok || !bytes.Equal(got, compressed) {
		t.Fatalf("valid compressed P2PK: ok=%v got=%x", ok, got)
	}

	uncompressed := fill(65, 4)
	validU := append([]byte{65}, append(append([]byte{}, uncompressed...), opCheckSig)...)
	gotU, okU := matchP2PK(validU)
	if !okU || !bytes.Equal(gotU, uncompressed) {
		t.Fatalf("valid uncompressed P2PK: ok=%v got=%x", okU, gotU)
	}

	invalid := [][]byte{
		nil,
		append([]byte{32}, append(fill(32, 5), opCheckSig)...), // wrong length (32, not 33/65)
		valid[:len(valid)-1], // truncated
	}
	for i, s := range invalid {
		if _, ok := matchP2PK(s); ok {
			t.Errorf("invalid[%d] unexpectedly matched as P2PK: %x", i, s)
		}
	}
}

func TestMatchNullData(t *testing.T) {
	valid := []struct {
		name   string
		script []byte
	}{
		{"bare OP_RETURN, no data", []byte{opReturn}},
		{"OP_RETURN + small push", append([]byte{opReturn, 4}, fill(4, 1)...)},
		{"OP_RETURN + OP_PUSHDATA1", append([]byte{opReturn, opPushData1, 4}, fill(4, 1)...)},
	}
	for _, tt := range valid {
		if !matchNullData(tt.script) {
			t.Errorf("%s: expected NULLDATA match, got false for %x", tt.name, tt.script)
		}
	}

	invalid := []struct {
		name   string
		script []byte
	}{
		{"empty", nil},
		{"no OP_RETURN prefix", []byte{opDup, 1, 2}},
		{"OP_RETURN + truncated push", []byte{opReturn, 10, 1, 2}},
		{"OP_RETURN + non-push opcode", []byte{opReturn, opCheckSig}},
	}
	for _, tt := range invalid {
		if matchNullData(tt.script) {
			t.Errorf("%s: unexpectedly matched as NULLDATA: %x", tt.name, tt.script)
		}
	}
}

func buildMultisig(m int, keys [][]byte, n int) []byte {
	s := []byte{smallIntOpcode(m)}
	for _, k := range keys {
		s = append(s, byte(len(k)))
		s = append(s, k...)
	}
	s = append(s, smallIntOpcode(n), opCheckMultisig)
	return s
}

func smallIntOpcode(v int) byte {
	if v == 0 {
		return opFalse
	}
	return op1 + byte(v-1)
}

func TestMatchMultisig(t *testing.T) {
	k1, k2, k3 := fill(33, 1), fill(33, 2), fill(33, 3)
	valid := buildMultisig(2, [][]byte{k1, k2, k3}, 3)

	keys, m, n, ok := matchMultisig(valid)
	if !ok || m != 2 || n != 3 || len(keys) != 3 {
		t.Fatalf("valid 2-of-3 multisig: ok=%v m=%d n=%d nkeys=%d", ok, m, n, len(keys))
	}
	if !bytes.Equal(keys[0], k1) || !bytes.Equal(keys[1], k2) || !bytes.Equal(keys[2], k3) {
		t.Fatalf("multisig keys not preserved in order: %x", keys)
	}

	// 1-of-1 with an uncompressed key.
	uk := fill(65, 9)
	valid1of1 := buildMultisig(1, [][]byte{uk}, 1)
	if _, m1, n1, ok1 := matchMultisig(valid1of1); !ok1 || m1 != 1 || n1 != 1 {
		t.Fatalf("valid 1-of-1 multisig: ok=%v m=%d n=%d", ok1, m1, n1)
	}

	invalid := []struct {
		name   string
		script []byte
	}{
		{"n does not match actual key count", buildMultisig(1, [][]byte{k1, k2}, 3)},
		{"m greater than n", func() []byte {
			s := buildMultisig(2, [][]byte{k1}, 1)
			return s
		}()},
		{"no OP_CHECKMULTISIG terminator", valid[:len(valid)-1]},
		{"truncated key push", append([]byte{smallIntOpcode(1), 33, 1, 2, 3}, smallIntOpcode(1), opCheckMultisig)},
		{"not starting with small-int", append([]byte{opDup}, valid[1:]...)},
	}
	for _, tt := range invalid {
		if _, _, _, ok := matchMultisig(tt.script); ok {
			t.Errorf("%s: unexpectedly matched as MULTISIG: %x", tt.name, tt.script)
		}
	}
}

func TestIsPushOnly_NeverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("isPushOnly panicked: %v", r)
		}
	}()
	for l := 0; l < 40; l++ {
		s := make([]byte, l)
		for i := range s {
			s[i] = byte(i * 13 % 256)
		}
		isPushOnly(s)
	}
}
