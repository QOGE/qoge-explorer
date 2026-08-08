package chain

// Block is the canonical, Core-shape-independent representation of a single
// block. Fields mirror the `blocks` table proposed in
// docs/ARCHITECTURE.md §3. This struct intentionally carries no consensus
// logic (no validation, no subsidy/difficulty computation) — it is a pure
// data holder; Qogecoin Core remains the sole authority on chain truth.
type Block struct {
	Hash         string // hex-encoded block hash, lowercase
	Height       int64
	PreviousHash string // empty only for genesis
	MerkleRoot   string
	Time         int64   // block header timestamp, unix seconds
	Bits         string  // compact-target, as reported by Core (hex string)
	Difficulty   float64 // display only; never used for consensus decisions
	Nonce        uint32
	Size         int
	Weight       int

	// TxCount is the transaction count as reported by Core (e.g. nTx),
	// independent of whether Transactions below is populated — a Block
	// built from a lightweight header fetch may know the count without
	// holding the full transaction bodies.
	TxCount int

	// Transactions holds the full transaction list when available. May be
	// nil for a header-only Block; callers should consult TxCount, not
	// len(Transactions), to learn the transaction count in that case.
	Transactions []Transaction
}
