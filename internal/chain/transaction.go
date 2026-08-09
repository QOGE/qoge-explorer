package chain

// Transaction is the canonical, Core-shape-independent representation of a
// single OBSERVED transaction serialization — one concrete witness variant,
// not just an abstract txid. See docs/ARCHITECTURE.md §3a for the full
// txid/wtxid/transaction/transaction-variant/block-occurrence terminology
// this type deliberately mirrors.
type Transaction struct {
	// TxID is Core's GetHash(): the serialization hash WITHOUT witness
	// data. RPC field "txid". Identifies the non-witness transaction body
	// (inputs' prevouts/scriptSigs, outputs) — this is what
	// internal/store's `transactions` table is keyed by.
	TxID string

	// WTxID is Core's GetWitnessHash(): the serialization hash INCLUDING
	// witness data. RPC field "hash", NOT "txid" — confirmed from Core's
	// TxToUniv. For a transaction with no witness data at all, WTxID ==
	// TxID. Because txid deliberately excludes witness data, two different
	// witness stacks can satisfy the same txid and produce two different
	// WTxIDs — both legitimately observable across competing
	// blocks/reorgs. This is what internal/store's `transaction_variants`
	// table is keyed by.
	WTxID string

	// Version has uint32 semantics, not int32. Core's in-memory
	// CTransaction stores nVersion as int32_t, but TxToUniv exposes it to
	// RPC as static_cast<uint32_t>(tx.nVersion) and treats it as unsigned
	// in consensus checks — the same RPC-facing representation this project
	// already follows for LockTime, sequence, and nonce. See
	// docs/ARCHITECTURE.md §3 for the C++-type-vs-RPC-representation
	// distinction.
	Version  uint32
	LockTime uint32

	// Size, VSize, and Weight mirror the fields Core's verbose RPC output
	// already provides at the transaction level (confirmed present in
	// TxToUniv — see docs/ARCHITECTURE.md §9). eIquidus stores none of
	// these; this model deliberately does. They describe THIS observed
	// serialization/variant (WTxID), which is why they stay on this single
	// Go struct even though the SQL schema splits the non-witness body
	// (transactions) from the per-variant metrics (transaction_variants) —
	// the in-memory model doesn't need that split, only the persistence
	// layer does.
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
