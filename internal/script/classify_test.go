package script

import (
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test hex %q: %v", s, err)
	}
	return b
}

// TestClassify_RealQOGEVectors uses scriptPubKeys fetched read-only from the
// local Qogecoin Core node (getblock/getrawtransaction), not manufactured.
// Sources, by height and txid, are recorded in each case's comment.
func TestClassify_RealQOGEVectors(t *testing.T) {
	tests := []struct {
		name   string
		hex    string
		want   Type
		source string
	}{
		{
			name:   "block 1 coinbase — bare P2PK",
			hex:    "21029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cdac",
			want:   TypeP2PK,
			source: "height=1 txid=1d4bdd70951bae6dd62b265a1877b7b140ea86b6ff0cd51102eec801c3947a1d vout=0",
		},
		{
			name:   "block 5000 coinbase — bare P2PK",
			hex:    "21029f42b0dbcf5cc08e4433a49f151c3d47539cdac8b30bae784d69100c1f4afdd4ac",
			want:   TypeP2PK,
			source: "height=5000 txid=78743cfb1f2264365e21ba1b5034a98218b0f5b64e6f3f447274df9c000b39af vout=0",
		},
		{
			name:   "block 8000 coinbase — P2PKH (post block-7985 format switch)",
			hex:    "76a914db6cdf671aa4dc3a395b934ca08bffb54658f36c88ac",
			want:   TypeP2PKH,
			source: "height=8000 txid=a8ee14b21e7d42a4e9c155de159c9836ce932d4c4cccf77a0f23a71acd031b45 vout=0",
		},
		{
			name:   "block 38393 — OP_RETURN witness commitment",
			hex:    "6a24aa21a9ede2f61c3f71d1defd3fa999dfa36953755c690689799962b48bebd836974e8cf9",
			want:   TypeNullData,
			source: "height=38393 txid=01d172762f277d6b2a48bc935ed2603dddd23f9eb85d84bd325fde05a3787be0 vout=1",
		},
		{
			name:   "block 494289 — native SegWit P2WPKH",
			hex:    "001471fcd715a320938d8dfa1b56d9acdab9b1616be1",
			want:   TypeP2WPKH,
			source: "height=494289 txid=180c6aee4e8ff354868f7f44945192e1bd2941827413203f5b69c36bb3fb4a29 vout=22",
		},
		{
			name:   "block 1284510 — real Taproot output (must NOT be misclassified as P2QPK)",
			hex:    "51202e44fe044d16a3b7900c179d9bb3fc005f0d5e92c89b8c7c0d340c7d6f56077c",
			want:   TypeUnknownWitness,
			source: "height=1284510 txid=8c7381260e076f781de4c0c5246c709579c80e964738a719579ae4fd5c312106 vout=0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(mustHex(t, tt.hex))
			if got.Type != tt.want {
				t.Errorf("Classify(%s) = %s, want %s\n  source: %s", tt.name, got.Type, tt.want, tt.source)
			}
		})
	}
}

// TestClassify_P2QPK is the critical structural-detection test: P2QPK must
// be identified purely from witness version 2 + 32-byte program, and every
// near-miss must fall to UNKNOWN_WITNESS, never P2QPK and never any other
// type. No real P2QPK output exists on QOGE mainnet yet (pre-activation) —
// these are source-derived synthetic vectors, per task instruction; none
// were broadcast anywhere.
func TestClassify_P2QPK(t *testing.T) {
	commitment := program(32) // a synthetic 32-byte HASH256(pubkey) commitment

	t.Run("exact OP_2 | PUSH32 | 32-byte program -> P2QPK", func(t *testing.T) {
		s := buildWitnessScript(op1+1, 32, commitment) // op1+1 == OP_2 == witness version 2
		got := Classify(s)
		if got.Type != TypeP2QPK {
			t.Fatalf("Type = %s, want %s (script=%x)", got.Type, TypeP2QPK, s)
		}
		if got.WitnessVersion == nil || *got.WitnessVersion != 2 {
			t.Errorf("WitnessVersion = %v, want 2", got.WitnessVersion)
		}
		if len(got.WitnessProgram) != 32 {
			t.Errorf("WitnessProgram len = %d, want 32", len(got.WitnessProgram))
		}
	})

	t.Run("also confirm real scriptPubKey byte shape 0x52 0x20 <32 bytes>", func(t *testing.T) {
		s := append([]byte{0x52, 0x20}, commitment...)
		got := Classify(s)
		if got.Type != TypeP2QPK {
			t.Fatalf("Type = %s, want %s", got.Type, TypeP2QPK)
		}
	})

	t.Run("wrong witness version: v2 payload but v1 (Taproot) version byte -> not P2QPK", func(t *testing.T) {
		s := buildWitnessScript(op1, 32, commitment) // OP_1 = witness v1
		got := Classify(s)
		if got.Type == TypeP2QPK {
			t.Fatalf("v1/32 must not classify as P2QPK, got %s", got.Type)
		}
		if got.Type != TypeUnknownWitness {
			t.Errorf("v1/32 Type = %s, want %s", got.Type, TypeUnknownWitness)
		}
	})

	t.Run("wrong program length: v2/31 -> UNKNOWN_WITNESS, not P2QPK", func(t *testing.T) {
		p := program(31)
		s := buildWitnessScript(op1+1, 31, p)
		got := Classify(s)
		if got.Type == TypeP2QPK {
			t.Fatalf("v2/31 must not classify as P2QPK")
		}
		if got.Type != TypeUnknownWitness {
			t.Errorf("v2/31 Type = %s, want %s", got.Type, TypeUnknownWitness)
		}
	})

	t.Run("wrong program length: v2/33 -> UNKNOWN_WITNESS, not P2QPK", func(t *testing.T) {
		p := program(33)
		s := buildWitnessScript(op1+1, 33, p)
		got := Classify(s)
		if got.Type == TypeP2QPK {
			t.Fatalf("v2/33 must not classify as P2QPK")
		}
		if got.Type != TypeUnknownWitness {
			t.Errorf("v2/33 Type = %s, want %s", got.Type, TypeUnknownWitness)
		}
	})

	t.Run("wrong witness version: v3/32 -> UNKNOWN_WITNESS, not P2QPK", func(t *testing.T) {
		s := buildWitnessScript(op1+2, 32, commitment) // OP_3 = witness v3
		got := Classify(s)
		if got.Type == TypeP2QPK {
			t.Fatalf("v3/32 must not classify as P2QPK")
		}
		if got.Type != TypeUnknownWitness {
			t.Errorf("v3/32 Type = %s, want %s", got.Type, TypeUnknownWitness)
		}
	})

	t.Run("other future witness versions with 32-byte programs -> UNKNOWN_WITNESS", func(t *testing.T) {
		for v := 4; v <= 16; v++ {
			s := buildWitnessScript(op1+byte(v-1), 32, commitment)
			got := Classify(s)
			if got.Type == TypeP2QPK {
				t.Fatalf("v%d/32 must not classify as P2QPK", v)
			}
			if got.Type != TypeUnknownWitness {
				t.Errorf("v%d/32 Type = %s, want %s", v, got.Type, TypeUnknownWitness)
			}
		}
	})

	t.Run("trailing bytes after a valid P2QPK program -> UNKNOWN (not a witness program at all)", func(t *testing.T) {
		s := append(buildWitnessScript(op1+1, 32, commitment), 0xff)
		got := Classify(s)
		if got.Type == TypeP2QPK {
			t.Fatalf("script with trailing byte must not classify as P2QPK: %x", s)
		}
	})

	t.Run("truncated P2QPK-shaped script -> not P2QPK", func(t *testing.T) {
		full := buildWitnessScript(op1+1, 32, commitment)
		truncated := full[:len(full)-1]
		got := Classify(truncated)
		if got.Type == TypeP2QPK {
			t.Fatalf("truncated script must not classify as P2QPK: %x", truncated)
		}
	})

	t.Run("empty script -> UNKNOWN, not P2QPK", func(t *testing.T) {
		got := Classify(nil)
		if got.Type != TypeUnknown {
			t.Errorf("Classify(nil) = %s, want %s", got.Type, TypeUnknown)
		}
	})
}

func TestClassify_AllRequiredTypes(t *testing.T) {
	pk := fill(33, 1)
	pkh := fill(20, 2)
	sh := fill(20, 3)
	p2wpkhProg := fill(20, 4)
	p2wshProg := fill(32, 5)
	p2qpkProg := fill(32, 6)

	tests := []struct {
		name   string
		script []byte
		want   Type
	}{
		{
			"P2PK",
			append(append([]byte{33}, pk...), opCheckSig),
			TypeP2PK,
		},
		{
			"P2PKH",
			append(append([]byte{opDup, opHash160, 20}, pkh...), opEqualVerify, opCheckSig),
			TypeP2PKH,
		},
		{
			"P2SH",
			append(append([]byte{opHash160, 20}, sh...), opEqual),
			TypeP2SH,
		},
		{
			"P2WPKH",
			buildWitnessScript(opFalse, 20, p2wpkhProg),
			TypeP2WPKH,
		},
		{
			"P2WSH",
			buildWitnessScript(opFalse, 32, p2wshProg),
			TypeP2WSH,
		},
		{
			"P2QPK",
			buildWitnessScript(op1+1, 32, p2qpkProg),
			TypeP2QPK,
		},
		{
			"NULLDATA",
			append([]byte{opReturn, 4}, fill(4, 7)...),
			TypeNullData,
		},
		{
			"MULTISIG",
			buildMultisig(2, [][]byte{fill(33, 8), fill(33, 9), fill(33, 10)}, 3),
			TypeMultisig,
		},
		{
			"UNKNOWN_WITNESS (v4/25, valid witness program shape, unrecognized combo)",
			buildWitnessScript(op1+3, 25, fill(25, 11)),
			TypeUnknownWitness,
		},
		{
			"UNKNOWN (random non-matching script)",
			[]byte{0x01, 0x02, 0x03, 0x04, 0x05},
			TypeUnknown,
		},
		{
			"UNKNOWN (nil script)",
			nil,
			TypeUnknown,
		},
		{
			"UNKNOWN (empty script)",
			[]byte{},
			TypeUnknown,
		},
	}

	seen := map[Type]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.script)
			if got.Type != tt.want {
				t.Errorf("Classify(%x) = %s, want %s", tt.script, got.Type, tt.want)
			}
			seen[got.Type] = true
		})
	}

	// Confirm every required classification in the task spec is exercised
	// by at least one case above.
	required := []Type{
		TypeP2PK, TypeP2PKH, TypeP2SH, TypeP2WPKH, TypeP2WSH,
		TypeP2QPK, TypeNullData, TypeMultisig, TypeUnknownWitness, TypeUnknown,
	}
	for _, want := range required {
		if !seen[want] {
			t.Errorf("no test case exercised required type %s", want)
		}
	}
}

// TestClassify_NeverPanics feeds a wide range of malformed/adversarial byte
// sequences through Classify and confirms none of them panic.
func TestClassify_NeverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Classify panicked: %v", r)
		}
	}()

	inputs := [][]byte{
		nil,
		{},
		{0x00},
		{0x51},
		{0xff},
		{opReturn},
		{opDup, opHash160},
		{opHash160, 20},
		{0x4c},       // OP_PUSHDATA1 with no length byte
		{0x4c, 0xff}, // OP_PUSHDATA1 claiming 255 bytes, none present
		{0x4d, 0xff, 0xff},
		{0x4e, 0xff, 0xff, 0xff, 0xff},
		{op1, 0xff}, // small-int version, bogus push length
		{opCheckMultisig},
	}
	for _, in := range inputs {
		Classify(in)
	}

	// Every byte length from 0 to 80 with pseudo-random content.
	for l := 0; l <= 80; l++ {
		s := make([]byte, l)
		for i := range s {
			s[i] = byte((i*31 + l*7) % 256)
		}
		Classify(s)
	}
}

func FuzzClassify(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x52, 0x20})
	f.Add(append([]byte{0x52, 0x20}, fill(32, 1)...))
	f.Add([]byte{opDup, opHash160, 20, 1, 2, 3, opEqualVerify, opCheckSig})
	f.Add([]byte{opReturn, 0x4c, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Classify panicked on %x: %v", data, r)
			}
		}()
		Classify(data)
	})
}
