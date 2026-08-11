package rpc

import (
	"context"
	"encoding/json"
	"fmt"
)

// MempoolFees mirrors the "fees" object Core's verbose getrawmempool
// response carries per entry. Only Base is used — the others (modified,
// ancestor, descendant) describe package-relative fee accounting this
// project doesn't need yet. Base is json.Number, never float64, for the
// same reason as rpc.RawVout.Value: internal/decode.DecodeAmount converts
// it to exact satoshis via text/integer arithmetic only.
type MempoolFees struct {
	Base json.Number `json:"base"`
}

// RawMempoolEntry mirrors one value of Core's `getrawmempool true`
// response map (keyed by txid). Every field Core always emits is a
// pointer, for the same "zero value is also legitimate data" reason as
// rpc.RawBlock — see that type's doc comment. Depends is a plain slice:
// encoding/json already leaves it nil only if the key itself is absent,
// and Core always emits "depends" (possibly as an empty array), so a nil
// check is a reliable presence check without a pointer wrapper.
type RawMempoolEntry struct {
	// VSize is Core mempool POLICY virtual size (CTxMemPoolEntry::GetTxSize,
	// i.e. GetVirtualTransactionSize(weight, sigOpCost) — see
	// policy/policy.cpp), not the plain BIP141 vsize verbose
	// getrawtransaction reports. It can legitimately exceed the
	// transaction's ordinary vsize for a high-sigop-cost transaction, so
	// internal/mempool must never compare it for equality against a
	// decoded chain.Transaction.VSize (see docs/ARCHITECTURE.md §22).
	// Kept as mempool policy metadata; not persisted in Phase 2F.1.
	VSize  *int   `json:"vsize"`
	Weight *int   `json:"weight"`
	Time   *int64 `json:"time"`

	// Height is the chain height at which this transaction entered the
	// mempool, as Core reports it. Not the same thing as a confirmation
	// height — a mempool transaction is by definition unconfirmed.
	Height *int64 `json:"height"`

	Fees *MempoolFees `json:"fees"`

	// Depends lists the txids of this transaction's unconfirmed (in
	// mempool) ancestors, as Core reports them.
	Depends []string `json:"depends"`

	// BIP125Replaceable mirrors Core's "bip125-replaceable" boolean. A
	// pointer since decoding it is optional metadata (item 9 of the
	// Phase 2F.1 spec: "RBF/replaceability metadata if Core supplies it
	// reliably") — never invented if Core's response shape changes.
	BIP125Replaceable *bool `json:"bip125-replaceable"`
}

// GetRawMempoolVerbose calls `getrawmempool true`, returning Core's full
// per-transaction verbose entries keyed by txid. This is the starting set
// for one mempool snapshot acquisition cycle — see internal/mempool.
func (c *Client) GetRawMempoolVerbose(ctx context.Context) (map[string]RawMempoolEntry, error) {
	var entries map[string]RawMempoolEntry
	if err := c.CallInto(ctx, &entries, "getrawmempool", true); err != nil {
		return nil, fmt.Errorf("getrawmempool true: %w", err)
	}
	return entries, nil
}
