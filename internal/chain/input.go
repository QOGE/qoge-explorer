package chain

// Input is one raw transaction input (vin), preserved 1:1 with what Core
// reports — no aggregation by address, no merging of multiple inputs from
// the same source. See docs/ARCHITECTURE.md §2 item 3 for why: eIquidus's
// same-address vin aggregation was an undocumented, irreversible
// transformation of Core's data, and this model deliberately avoids
// reproducing that pattern.
type Input struct {
	// Index is this input's position within its transaction's vin list.
	Index uint32

	// PreviousOut identifies the output being spent. Nil for a coinbase
	// input, which has no previous output.
	PreviousOut *OutPoint

	// Coinbase holds the raw coinbase script bytes. Non-nil only when
	// PreviousOut is nil (i.e. this is the coinbase input).
	Coinbase []byte

	// ScriptSig is the raw, unparsed scriptSig bytes for a non-coinbase
	// input. Empty for coinbase inputs and for pure-SegWit spends that
	// carry no scriptSig.
	ScriptSig []byte

	// Sequence is the raw nSequence field.
	Sequence uint32

	// Witness is the input's witness stack, if any. Empty/nil for
	// non-SegWit spends.
	Witness WitnessStack
}

// IsCoinbase reports whether this input is a coinbase input (no previous
// output being spent).
func (in Input) IsCoinbase() bool {
	return in.PreviousOut == nil
}
