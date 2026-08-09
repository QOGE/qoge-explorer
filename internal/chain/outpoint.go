package chain

import "fmt"

// OutPoint is the canonical identity of a transaction output: txid:vout.
// An output is created exactly once (by its transaction) and can be spent
// exactly once (by one input elsewhere on the canonical chain) — this type
// is the key that later persistence/UTXO logic hangs off of.
//
// OutPoint is a plain comparable struct (string + uint32) so it can be used
// directly as a map key and compared with ==, which later indexing code
// relies on for deterministic UTXO bookkeeping.
type OutPoint struct {
	TxID  string // hex-encoded txid, lowercase, as returned by Core RPC
	Index uint32 // vout index within that transaction
}

// String renders the OutPoint in the conventional "txid:vout" form.
func (o OutPoint) String() string {
	return fmt.Sprintf("%s:%d", o.TxID, o.Index)
}
