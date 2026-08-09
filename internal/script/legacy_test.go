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

// fillPubKey builds a structurally valid-looking public key: a real
// compressed (0x02/0x03 prefix, 33 bytes) or uncompressed/hybrid
// (0x04/0x06/0x07 prefix, 65 bytes) size and prefix, matching Core's
// CPubKey::ValidSize (isPubKeyPush mirrors it) — the actual curve point
// data is irrelevant to structural classification.
func fillPubKey(prefix byte, seed byte) []byte {
	n := compressedPKLen
	if prefix == 0x04 || prefix == 0x06 || prefix == 0x07 {
		n = uncompressedPKLen
	}
	k := make([]byte, n)
	k[0] = prefix
	for i := 1; i < n; i++ {
		k[i] = seed + byte(i)
	}
	return k
}

// encodeScriptNum encodes v (1..20) exactly the way Core's MatchMultisig
// requires: OP_1-OP_16 for 1-16, or a minimal single-byte push for 17-20.
func encodeScriptNum(v int) []byte {
	if v >= 1 && v <= 16 {
		return []byte{smallIntOpcode(v)}
	}
	return []byte{0x01, byte(v)} // minimal single-byte push, per CheckMinimalPush
}

func buildMultisig(m int, keys [][]byte, n int) []byte {
	s := encodeScriptNum(m)
	for _, k := range keys {
		s = append(s, byte(len(k)))
		s = append(s, k...)
	}
	s = append(s, encodeScriptNum(n)...)
	s = append(s, opCheckMultisig)
	return s
}

func smallIntOpcode(v int) byte {
	if v == 0 {
		return opFalse
	}
	return op1 + byte(v-1)
}

func nKeys(count int, prefix byte, seedBase byte) [][]byte {
	keys := make([][]byte, count)
	for i := range keys {
		keys[i] = fillPubKey(prefix, seedBase+byte(i))
	}
	return keys
}

func TestMatchMultisig(t *testing.T) {
	k1, k2, k3 := fillPubKey(0x02, 1), fillPubKey(0x03, 2), fillPubKey(0x02, 3)
	valid := buildMultisig(2, [][]byte{k1, k2, k3}, 3)

	keys, m, n, ok := matchMultisig(valid)
	if !ok || m != 2 || n != 3 || len(keys) != 3 {
		t.Fatalf("valid 2-of-3 multisig: ok=%v m=%d n=%d nkeys=%d", ok, m, n, len(keys))
	}
	if !bytes.Equal(keys[0], k1) || !bytes.Equal(keys[1], k2) || !bytes.Equal(keys[2], k3) {
		t.Fatalf("multisig keys not preserved in order: %x", keys)
	}

	// 1-of-1 with an uncompressed key.
	uk := fillPubKey(0x04, 9)
	valid1of1 := buildMultisig(1, [][]byte{uk}, 1)
	if _, m1, n1, ok1 := matchMultisig(valid1of1); !ok1 || m1 != 1 || n1 != 1 {
		t.Fatalf("valid 1-of-1 multisig: ok=%v m=%d n=%d", ok1, m1, n1)
	}

	// 2-of-3 with a hybrid-prefix (0x06/0x07) uncompressed key mixed in.
	hk := fillPubKey(0x07, 20)
	validHybrid := buildMultisig(2, [][]byte{k1, hk}, 2)
	if _, _, _, okH := matchMultisig(validHybrid); !okH {
		t.Fatalf("valid 2-of-2 multisig with hybrid-prefix key: ok=false")
	}

	boundary := []struct {
		name     string
		m, total int
	}{
		{"16-of-16 (max small-int opcode)", 16, 16},
		{"17-of-17 (min pushed-number encoding)", 17, 17},
		{"20-of-20 (max, maxPubKeysPerMultisig)", 20, 20},
	}
	for _, tt := range boundary {
		t.Run(tt.name, func(t *testing.T) {
			keys := nKeys(tt.total, 0x02, 50)
			s := buildMultisig(tt.m, keys, tt.total)
			gotKeys, gotM, gotN, ok := matchMultisig(s)
			if !ok || gotM != tt.m || gotN != tt.total || len(gotKeys) != tt.total {
				t.Fatalf("ok=%v m=%d n=%d nkeys=%d, want m=%d n=%d nkeys=%d",
					ok, gotM, gotN, len(gotKeys), tt.m, tt.total, tt.total)
			}
		})
	}

	invalid := []struct {
		name   string
		script []byte
	}{
		{"n does not match actual key count", buildMultisig(1, [][]byte{k1, k2}, 3)},
		{"m greater than n", buildMultisig(2, [][]byte{k1}, 1)},
		{"no OP_CHECKMULTISIG terminator", valid[:len(valid)-1]},
		{"truncated key push", append([]byte{smallIntOpcode(1), 33, 1, 2, 3}, smallIntOpcode(1), opCheckMultisig)},
		{"not starting with small-int or push", append([]byte{opDup}, valid[1:]...)},
		{"21-of-21 exceeds maxPubKeysPerMultisig", buildMultisig(21, nKeys(21, 0x02, 60), 21)},
		{"0-of-0", buildMultisig(0, nil, 0)},
		{"non-minimal: m=1 pushed as data instead of OP_1", func() []byte {
			s := []byte{0x01, 0x01} // push(1) instead of OP_1 — CheckMinimalPush rejects this
			s = append(s, byte(len(k1)))
			s = append(s, k1...)
			s = append(s, smallIntOpcode(1), opCheckMultisig)
			return s
		}()},
		{"non-minimal: n=17 pushed as 2 bytes (0x11 0x00) instead of 1", func() []byte {
			s := []byte{smallIntOpcode(1)}
			for _, k := range nKeys(17, 0x02, 70) {
				s = append(s, byte(len(k)))
				s = append(s, k...)
			}
			s = append(s, 0x02, 0x11, 0x00) // 2-byte push of 17 — CScriptNum minimality rejects this
			s = append(s, opCheckMultisig)
			return s
		}()},
		{"pubkey push with invalid prefix byte (not 2/3/4/6/7)", func() []byte {
			bad := fillPubKey(0x02, 1)
			bad[0] = 0x05 // invalid prefix
			return buildMultisig(1, [][]byte{bad}, 1)
		}()},
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
