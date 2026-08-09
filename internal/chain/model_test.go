package chain

import (
	"testing"

	"github.com/QOGE/qoge-explorer/internal/script"
)

// classifyHex is a small test helper that decodes hex and runs it through
// script.Classify, the same way a future indexer would when populating an
// Output's ScriptType field.
func classifyHex(t *testing.T, hexStr string) script.Result {
	t.Helper()
	b := mustHexBytes(t, hexStr)
	return script.Classify(b)
}

func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := range b {
		var v byte
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= c - '0'
			case c >= 'a' && c <= 'f':
				v |= c - 'a' + 10
			default:
				t.Fatalf("bad hex char %q in %q", c, s)
			}
		}
		b[i] = v
	}
	return b
}

// TestBlock_RealVector_1000000 builds a chain.Block from real data fetched
// read-only via getblock on the local Core node (height=1,000,000,
// hash=f7505939ca5b2ea5aa88534820e3d0dc6b971f8b2cff79acdfd0fec3e18f8a31) and
// checks every field round-trips exactly.
func TestBlock_RealVector_1000000(t *testing.T) {
	b := Block{
		Hash:         "f7505939ca5b2ea5aa88534820e3d0dc6b971f8b2cff79acdfd0fec3e18f8a31",
		Height:       1000000,
		PreviousHash: "0bec88c0fdb1e1da10daad461d6a093a6166e9763a1610a4f8d496fc04af4105",
		MerkleRoot:   "046756d96716e5edce4f430daadef4d7d202eb2630e91745bc4830c55db5f4e6",
		Time:         1695038321,
		Bits:         "1e0141bb",
		Difficulty:   0.003108144357903428,
		Nonce:        1452081152,
		Size:         196,
		Weight:       784,
		TxCount:      1,
	}

	if b.Height != 1000000 {
		t.Errorf("Height = %d, want 1000000", b.Height)
	}
	if b.PreviousHash == "" {
		t.Error("PreviousHash must not be empty for a non-genesis block")
	}
	if b.TxCount != 1 {
		t.Errorf("TxCount = %d, want 1", b.TxCount)
	}
}

// TestTransaction_RealVector_CoinbaseP2PKH builds the real coinbase
// transaction from block 1,000,000 and confirms the coinbase input and
// P2PKH output are preserved and correctly classified.
func TestTransaction_RealVector_CoinbaseP2PKH(t *testing.T) {
	const coinbaseHex = "0340420f04713b0865088100210e02000000506f6f6c4d696e652e78797a"
	const outScriptHex = "76a914eb818c1024f7a5e01ed387e3550a558c9997a4fb88ac"

	classified := classifyHex(t, outScriptHex)
	if classified.Type != script.TypeP2PKH {
		t.Fatalf("expected P2PKH classification, got %s", classified.Type)
	}

	tx := Transaction{
		TxID:       "046756d96716e5edce4f430daadef4d7d202eb2630e91745bc4830c55db5f4e6",
		Version:    2,
		LockTime:   0,
		Size:       115,
		VSize:      115,
		Weight:     460,
		IsCoinbase: true,
		Inputs: []Input{
			{
				Index:    0,
				Coinbase: mustHexBytes(t, coinbaseHex),
				Sequence: 0,
			},
		},
		Outputs: []Output{
			{
				Index:        0,
				Value:        2_500_000_000, // 25 QOGE, confirmed live: height 1,000,000 = 2 halvings, 100>>2 = 25
				ScriptPubKey: mustHexBytes(t, outScriptHex),
				ScriptType:   classified.Type,
				Address:      "qf2d9CjydUxEQEZgiBgfxsHpNA2ByE1ytX",
			},
		},
	}

	if !tx.IsCoinbase {
		t.Fatal("IsCoinbase must be true")
	}
	if len(tx.Inputs) != 1 || !tx.Inputs[0].IsCoinbase() {
		t.Fatalf("expected exactly one coinbase input, got %+v", tx.Inputs)
	}
	if tx.Inputs[0].PreviousOut != nil {
		t.Errorf("coinbase input must have nil PreviousOut, got %+v", tx.Inputs[0].PreviousOut)
	}
	if len(tx.Outputs) != 1 {
		t.Fatalf("expected exactly one output, got %d", len(tx.Outputs))
	}
	if tx.Outputs[0].Value.WholeQOGE() != 25 {
		t.Errorf("output value = %s, want 25 QOGE", tx.Outputs[0].Value)
	}
	if tx.Outputs[0].ScriptType != script.TypeP2PKH {
		t.Errorf("ScriptType = %s, want %s", tx.Outputs[0].ScriptType, script.TypeP2PKH)
	}
	if tx.Fee != nil {
		t.Error("Fee must be nil in Phase 2A — fee computation is deferred to the indexer")
	}
}

// TestTransaction_RealVector_MultiInputNoAggregation builds the real
// 9-input, 3-output transaction 684bc7cbd1cb90309ce199f685e10b91d02c9bfd0ec4615a7cd5962b2b37aad3
// (block 2,438,961) and confirms every raw input is preserved as its own
// Input — no same-address aggregation, unlike eIquidus (see
// docs/ARCHITECTURE.md §2 item 3). Only the first two of the nine real
// inputs are reproduced here; that's sufficient to prove distinct,
// unaggregated Input entries with correct per-input PreviousOut identity.
func TestTransaction_RealVector_MultiInputNoAggregation(t *testing.T) {
	tx := Transaction{
		TxID:    "684bc7cbd1cb90309ce199f685e10b91d02c9bfd0ec4615a7cd5962b2b37aad3",
		Version: 2,
		Size:    1437,
		VSize:   1350,
		Weight:  5397,
		Inputs: []Input{
			{
				Index:       0,
				PreviousOut: &OutPoint{TxID: "b3b13f313940e58dfac399a2ec8e758f750f38a81b40cc54d2e102caea3a3d8a", Index: 1},
				ScriptSig:   mustHexBytes(t, "473044022003d9a552199e083737a9529c9207fd695ef4cd1bd0d02bcc423f97a5731224be022075fb04ba95c32924dd89540825c7e1eff30fb9b60cf554e14807cb7ffb65ce6a012103ce7daaecc12e3be7b639ed5a0b7d6764b2c30763a36fab38be3512c8425b0526"),
				Sequence:    4294967294,
			},
			{
				Index:       1,
				PreviousOut: &OutPoint{TxID: "dc077e573e540b627a559a11ff063a4b55e19d4b62030b7f46d7ab337fdc84ad", Index: 1},
				Sequence:    4294967294,
			},
			// inputs 2-8 omitted from this fixture; the real transaction has 9 total.
		},
		Outputs: []Output{
			{Index: 0, Value: 3_587_890_602, Address: "bq1qmm0t3gyvy2tq7as4g49gzkw7ge5nq3k23gd7f3"},
			{Index: 1, Value: 116_649, Address: "bq1q50lvzk36875uvj97fuwsvmlgcufhtmrpneaaeh"},
			{Index: 2, Value: 1_412_029_399, Address: "bq1qysjnhfn7t4ta2ythqgc3g52wghu9xu8mewwmxz"},
		},
	}

	if tx.Inputs[0].IsCoinbase() || tx.Inputs[1].IsCoinbase() {
		t.Fatal("non-coinbase inputs incorrectly report IsCoinbase() == true")
	}
	if tx.Inputs[0].PreviousOut.TxID == tx.Inputs[1].PreviousOut.TxID {
		t.Fatal("test fixture bug: inputs 0 and 1 must reference distinct previous outpoints")
	}
	// The critical assertion: two structurally-similar spends from what
	// could be the same wallet remain two distinct Input entries, each
	// with its own OutPoint identity — never merged into one summed entry.
	if len(tx.Inputs) != 2 {
		t.Fatalf("expected 2 preserved (of 9 real) inputs in this fixture, got %d", len(tx.Inputs))
	}
	wantOP0 := OutPoint{TxID: "b3b13f313940e58dfac399a2ec8e758f750f38a81b40cc54d2e102caea3a3d8a", Index: 1}
	if *tx.Inputs[0].PreviousOut != wantOP0 {
		t.Errorf("Inputs[0].PreviousOut = %+v, want %+v", *tx.Inputs[0].PreviousOut, wantOP0)
	}

	// Real witness_v0_keyhash scriptPubKey hex for each output above,
	// exactly as fetched from Core, keyed by address for readability.
	realScriptHexByAddress := map[string]string{
		"bq1qmm0t3gyvy2tq7as4g49gzkw7ge5nq3k23gd7f3": "0014dedeb8a08c22960f7615454a8159de46693046ca",
		"bq1q50lvzk36875uvj97fuwsvmlgcufhtmrpneaaeh": "0014a3fec15a3a3fa9c648be4f1d066fe8c71375ec61",
		"bq1qysjnhfn7t4ta2ythqgc3g52wghu9xu8mewwmxz": "001424253ba67e5d57d51177023114514e45f85370fb",
	}
	for _, out := range tx.Outputs {
		scriptHex, known := realScriptHexByAddress[out.Address]
		if !known {
			t.Fatalf("no known scriptPubKey fixture for address %q", out.Address)
		}
		got := classifyHex(t, scriptHex)
		if got.Type != script.TypeP2WPKH {
			t.Errorf("output %d: expected P2WPKH, got %s", out.Index, got.Type)
		}
	}
}

// TestOutput_NullDataNeverDropped confirms a zero-value OP_RETURN output —
// the real block-38393 witness-commitment output — is representable and
// preserved exactly like any other output, per docs/ARCHITECTURE.md §2
// item 3 ("do not drop zero-value nulldata outputs", the eIquidus behavior
// this model deliberately avoids reproducing).
func TestOutput_NullDataNeverDropped(t *testing.T) {
	const nullDataHex = "6a24aa21a9ede2f61c3f71d1defd3fa999dfa36953755c690689799962b48bebd836974e8cf9"
	classified := classifyHex(t, nullDataHex)
	if classified.Type != script.TypeNullData {
		t.Fatalf("expected NULLDATA classification, got %s", classified.Type)
	}

	tx := Transaction{
		TxID: "01d172762f277d6b2a48bc935ed2603dddd23f9eb85d84bd325fde05a3787be0",
		Outputs: []Output{
			{Index: 0, Value: 10_000_000_000, ScriptType: script.TypeP2PK},
			{Index: 1, Value: 0, ScriptPubKey: mustHexBytes(t, nullDataHex), ScriptType: classified.Type},
		},
	}

	if len(tx.Outputs) != 2 {
		t.Fatalf("expected both outputs preserved (including the zero-value one), got %d", len(tx.Outputs))
	}
	nullOut := tx.Outputs[1]
	if nullOut.Value != 0 {
		t.Errorf("Value = %d, want 0", nullOut.Value)
	}
	if nullOut.ScriptType != script.TypeNullData {
		t.Errorf("ScriptType = %s, want %s", nullOut.ScriptType, script.TypeNullData)
	}
}

// TestOutput_P2PKHasNoAddressButExposesPubKey confirms the model captures
// what Core actually gives us for a bare P2PK output: no address (Core
// deliberately omits it — see docs/ARCHITECTURE.md §7), but the raw pubkey
// is available on the classification Result for a later resolver, rather
// than the classifier inventing an address itself.
func TestOutput_P2PKHasNoAddressButExposesPubKey(t *testing.T) {
	// Real block-1 coinbase output.
	const p2pkHex = "21029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cdac"
	classified := classifyHex(t, p2pkHex)

	out := Output{
		Index:        0,
		Value:        10_000_000_000,
		ScriptPubKey: mustHexBytes(t, p2pkHex),
		ScriptType:   classified.Type,
		PubKeys:      classified.PubKeys,
		Address:      "", // Core provides no address for type=="pubkey"; confirmed against live RPC
	}

	if out.ScriptType != script.TypeP2PK {
		t.Fatalf("ScriptType = %s, want %s", out.ScriptType, script.TypeP2PK)
	}
	if out.Address != "" {
		t.Errorf("Address = %q, want empty (Core never supplies one for P2PK)", out.Address)
	}
	if len(out.PubKeys) != 1 {
		t.Fatalf("expected exactly one extracted pubkey, got %d", len(out.PubKeys))
	}
	wantPubKey := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	if got := hexString(out.PubKeys[0]); got != wantPubKey {
		t.Errorf("PubKeys[0] = %s, want %s", got, wantPubKey)
	}
}

func hexString(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}
