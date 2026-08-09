package chain

// WitnessStack is the ordered list of witness stack items for a single
// input. Item 0 is the bottom of the stack, matching the order Core's
// getrawtransaction/getblock RPC reports in txinwitness, and the order a
// P2QPK spend uses: [signature (17,088 bytes), pubkey (32 bytes)].
//
// A nil or empty WitnessStack means the input carries no witness data at
// all (pre-SegWit style spend).
type WitnessStack [][]byte

// IsEmpty reports whether the witness stack has no items.
func (w WitnessStack) IsEmpty() bool {
	return len(w) == 0
}
