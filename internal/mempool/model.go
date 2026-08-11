package mempool

import (
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
)

// CandidateTransaction is one fully, strictly decoded mempool transaction
// (via decode.DecodeTransaction against Core's getrawtransaction verbose
// response — see sync.go) plus the current-snapshot metadata Core's
// getrawmempool verbose entry reports for it. TxID/WTxID/Version/
// LockTime/Size/VSize/Weight/Inputs/Outputs all come from
// chain.Transaction exactly as decoded — never re-derived here.
type CandidateTransaction struct {
	chain.Transaction

	// FeeSatoshis is Core's mempool entry "fees.base", converted to exact
	// satoshis via decode.DecodeAmount — never through float64.
	FeeSatoshis int64

	// EntryTime is Core's mempool entry "time" (unix seconds): when this
	// transaction entered Core's mempool.
	EntryTime int64

	// EntryHeight is Core's mempool entry "height", if reported: the
	// chain height current when the transaction entered the mempool. Not
	// a confirmation height — a mempool transaction is by definition
	// unconfirmed.
	EntryHeight *int64

	// Replaceable mirrors Core's "bip125-replaceable", if reported. Nil
	// means Core's response didn't include reliable metadata for it.
	Replaceable *bool

	// Depends lists the txids of this transaction's unconfirmed (in
	// mempool) ancestors, per Core's "depends". Every entry must refer to
	// another transaction in the same Candidate — see validate.
	Depends []string
}

// Candidate is one complete, not-yet-persisted mempool snapshot, anchored
// to a specific Core active-chain tip observed during acquisition (see
// sync.go and docs/ARCHITECTURE.md §22).
type Candidate struct {
	CoreTipHeight int64
	CoreTipHash   string
	Transactions  []CandidateTransaction
}

// validate enforces the structural invariants Store.ReplaceSnapshot
// requires before ever touching PostgreSQL. This is defense in depth
// alongside whatever sync.go's acquisition path already checked, and is
// the sole structural gate for any direct/test caller of ReplaceSnapshot
// (see store_test.go's atomic-replacement tests, spec item 37 H/I/J).
func (c Candidate) validate() error {
	if err := validateHexHash(c.CoreTipHash); err != nil {
		return fmt.Errorf("mempool: candidate core tip hash: %w", err)
	}
	if c.CoreTipHeight < 0 {
		return fmt.Errorf("mempool: candidate core tip height %d is negative", c.CoreTipHeight)
	}

	txids := make(map[string]bool, len(c.Transactions))
	wtxids := make(map[string]bool, len(c.Transactions))
	for _, txn := range c.Transactions {
		// A mempool transaction must never be coinbase-shaped — checked
		// two ways: the decoder's own IsCoinbase classification, and
		// (independently) that every input actually has a previous
		// output. Either alone would catch a real coinbase transaction;
		// checking both makes this robust against a hand-built test
		// fixture that sets one without the other.
		if txn.IsCoinbase {
			return fmt.Errorf("mempool: candidate transaction %s is coinbase-shaped; a mempool transaction must never be coinbase", txn.TxID)
		}
		for _, in := range txn.Inputs {
			if in.PreviousOut == nil {
				return fmt.Errorf("mempool: candidate transaction %s has a coinbase-shaped input (no previous output); a mempool transaction must never be coinbase", txn.TxID)
			}
		}

		if txids[txn.TxID] {
			return fmt.Errorf("mempool: candidate has duplicate txid %s", txn.TxID)
		}
		txids[txn.TxID] = true

		if wtxids[txn.WTxID] {
			return fmt.Errorf("mempool: candidate has duplicate wtxid %s", txn.WTxID)
		}
		wtxids[txn.WTxID] = true
	}

	// Dependencies must refer to another transaction in THIS candidate
	// snapshot — reject dangling mempool-only references rather than
	// storing them (spec item 33).
	for _, txn := range c.Transactions {
		for _, dep := range txn.Depends {
			if dep == txn.TxID {
				return fmt.Errorf("mempool: candidate transaction %s depends on itself", txn.TxID)
			}
			if !txids[dep] {
				return fmt.Errorf("mempool: candidate transaction %s depends on %s, which is not part of this snapshot", txn.TxID, dep)
			}
		}
	}

	return nil
}

// validateHexHash requires s to be exactly 64 lowercase hex characters —
// the same shape internal/decode's validateHash enforces, duplicated
// locally (a few lines) rather than exporting an internal decode helper
// purely for this one call site.
func validateHexHash(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("invalid hash %q: want 64 hex characters, got %d", s, len(s))
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("invalid hash %q: not lowercase hex", s)
		}
	}
	return nil
}
