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
	compressed := fillPubKey(0x03, 3)
	valid := append([]byte{33}, append(append([]byte{}, compressed...), opCheckSig)...)
	got, ok := matchP2PK(valid)
	if !ok || !bytes.Equal(got, compressed) {
		t.Fatalf("valid compressed P2PK: ok=%v got=%x", ok, got)
	}

	uncompressed := fillPubKey(0x04, 4)
	validU := append([]byte{65}, append(append([]byte{}, uncompressed...), opCheckSig)...)
	gotU, okU := matchP2PK(validU)
	if !okU || !bytes.Equal(gotU, uncompressed) {
		t.Fatalf("valid uncompressed P2PK: ok=%v got=%x", okU, gotU)
	}

	// Real block-1 and block-5000 P2PK coinbase pubkeys (0x02-prefixed
	// compressed keys), confirmed live against Core — must keep matching.
	block1PubKey := mustHex(t, "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd")
	block1Script := append(append([]byte{33}, block1PubKey...), opCheckSig)
	if got, ok := matchP2PK(block1Script); !ok || !bytes.Equal(got, block1PubKey) {
		t.Fatalf("block-1 real P2PK vector regressed: ok=%v got=%x", ok, got)
	}
	block5000PubKey := mustHex(t, "029f42b0dbcf5cc08e4433a49f151c3d47539cdac8b30bae784d69100c1f4afdd4")
	block5000Script := append(append([]byte{33}, block5000PubKey...), opCheckSig)
	if got, ok := matchP2PK(block5000Script); !ok || !bytes.Equal(got, block5000PubKey) {
		t.Fatalf("block-5000 real P2PK vector regressed: ok=%v got=%x", ok, got)
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

	// PR #1 review item 1: right length, wrong/invalid serialized-key
	// prefix — Core's MatchPayToPubkey requires CPubKey::ValidSize, not
	// just the push length, so these must NOT match as P2PK.
	invalidPrefix := []struct {
		name   string
		script []byte
	}{
		{
			"33-byte key with invalid prefix 0x05",
			func() []byte {
				k := fillPubKey(0x02, 6)
				k[0] = 0x05
				return append(append([]byte{33}, k...), opCheckSig)
			}(),
		},
		{
			"65-byte key with invalid prefix 0x05",
			func() []byte {
				k := fillPubKey(0x04, 7)
				k[0] = 0x05
				return append(append([]byte{65}, k...), opCheckSig)
			}(),
		},
		{
			"65-byte key with invalid prefix 0x08",
			func() []byte {
				k := fillPubKey(0x04, 8)
				k[0] = 0x08
				return append(append([]byte{65}, k...), opCheckSig)
			}(),
		},
	}
	for _, tt := range invalidPrefix {
		if _, ok := matchP2PK(tt.script); ok {
			t.Errorf("%s: unexpectedly matched as P2PK: %x", tt.name, tt.script)
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

// pushData4 encodes data as an explicit OP_PUSHDATA4 push, regardless of
// how small data is — real encoders would never do this for a 33/65-byte
// pubkey (it's non-minimal, and MatchMultisig doesn't enforce push
// minimality on pubkey operands, only on the m/n numbers — see
// multisigScriptNumber), but Core's GetOp-based token reader accepts it
// structurally, so nextToken/matchMultisig must too.
func pushData4(data []byte) []byte {
	n := len(data)
	out := []byte{opPushData4, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	return append(out, data...)
}

func TestNextToken_PushData4(t *testing.T) {
	data := fillPubKey(0x02, 1)
	tok, next, ok := nextToken(pushData4(data), 0)
	if !ok {
		t.Fatal("expected ok=true for a valid OP_PUSHDATA4 token")
	}
	if tok.opcode != opPushData4 {
		t.Errorf("opcode = %#x, want %#x", tok.opcode, opPushData4)
	}
	if !bytes.Equal(tok.data, data) {
		t.Errorf("data = %x, want %x", tok.data, data)
	}
	if next != 5+len(data) {
		t.Errorf("next = %d, want %d", next, 5+len(data))
	}

	truncated := []struct {
		name   string
		script []byte
	}{
		{"no length bytes at all", []byte{opPushData4}},
		{"length bytes truncated (only 2 of 4)", []byte{opPushData4, 0x21, 0x00}},
		{"length says 33 bytes but none follow", []byte{opPushData4, 33, 0, 0, 0}},
		{"length says 33 bytes but only 10 follow", append([]byte{opPushData4, 33, 0, 0, 0}, fill(10, 1)...)},
	}
	for _, tt := range truncated {
		if _, _, ok := nextToken(tt.script, 0); ok {
			t.Errorf("%s: expected ok=false, got ok=true", tt.name)
		}
	}
}

func TestNextToken_NeverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nextToken panicked: %v", r)
		}
	}()
	inputs := [][]byte{
		nil, {}, {opPushData4}, {opPushData4, 0xff, 0xff, 0xff, 0xff},
		{opPushData4, 0xff, 0xff, 0xff, 0x7f}, // huge but non-negative claimed length
		{opPushData2, 0xff, 0xff}, {opPushData1, 0xff},
	}
	for _, in := range inputs {
		for i := 0; i <= len(in); i++ {
			nextToken(in, i)
		}
	}
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

	// PR #1 review item 2: a pubkey pushed via OP_PUSHDATA4 (rather than a
	// minimal direct push) must still tokenize and classify consistently
	// with Core's GetOp-based MatchMultisig — GetOp/nextToken don't enforce
	// push minimality on pubkey operands, only multisigScriptNumber does
	// that for m/n.
	t.Run("pubkey pushed via OP_PUSHDATA4", func(t *testing.T) {
		pd4Key := fillPubKey(0x02, 30)
		s := []byte{smallIntOpcode(1)}
		s = append(s, pushData4(pd4Key)...)
		s = append(s, smallIntOpcode(1), opCheckMultisig)

		keys, m, n, ok := matchMultisig(s)
		if !ok || m != 1 || n != 1 {
			t.Fatalf("1-of-1 multisig with OP_PUSHDATA4 pubkey: ok=%v m=%d n=%d", ok, m, n)
		}
		if len(keys) != 1 || !bytes.Equal(keys[0], pd4Key) {
			t.Fatalf("keys = %x, want [%x]", keys, pd4Key)
		}
	})

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
