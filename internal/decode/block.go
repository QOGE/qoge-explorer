package decode

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// require returns the dereferenced value of a required pointer field, or
// an error naming field if p is nil (Core omitted the key entirely).
// Generic over every scalar field type this package's raw DTOs use
// (string, int, int64, uint32, float64) — see rpc.RawBlock's doc comment
// for why these must be pointers rather than plain zero-valued fields.
func require[T any](p *T, field string) (T, error) {
	if p == nil {
		var zero T
		return zero, fmt.Errorf("missing required field %q", field)
	}
	return *p, nil
}

// DecodeBlock strictly decodes raw (Core's `getblock <hash> 2` response)
// into a chain.Block ready for Store.ApplyBlock. resolver is used to
// resolve bare P2PK/multisig participant addresses (see AddressResolver);
// it may be nil if raw is known not to contain any P2PK/multisig outputs,
// but decodeOutput rejects — rather than panics — if a nil resolver is
// actually needed.
//
// DecodeBlock requires raw.NTx == len(raw.Tx) before returning a complete
// block — Store.ApplyBlock represents a fully indexed block, never a
// header-only/partial one (docs/ARCHITECTURE.md §16), so a truncated or
// otherwise incomplete RPC response must fail here rather than construct a
// chain.Block Store would later reject with less context.
//
// Genesis (height 0) requires raw.PreviousBlockHash to be absent (nil);
// every other height requires it present and a valid hash — Core's
// blockheaderToJSON only emits "previousblockhash" when a previous block
// actually exists, so decoding genesis to chain.Block.PreviousHash == ""
// is a positive assertion about height, never inferred from an empty or
// missing string alone.
func DecodeBlock(ctx context.Context, raw rpc.RawBlock, resolver AddressResolver) (chain.Block, error) {
	hashStr, err := require(raw.Hash, "hash")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block: %w", err)
	}
	if err := validateHash(hashStr); err != nil {
		return chain.Block{}, fmt.Errorf("decode block: hash: %w", err)
	}

	height, err := require(raw.Height, "height")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}
	if height < 0 {
		return chain.Block{}, fmt.Errorf("decode block %s: height %d is negative", hashStr, height)
	}

	merkleRootStr, err := require(raw.MerkleRoot, "merkleroot")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}
	if err := validateHash(merkleRootStr); err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: merkleroot: %w", hashStr, err)
	}

	prevHash, err := decodeGenesisRelation(hashStr, height, raw.PreviousBlockHash)
	if err != nil {
		return chain.Block{}, err
	}

	timeVal, err := require(raw.Time, "time")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}
	bits, err := require(raw.Bits, "bits")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}
	if err := validateBits(bits); err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: bits: %w", hashStr, err)
	}
	difficulty, err := require(raw.Difficulty, "difficulty")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}
	nonce, err := require(raw.Nonce, "nonce")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}
	size, err := require(raw.Size, "size")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}
	if size < 0 {
		return chain.Block{}, fmt.Errorf("decode block %s: size %d is negative", hashStr, size)
	}
	weight, err := require(raw.Weight, "weight")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}
	if weight < 0 {
		return chain.Block{}, fmt.Errorf("decode block %s: weight %d is negative", hashStr, weight)
	}
	nTx, err := require(raw.NTx, "nTx")
	if err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: %w", hashStr, err)
	}

	if raw.Tx == nil {
		return chain.Block{}, fmt.Errorf("decode block %s: missing required field %q", hashStr, "tx")
	}
	if nTx != len(raw.Tx) {
		return chain.Block{}, fmt.Errorf("decode block %s: nTx=%d but %d transactions supplied", hashStr, nTx, len(raw.Tx))
	}

	txs := make([]chain.Transaction, len(raw.Tx))
	for i, rawTx := range raw.Tx {
		txn, err := DecodeTransaction(ctx, rawTx, resolver)
		if err != nil {
			return chain.Block{}, fmt.Errorf("decode block %s tx %d: %w", hashStr, i, err)
		}
		txs[i] = txn
	}

	return chain.Block{
		Hash:         hashStr,
		Height:       height,
		PreviousHash: prevHash,
		MerkleRoot:   merkleRootStr,
		Time:         timeVal,
		Bits:         bits,
		Difficulty:   difficulty,
		Nonce:        nonce,
		Size:         size,
		Weight:       weight,
		TxCount:      nTx,
		Transactions: txs,
	}, nil
}

// decodeGenesisRelation enforces height<->previousblockhash presence
// exactly: height 0 requires prevHashField absent; height > 0 requires it
// present and a valid hash. Presence, not string emptiness, is what's
// checked — Core never emits "previousblockhash":"" for any block.
func decodeGenesisRelation(blockHash string, height int64, prevHashField *string) (string, error) {
	switch {
	case height == 0:
		if prevHashField != nil {
			return "", fmt.Errorf("decode block %s: height 0 (genesis) but previousblockhash is present (%q)", blockHash, *prevHashField)
		}
		return "", nil
	default: // height > 0 (negative height already rejected by the caller)
		if prevHashField == nil {
			return "", fmt.Errorf("decode block %s: height %d but previousblockhash is missing", blockHash, height)
		}
		if err := validateHash(*prevHashField); err != nil {
			return "", fmt.Errorf("decode block %s: previousblockhash: %w", blockHash, err)
		}
		return *prevHashField, nil
	}
}

// DecodeTransaction strictly decodes one raw transaction. txid/wtxid are
// taken from Core's own "txid"/"hash" fields exactly as reported — never
// derived, recomputed, or substituted (docs/ARCHITECTURE.md §3a; see also
// chain.Transaction's TxID/WTxID doc comments).
func DecodeTransaction(ctx context.Context, raw rpc.RawTransaction, resolver AddressResolver) (chain.Transaction, error) {
	txid, err := require(raw.TxID, "txid")
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction: %w", err)
	}
	if err := validateHash(txid); err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction: txid: %w", err)
	}
	wtxid, err := require(raw.Hash, "hash")
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: %w", txid, err)
	}
	if err := validateHash(wtxid); err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: hash (wtxid): %w", txid, err)
	}

	version, err := require(raw.Version, "version")
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: %w", txid, err)
	}
	size, err := require(raw.Size, "size")
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: %w", txid, err)
	}
	if size < 0 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: size %d is negative", txid, size)
	}
	vsize, err := require(raw.VSize, "vsize")
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: %w", txid, err)
	}
	if vsize < 0 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: vsize %d is negative", txid, vsize)
	}
	weight, err := require(raw.Weight, "weight")
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: %w", txid, err)
	}
	if weight < 0 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: weight %d is negative", txid, weight)
	}
	lockTime, err := require(raw.LockTime, "locktime")
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: %w", txid, err)
	}

	if len(raw.Vin) == 0 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: no inputs", txid)
	}
	if len(raw.Vout) == 0 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: no outputs", txid)
	}

	inputs := make([]chain.Input, len(raw.Vin))
	coinbaseCount := 0
	for i, rawVin := range raw.Vin {
		in, err := decodeInput(uint32(i), rawVin)
		if err != nil {
			return chain.Transaction{}, fmt.Errorf("decode transaction %s vin %d: %w", txid, i, err)
		}
		if in.PreviousOut == nil {
			coinbaseCount++
		}
		inputs[i] = in
	}
	if coinbaseCount > 1 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: %d coinbase-shaped inputs, want at most 1", txid, coinbaseCount)
	}
	isCoinbase := coinbaseCount == 1
	if isCoinbase && len(inputs) != 1 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: coinbase input mixed with %d other input(s)", txid, len(inputs)-1)
	}

	outputs := make([]chain.Output, len(raw.Vout))
	for i, rawVout := range raw.Vout {
		out, err := decodeOutput(ctx, uint32(i), rawVout, resolver)
		if err != nil {
			return chain.Transaction{}, fmt.Errorf("decode transaction %s vout %d: %w", txid, i, err)
		}
		outputs[i] = out
	}

	return chain.Transaction{
		TxID:       txid,
		WTxID:      wtxid,
		Version:    version,
		LockTime:   lockTime,
		Size:       size,
		VSize:      vsize,
		Weight:     weight,
		IsCoinbase: isCoinbase,
		Inputs:     inputs,
		Outputs:    outputs,
	}, nil
}

// decodeInput decodes one vin into a chain.Input at the given positional
// index. Core reports exactly one of two mutually exclusive wire shapes
// (rpc.RawVin's doc comment); decodeInput requires the ENTIRE shape to
// match one side exactly — a raw vin carrying fields from both shapes
// (e.g. "coinbase" and "txid" together) is contradictory data, rejected
// rather than resolved by silently preferring one field over the other.
func decodeInput(index uint32, raw rpc.RawVin) (chain.Input, error) {
	witness, err := decodeWitness(raw.TxInWitness)
	if err != nil {
		return chain.Input{}, err
	}

	if raw.Coinbase != nil {
		if raw.TxID != nil {
			return chain.Input{}, fmt.Errorf("coinbase input also has txid (contradictory wire shape)")
		}
		if raw.Vout != nil {
			return chain.Input{}, fmt.Errorf("coinbase input also has vout (contradictory wire shape)")
		}
		if raw.ScriptSig != nil {
			return chain.Input{}, fmt.Errorf("coinbase input also has scriptSig (contradictory wire shape)")
		}
		sequence, err := require(raw.Sequence, "sequence")
		if err != nil {
			return chain.Input{}, err
		}
		coinbaseBytes, err := hex.DecodeString(*raw.Coinbase)
		if err != nil {
			return chain.Input{}, fmt.Errorf("invalid coinbase hex: %w", err)
		}
		if len(coinbaseBytes) == 0 {
			return chain.Input{}, fmt.Errorf("coinbase script is empty")
		}
		return chain.Input{
			Index:       index,
			PreviousOut: nil,
			Coinbase:    coinbaseBytes,
			ScriptSig:   nil,
			Sequence:    sequence,
			Witness:     witness,
		}, nil
	}

	txid, err := require(raw.TxID, "txid")
	if err != nil {
		return chain.Input{}, fmt.Errorf("ordinary (non-coinbase) input: %w", err)
	}
	if err := validateHash(txid); err != nil {
		return chain.Input{}, fmt.Errorf("ordinary input prevout txid: %w", err)
	}
	prevVout, err := require(raw.Vout, "vout")
	if err != nil {
		return chain.Input{}, fmt.Errorf("ordinary input: %w", err)
	}
	if raw.ScriptSig == nil {
		return chain.Input{}, fmt.Errorf("ordinary input missing scriptSig")
	}
	scriptSigHex, err := require(raw.ScriptSig.Hex, "scriptSig.hex")
	if err != nil {
		return chain.Input{}, fmt.Errorf("ordinary input: %w", err)
	}
	scriptSig, err := hex.DecodeString(scriptSigHex)
	if err != nil {
		return chain.Input{}, fmt.Errorf("invalid scriptSig hex: %w", err)
	}
	sequence, err := require(raw.Sequence, "sequence")
	if err != nil {
		return chain.Input{}, fmt.Errorf("ordinary input: %w", err)
	}

	return chain.Input{
		Index:       index,
		PreviousOut: &chain.OutPoint{TxID: txid, Index: prevVout},
		Coinbase:    nil,
		ScriptSig:   scriptSig,
		Sequence:    sequence,
		Witness:     witness,
	}, nil
}

// decodeWitness hex-decodes every txinwitness item independently, byte for
// byte, preserving item ordering, zero-length items, and any length up to
// and including a full 17,088-byte P2QPK signature — never concatenated,
// never truncated (docs/ARCHITECTURE.md §8).
func decodeWitness(hexItems []string) (chain.WitnessStack, error) {
	if len(hexItems) == 0 {
		return nil, nil
	}
	stack := make(chain.WitnessStack, len(hexItems))
	for i, h := range hexItems {
		b, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("invalid witness item %d hex: %w", i, err)
		}
		stack[i] = b
	}
	return stack, nil
}

// decodeOutput decodes one vout, classifying its scriptPubKey structurally
// via script.Classify — Core's own reported scriptPubKey.type is retained
// on the raw RPC DTO for diagnostics only and never consulted here (see
// rpc.RawScriptPubKey's doc comment; this is what correctly turns a Core
// "witness_unknown" v2/32 output into script.TypeP2QPK rather than trusting
// Core's generic label).
//
// Once the structural type is known, decodeOutput enforces the address
// SHAPE Core's own ScriptToUniv actually emits for that type (source:
// Core's ScriptToUniv adds "address" iff ExtractDestination succeeds AND
// the type isn't PUBKEY):
//
//   - P2PK: ExtractDestination succeeds but ScriptToUniv explicitly
//     suppresses "address" for PUBKEY — an address present here is
//     contradictory RPC data. Output.Address is instead resolved via
//     resolver.
//   - Multisig (bare CHECKMULTISIG): not a Core "address" type at all;
//     Output.Address stays empty, and every participant pubkey is
//     resolved via resolver into ParticipantAddresses.
//   - P2PKH/P2SH/P2WPKH/P2WSH/P2TR/P2QPK/TypeUnknownWitness:
//     ExtractDestination succeeds and ScriptToUniv does not suppress the
//     address for these — a MISSING address here is contradictory RPC
//     data (silently accepting it would persist the output with no
//     output_addresses row and permanently undercount that address's
//     balance). Copied as-is, never derived.
//   - TypeNullData/TypeUnknown: ExtractDestination fails; Core reports no
//     address. A present, non-empty address here is contradictory RPC
//     data — never copied, always rejected rather than silently trusted.
func decodeOutput(ctx context.Context, index uint32, raw rpc.RawVout, resolver AddressResolver) (chain.Output, error) {
	n, err := require(raw.N, "n")
	if err != nil {
		return chain.Output{}, err
	}
	if n != index {
		return chain.Output{}, fmt.Errorf("vout n=%d does not match its position %d in the vout list", n, index)
	}

	value, err := DecodeAmount(raw.Value)
	if err != nil {
		return chain.Output{}, fmt.Errorf("value: %w", err)
	}

	if raw.ScriptPubKey == nil {
		return chain.Output{}, fmt.Errorf("missing scriptPubKey object")
	}
	scriptHex, err := require(raw.ScriptPubKey.Hex, "scriptPubKey.hex")
	if err != nil {
		return chain.Output{}, err
	}
	// scriptHex == "" here is legitimate data (a genuinely empty CScript —
	// Core's ScriptToUniv always emits "hex": HexStr(script), including
	// for an empty script) — hex.DecodeString("") correctly yields a
	// zero-length, non-nil []byte, not an error.
	rawScript, err := hex.DecodeString(scriptHex)
	if err != nil {
		return chain.Output{}, fmt.Errorf("invalid scriptPubKey hex: %w", err)
	}

	classified := script.Classify(rawScript)
	addr := raw.ScriptPubKey.Address // nil iff Core omitted "address"

	out := chain.Output{
		Index:          index,
		Value:          value,
		ScriptPubKey:   rawScript,
		ScriptType:     classified.Type,
		WitnessVersion: classified.WitnessVersion,
		WitnessProgram: classified.WitnessProgram,
		PubKeys:        classified.PubKeys,
	}

	switch classified.Type {
	case script.TypeMultisig:
		if addr != nil {
			return chain.Output{}, fmt.Errorf("multisig output unexpectedly has a Core-reported address %q", *addr)
		}
		if len(classified.PubKeys) == 0 {
			return chain.Output{}, fmt.Errorf("multisig output classified with no pubkeys")
		}
		participants := make([]string, len(classified.PubKeys))
		for i, pk := range classified.PubKeys {
			resolved, err := resolvePubKey(ctx, resolver, pk)
			if err != nil {
				return chain.Output{}, fmt.Errorf("resolve multisig participant %d address: %w", i, err)
			}
			participants[i] = resolved
		}
		out.ParticipantAddresses = participants

	case script.TypeP2PK:
		if addr != nil {
			return chain.Output{}, fmt.Errorf("p2pk output unexpectedly has a Core-reported address %q (Core's ScriptToUniv suppresses \"address\" for PUBKEY)", *addr)
		}
		if len(classified.PubKeys) != 1 {
			return chain.Output{}, fmt.Errorf("p2pk output classified with %d pubkeys, want exactly 1", len(classified.PubKeys))
		}
		resolved, err := resolvePubKey(ctx, resolver, classified.PubKeys[0])
		if err != nil {
			return chain.Output{}, fmt.Errorf("resolve p2pk address: %w", err)
		}
		out.Address = resolved

	case script.TypeNullData, script.TypeUnknown:
		if addr != nil && *addr != "" {
			return chain.Output{}, fmt.Errorf("%s output unexpectedly has a Core-reported address %q (Core's ExtractDestination fails for this type)", classified.Type, *addr)
		}

	default:
		// P2PKH, P2SH, P2WPKH, P2WSH, P2TR, P2QPK, TypeUnknownWitness:
		// Core's ExtractDestination succeeds and ScriptToUniv always
		// includes a non-empty address for these.
		if addr == nil || *addr == "" {
			return chain.Output{}, fmt.Errorf("%s output missing Core-reported address (Core's ExtractDestination succeeds for this type)", classified.Type)
		}
		out.Address = *addr
	}

	return out, nil
}

// resolvePubKey resolves pubKey through resolver, rejecting rather than
// panicking on a nil resolver, and rejecting an ("", nil) result even from
// a custom/fake AddressResolver implementation that doesn't itself treat
// that as an error.
func resolvePubKey(ctx context.Context, resolver AddressResolver, pubKey []byte) (string, error) {
	if resolver == nil {
		return "", fmt.Errorf("no AddressResolver configured (nil)")
	}
	pubKeyHex := hex.EncodeToString(pubKey)
	addr, err := resolver.ResolvePubKeyAddress(ctx, pubKeyHex)
	if err != nil {
		return "", err
	}
	if addr == "" {
		return "", fmt.Errorf("resolver returned an empty address for pubkey %s", pubKeyHex)
	}
	return addr, nil
}

// validateHash requires s to be exactly 64 lowercase hex characters — the
// shape every block/transaction hash in this codebase's schema and Core's
// own RPC output share (migrations/0001_initial.up.sql's `~ '^[0-9a-f]
// {64}$'` CHECK constraints). Uppercase is deliberately NOT normalized:
// this codebase has no confirmed case of Core legitimately emitting
// uppercase hex for these fields, so silently accepting it would risk
// masking genuinely malformed data rather than merely tolerating a
// harmless case variant.
func validateHash(s string) error {
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

// validateBits requires s to be exactly 8 lowercase hex characters — the
// shape of Core's compact-target "bits" field.
func validateBits(s string) error {
	if len(s) != 8 {
		return fmt.Errorf("invalid bits %q: want 8 hex characters, got %d", s, len(s))
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("invalid bits %q: not lowercase hex", s)
		}
	}
	return nil
}
