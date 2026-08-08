package chain

import (
	"encoding/hex"

	"github.com/QOGE/qoge-explorer/internal/script"
)

// Output is one raw transaction output (vout), preserved 1:1 with what Core
// reports — including zero-value outputs (e.g. OP_RETURN/nulldata
// commitments), which must never be silently dropped. See
// docs/ARCHITECTURE.md §2 item 3.
type Output struct {
	// Index is this output's position within its transaction's vout list.
	Index uint32

	// Value is the output's value in satoshis. Zero is valid and meaningful
	// (e.g. OP_RETURN outputs) — it is never used as a signal to omit the
	// output from the model.
	Value Amount

	// ScriptPubKey is the raw, unparsed output script bytes.
	ScriptPubKey []byte

	// ScriptType is the structural classification of ScriptPubKey, as
	// determined by internal/script. The zero value ("") means
	// classification has not been performed yet — callers that construct an
	// Output should normally set this via script.Classify.
	ScriptType script.Type

	// WitnessVersion and WitnessProgram are populated when ScriptPubKey is a
	// SegWit-style witness program (P2WPKH, P2WSH, P2QPK, or an
	// unrecognized future witness version/length). Nil/empty otherwise.
	WitnessVersion *int
	WitnessProgram []byte

	// PubKeys holds the raw public key(s) extracted structurally from a
	// P2PK or bare-MULTISIG scriptPubKey, for a later address resolver to
	// consume. The classifier never invents an address itself — see
	// docs/ARCHITECTURE.md §7. Nil for every other script type.
	PubKeys [][]byte

	// Address is the address Core's own RPC reported for this output, taken
	// as-is, if any. Empty when Core did not supply one (e.g. bare P2PK,
	// where Core deliberately omits the address field — see
	// docs/ARCHITECTURE.md §7 "P2PK handling").
	Address string
}

// ScriptPubKeyHex returns the output script as a lowercase hex string.
func (o Output) ScriptPubKeyHex() string {
	return hex.EncodeToString(o.ScriptPubKey)
}
