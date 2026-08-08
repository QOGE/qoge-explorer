package chain

// Transaction is the canonical, Core-shape-independent representation of a
// single transaction.
type Transaction struct {
	TxID     string // hex-encoded txid, lowercase
	Version  int32
	LockTime uint32

	// Size, VSize, and Weight mirror the fields Core's verbose RPC output
	// already provides at the transaction level (confirmed present in
	// TxToUniv — see docs/ARCHITECTURE.md §9). eIquidus stores none of
	// these; this model deliberately does.
	Size   int
	VSize  int
	Weight int

	IsCoinbase bool

	Inputs  []Input
	Outputs []Output

	// Fee is an optional, derived field. It is intentionally left unset
	// (nil) in Phase 2A — fee computation requires resolving every input's
	// previous output value, which is indexer/persistence work, not part of
	// the canonical in-memory model locked down here.
	Fee *Amount
}
