package rpc

import (
	"context"
	"encoding/json"
	"fmt"
)

// RawBlock mirrors the fields Qogecoin Core's `getblock <hash> 2` response
// carries that the decoder (internal/decode) needs. It is a raw wire-shape
// DTO — deliberately NOT chain.Block — so that a strict decoder can convert
// it into the canonical model with explicit validation, rather than the
// RPC's own (untrusted) shape leaking directly into internal/chain/store.
// Fields Core returns that the explorer doesn't use are simply not decoded.
//
// Every field Core always emits is a pointer, not a plain value: many of
// these fields (Height, Nonce, Size, Weight, Time, NTx) have a Go zero
// value (0, "") that is also a perfectly legitimate real value (height 0
// is genesis; a real nonce, given enough luck, could be 0). A plain
// numeric/string field therefore cannot distinguish "Core omitted this
// key" from "Core reported this exact value" — only a nil-vs-non-nil
// pointer can. internal/decode's DecodeBlock treats a nil required field
// as a decode error, never silently substituting a zero value.
type RawBlock struct {
	Hash   *string `json:"hash"`
	Height *int64  `json:"height"`

	// PreviousBlockHash is present for every block except genesis — Core's
	// blockheaderToJSON only emits this key when pprev exists. nil means
	// "key absent" (expected only at height 0); internal/decode enforces
	// the height<->presence relationship explicitly, never inferring
	// genesis from an empty string.
	PreviousBlockHash *string `json:"previousblockhash"`

	MerkleRoot *string  `json:"merkleroot"`
	Time       *int64   `json:"time"`
	Bits       *string  `json:"bits"`
	Difficulty *float64 `json:"difficulty"` // display only; never used for consensus decisions
	Nonce      *uint32  `json:"nonce"`
	Size       *int     `json:"size"`
	Weight     *int     `json:"weight"`

	// NTx is Core's own transaction count (verbosity=2's "nTx"),
	// independent of len(Tx) — the decoder requires these to agree before
	// treating the block as complete.
	NTx *int `json:"nTx"`

	// Tx is a plain (non-pointer) slice: encoding/json already leaves a
	// slice field nil when its JSON key is entirely absent, and sets it to
	// a non-nil (possibly zero-length) slice when the key is present —
	// e.g. "tx":[] — so a nil check on Tx itself is a reliable presence
	// check without needing a pointer wrapper.
	Tx []RawTransaction `json:"tx"`
}

// RawTransaction mirrors one transaction entry of `getblock <hash> 2`
// (equivalently, `getrawtransaction <txid> true`'s decoded shape). See
// RawBlock's doc comment for why required scalar fields are pointers.
type RawTransaction struct {
	// TxID is Core's "txid" — GetHash(), excluding witness data.
	TxID *string `json:"txid"`

	// Hash is Core's "hash" field — GetWitnessHash(), the wtxid. This is
	// NOT the block hash; Core reuses the field name "hash" for the
	// witness transaction id in verbose transaction output. Never derive
	// or substitute this locally — see internal/decode.
	Hash *string `json:"hash"`

	Version  *uint32 `json:"version"`
	Size     *int    `json:"size"`
	VSize    *int    `json:"vsize"`
	Weight   *int    `json:"weight"`
	LockTime *uint32 `json:"locktime"`

	// Vin/Vout are plain slices — see Tx's doc comment above for why a nil
	// check on the slice itself is already a reliable presence check.
	Vin  []RawVin  `json:"vin"`
	Vout []RawVout `json:"vout"`
}

// RawVin mirrors one input in Core's verbose transaction JSON. Core emits
// one of two mutually exclusive shapes: a coinbase input (only "coinbase"
// + "sequence" [+ "txinwitness"]) or an ordinary input ("txid"/"vout"/
// "scriptSig"/"sequence" [+ "txinwitness"]). Every field is a pointer so
// the decoder can enforce that exclusivity structurally — a raw vin
// carrying BOTH "coinbase" and "txid", say, is contradictory Core wire
// data that must be rejected, not silently resolved by picking one and
// discarding the other. Coinbase in particular distinguishes "field
// present" (even if, hypothetically, an empty string) from "field
// absent" — Core's own discriminator between the two shapes is presence,
// not content.
type RawVin struct {
	Coinbase *string `json:"coinbase,omitempty"`

	TxID      *string       `json:"txid,omitempty"`
	Vout      *uint32       `json:"vout,omitempty"`
	ScriptSig *RawScriptSig `json:"scriptSig,omitempty"`

	Sequence *uint32 `json:"sequence"`

	// TxInWitness holds each witness stack item as a hex string, bottom of
	// stack first — Core's own txinwitness order, preserved exactly. A
	// plain slice: absent (no witness data) and present-but-empty are
	// treated identically (both mean "no witness items"), so there is no
	// presence-tracking need here beyond the nil-vs-non-nil-empty
	// distinction encoding/json already gives a slice for free.
	TxInWitness []string `json:"txinwitness,omitempty"`
}

// RawScriptSig mirrors Core's nested {"asm": ..., "hex": ...} scriptSig
// object. Only the raw hex is needed; "asm" is not decoded. Hex is a
// pointer: Core's ScriptToUniv always emits "hex" (even as "" for an
// empty/pure-witness scriptSig), so a present-but-empty Hex (non-nil,
// points at "") is valid data, while a nil Hex means the "hex" member
// itself was missing from the scriptSig object — a malformed response.
type RawScriptSig struct {
	Hex *string `json:"hex"`
}

// RawVout mirrors one output in Core's verbose transaction JSON. Value is
// json.Number, never float64/float32 — see internal/decode.DecodeAmount;
// decoding a monetary RPC field through float64 anywhere is exactly the
// class of bug this type exists to make impossible. An entirely-missing
// "value" key decodes to json.Number(""), which DecodeAmount already
// rejects as a malformed (empty) number — no separate presence tracking
// needed for this field.
type RawVout struct {
	Value        json.Number      `json:"value"`
	N            *uint32          `json:"n"`
	ScriptPubKey *RawScriptPubKey `json:"scriptPubKey"`
}

// RawScriptPubKey mirrors Core's nested scriptPubKey object. Type is
// retained for diagnostics/tests only — it must never drive explorer
// script classification (internal/script.Classify does that, structurally,
// from Hex alone). Hex is a pointer for the same reason as
// RawScriptSig.Hex: Core's ScriptToUniv always emits "hex", even as "" for
// a genuinely empty CScript, so nil (member missing) and non-nil-empty
// (member present, empty script) must be distinguished, not conflated.
// Address is Core's own reported destination — also a pointer, since its
// PRESENCE (not just its content) is meaningful: Core's ScriptToUniv adds
// "address" if and only if ExtractDestination succeeds, which is a
// function of the script's structural type (see internal/decode's address
// presence enforcement). Bare P2PK/multisig, which Core deliberately
// omits an address for, are resolved separately — see internal/decode.
type RawScriptPubKey struct {
	Hex     *string `json:"hex"`
	Type    string  `json:"type,omitempty"`
	Address *string `json:"address,omitempty"`
}

// GetBlockHash calls getblockhash, returning the canonical-at-call-time
// block hash for height.
func (c *Client) GetBlockHash(ctx context.Context, height int64) (string, error) {
	var hash string
	if err := c.CallInto(ctx, &hash, "getblockhash", height); err != nil {
		return "", fmt.Errorf("getblockhash %d: %w", height, err)
	}
	return hash, nil
}

// GetBlockVerbose2 calls `getblock <hash> 2` — full block header plus every
// transaction, fully decoded (not just txids).
func (c *Client) GetBlockVerbose2(ctx context.Context, hash string) (RawBlock, error) {
	var block RawBlock
	if err := c.CallInto(ctx, &block, "getblock", hash, 2); err != nil {
		return RawBlock{}, fmt.Errorf("getblock %s 2: %w", hash, err)
	}
	return block, nil
}

// GetRawTransactionVerbose calls `getrawtransaction <txid> true`. Useful
// for focused single-transaction vector tests; Core's actual response
// carries a few more fields (confirmations, blockhash, blocktime, ...)
// that RawTransaction does not declare and that are simply ignored on
// decode.
func (c *Client) GetRawTransactionVerbose(ctx context.Context, txid string) (RawTransaction, error) {
	var tx RawTransaction
	if err := c.CallInto(ctx, &tx, "getrawtransaction", txid, true); err != nil {
		return RawTransaction{}, fmt.Errorf("getrawtransaction %s true: %w", txid, err)
	}
	return tx, nil
}

// DescriptorInfo mirrors Core's getdescriptorinfo response — only the
// canonical, checksum-appended Descriptor is needed.
type DescriptorInfo struct {
	Descriptor string `json:"descriptor"`
}

// GetDescriptorInfo calls getdescriptorinfo, returning the canonical
// (checksum-appended) form of descriptor.
func (c *Client) GetDescriptorInfo(ctx context.Context, descriptor string) (DescriptorInfo, error) {
	var info DescriptorInfo
	if err := c.CallInto(ctx, &info, "getdescriptorinfo", descriptor); err != nil {
		return DescriptorInfo{}, fmt.Errorf("getdescriptorinfo: %w", err)
	}
	return info, nil
}

// DeriveAddresses calls deriveaddresses on a canonical descriptor (as
// returned by GetDescriptorInfo), returning the address(es) it derives to.
// For the non-ranged descriptors this package builds (e.g. "pkh(<pubkey>)"),
// Core returns exactly one address.
func (c *Client) DeriveAddresses(ctx context.Context, canonicalDescriptor string) ([]string, error) {
	var addrs []string
	if err := c.CallInto(ctx, &addrs, "deriveaddresses", canonicalDescriptor); err != nil {
		return nil, fmt.Errorf("deriveaddresses: %w", err)
	}
	return addrs, nil
}
