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
type RawBlock struct {
	Hash   string `json:"hash"`
	Height int64  `json:"height"`

	// PreviousBlockHash is absent (empty string) for the genesis block —
	// Core's JSON simply omits the key rather than emitting an empty/null
	// value.
	PreviousBlockHash string `json:"previousblockhash"`

	MerkleRoot string  `json:"merkleroot"`
	Time       int64   `json:"time"`
	Bits       string  `json:"bits"`
	Difficulty float64 `json:"difficulty"` // display only; never used for consensus decisions
	Nonce      uint32  `json:"nonce"`
	Size       int     `json:"size"`
	Weight     int     `json:"weight"`

	// NTx is Core's own transaction count (verbosity=2's "nTx"),
	// independent of len(Tx) — the decoder requires these to agree before
	// treating the block as complete.
	NTx int              `json:"nTx"`
	Tx  []RawTransaction `json:"tx"`
}

// RawTransaction mirrors one transaction entry of `getblock <hash> 2`
// (equivalently, `getrawtransaction <txid> true`'s decoded shape).
type RawTransaction struct {
	// TxID is Core's "txid" — GetHash(), excluding witness data.
	TxID string `json:"txid"`

	// Hash is Core's "hash" field — GetWitnessHash(), the wtxid. This is
	// NOT the block hash; Core reuses the field name "hash" for the
	// witness transaction id in verbose transaction output. Never derive
	// or substitute this locally — see internal/decode.
	Hash string `json:"hash"`

	Version  uint32 `json:"version"`
	Size     int    `json:"size"`
	VSize    int    `json:"vsize"`
	Weight   int    `json:"weight"`
	LockTime uint32 `json:"locktime"`

	Vin  []RawVin  `json:"vin"`
	Vout []RawVout `json:"vout"`
}

// RawVin mirrors one input in Core's verbose transaction JSON. Core emits
// one of two mutually exclusive shapes: a coinbase input (only "coinbase"
// + "sequence" [+ "txinwitness"]) or an ordinary input ("txid"/"vout"/
// "scriptSig"/"sequence" [+ "txinwitness"]). Coinbase is a pointer so the
// decoder can distinguish "field present" (even if, hypothetically, an
// empty string) from "field absent" — Core's own discriminator between the
// two shapes is presence, not content.
type RawVin struct {
	Coinbase *string `json:"coinbase,omitempty"`

	TxID      string        `json:"txid,omitempty"`
	Vout      uint32        `json:"vout"`
	ScriptSig *RawScriptSig `json:"scriptSig,omitempty"`

	Sequence uint32 `json:"sequence"`

	// TxInWitness holds each witness stack item as a hex string, bottom of
	// stack first — Core's own txinwitness order, preserved exactly.
	TxInWitness []string `json:"txinwitness,omitempty"`
}

// RawScriptSig mirrors Core's nested {"asm": ..., "hex": ...} scriptSig
// object. Only the raw hex is needed; "asm" is not decoded.
type RawScriptSig struct {
	Hex string `json:"hex"`
}

// RawVout mirrors one output in Core's verbose transaction JSON. Value is
// json.Number, never float64/float32 — see internal/decode.DecodeAmount;
// decoding a monetary RPC field through float64 anywhere is exactly the
// class of bug this type exists to make impossible.
type RawVout struct {
	Value        json.Number     `json:"value"`
	N            uint32          `json:"n"`
	ScriptPubKey RawScriptPubKey `json:"scriptPubKey"`
}

// RawScriptPubKey mirrors Core's nested scriptPubKey object. Type is
// retained for diagnostics/tests only — it must never drive explorer
// script classification (internal/script.Classify does that, structurally,
// from Hex alone). Address is Core's own reported destination, copied
// as-is where the decoder trusts it (ordinary addressable script types);
// bare P2PK/multisig, which Core deliberately omits an address for, are
// resolved separately — see internal/decode.
type RawScriptPubKey struct {
	Hex     string `json:"hex"`
	Type    string `json:"type,omitempty"`
	Address string `json:"address,omitempty"`
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
