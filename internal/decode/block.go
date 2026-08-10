package decode

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// DecodeBlock strictly decodes raw (Core's `getblock <hash> 2` response)
// into a chain.Block ready for Store.ApplyBlock. resolver is used to
// resolve bare P2PK/multisig participant addresses (see AddressResolver).
//
// DecodeBlock requires raw.NTx == len(raw.Tx) before returning a complete
// block — Store.ApplyBlock represents a fully indexed block, never a
// header-only/partial one (docs/ARCHITECTURE.md §16), so a truncated or
// otherwise incomplete RPC response must fail here rather than construct a
// chain.Block Store would later reject with less context.
//
// Genesis (raw.PreviousBlockHash absent, i.e. "") decodes to
// chain.Block.PreviousHash == "", matching chain.Block's own documented
// convention ("empty only for genesis").
func DecodeBlock(ctx context.Context, raw rpc.RawBlock, resolver AddressResolver) (chain.Block, error) {
	if err := validateHash(raw.Hash); err != nil {
		return chain.Block{}, fmt.Errorf("decode block: hash: %w", err)
	}
	if err := validateHash(raw.MerkleRoot); err != nil {
		return chain.Block{}, fmt.Errorf("decode block %s: merkleroot: %w", raw.Hash, err)
	}

	prevHash := ""
	if raw.PreviousBlockHash != "" {
		if err := validateHash(raw.PreviousBlockHash); err != nil {
			return chain.Block{}, fmt.Errorf("decode block %s: previousblockhash: %w", raw.Hash, err)
		}
		prevHash = raw.PreviousBlockHash
	}

	if raw.NTx != len(raw.Tx) {
		return chain.Block{}, fmt.Errorf("decode block %s: nTx=%d but %d transactions supplied", raw.Hash, raw.NTx, len(raw.Tx))
	}

	txs := make([]chain.Transaction, len(raw.Tx))
	for i, rawTx := range raw.Tx {
		txn, err := DecodeTransaction(ctx, rawTx, resolver)
		if err != nil {
			return chain.Block{}, fmt.Errorf("decode block %s tx %d: %w", raw.Hash, i, err)
		}
		txs[i] = txn
	}

	return chain.Block{
		Hash:         raw.Hash,
		Height:       raw.Height,
		PreviousHash: prevHash,
		MerkleRoot:   raw.MerkleRoot,
		Time:         raw.Time,
		Bits:         raw.Bits,
		Difficulty:   raw.Difficulty,
		Nonce:        raw.Nonce,
		Size:         raw.Size,
		Weight:       raw.Weight,
		TxCount:      raw.NTx,
		Transactions: txs,
	}, nil
}

// DecodeTransaction strictly decodes one raw transaction. txid/wtxid are
// taken from Core's own "txid"/"hash" fields exactly as reported — never
// derived, recomputed, or substituted (docs/ARCHITECTURE.md §3a; see also
// chain.Transaction's TxID/WTxID doc comments).
func DecodeTransaction(ctx context.Context, raw rpc.RawTransaction, resolver AddressResolver) (chain.Transaction, error) {
	if raw.TxID == "" {
		return chain.Transaction{}, fmt.Errorf("decode transaction: missing txid")
	}
	if err := validateHash(raw.TxID); err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction: txid: %w", err)
	}
	if raw.Hash == "" {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: missing hash (wtxid)", raw.TxID)
	}
	if err := validateHash(raw.Hash); err != nil {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: hash (wtxid): %w", raw.TxID, err)
	}

	if len(raw.Vin) == 0 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: no inputs", raw.TxID)
	}
	if len(raw.Vout) == 0 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: no outputs", raw.TxID)
	}

	inputs := make([]chain.Input, len(raw.Vin))
	coinbaseCount := 0
	for i, rawVin := range raw.Vin {
		in, err := decodeInput(uint32(i), rawVin)
		if err != nil {
			return chain.Transaction{}, fmt.Errorf("decode transaction %s vin %d: %w", raw.TxID, i, err)
		}
		if in.PreviousOut == nil {
			coinbaseCount++
		}
		inputs[i] = in
	}
	if coinbaseCount > 1 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: %d coinbase-shaped inputs, want at most 1", raw.TxID, coinbaseCount)
	}
	isCoinbase := coinbaseCount == 1
	if isCoinbase && len(inputs) != 1 {
		return chain.Transaction{}, fmt.Errorf("decode transaction %s: coinbase input mixed with %d other input(s)", raw.TxID, len(inputs)-1)
	}

	outputs := make([]chain.Output, len(raw.Vout))
	for i, rawVout := range raw.Vout {
		out, err := decodeOutput(ctx, uint32(i), rawVout, resolver)
		if err != nil {
			return chain.Transaction{}, fmt.Errorf("decode transaction %s vout %d: %w", raw.TxID, i, err)
		}
		outputs[i] = out
	}

	return chain.Transaction{
		TxID:       raw.TxID,
		WTxID:      raw.Hash,
		Version:    raw.Version,
		LockTime:   raw.LockTime,
		Size:       raw.Size,
		VSize:      raw.VSize,
		Weight:     raw.Weight,
		IsCoinbase: isCoinbase,
		Inputs:     inputs,
		Outputs:    outputs,
	}, nil
}

// decodeInput decodes one vin into a chain.Input at the given positional
// index. Core reports exactly one of two mutually exclusive shapes; see
// rpc.RawVin's doc comment.
func decodeInput(index uint32, raw rpc.RawVin) (chain.Input, error) {
	witness, err := decodeWitness(raw.TxInWitness)
	if err != nil {
		return chain.Input{}, err
	}

	if raw.Coinbase != nil {
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
			Sequence:    raw.Sequence,
			Witness:     witness,
		}, nil
	}

	if raw.TxID == "" {
		return chain.Input{}, fmt.Errorf("ordinary (non-coinbase) input missing txid")
	}
	if err := validateHash(raw.TxID); err != nil {
		return chain.Input{}, fmt.Errorf("ordinary input prevout txid: %w", err)
	}

	var scriptSig []byte
	if raw.ScriptSig != nil {
		b, err := hex.DecodeString(raw.ScriptSig.Hex)
		if err != nil {
			return chain.Input{}, fmt.Errorf("invalid scriptSig hex: %w", err)
		}
		scriptSig = b
	}

	return chain.Input{
		Index:       index,
		PreviousOut: &chain.OutPoint{TxID: raw.TxID, Index: raw.Vout},
		Coinbase:    nil,
		ScriptSig:   scriptSig,
		Sequence:    raw.Sequence,
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
func decodeOutput(ctx context.Context, index uint32, raw rpc.RawVout, resolver AddressResolver) (chain.Output, error) {
	if raw.N != index {
		return chain.Output{}, fmt.Errorf("vout n=%d does not match its position %d in the vout list", raw.N, index)
	}

	value, err := DecodeAmount(raw.Value)
	if err != nil {
		return chain.Output{}, fmt.Errorf("value: %w", err)
	}

	if raw.ScriptPubKey.Hex == "" {
		return chain.Output{}, fmt.Errorf("missing scriptPubKey")
	}
	rawScript, err := hex.DecodeString(raw.ScriptPubKey.Hex)
	if err != nil {
		return chain.Output{}, fmt.Errorf("invalid scriptPubKey hex: %w", err)
	}

	classified := script.Classify(rawScript)

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
		// Core deliberately omits an address for a bare multisig output;
		// resolve every participant pubkey through the same descriptor
		// method as P2PK (docs/ARCHITECTURE.md §10/§13.A). Output.Address
		// stays empty — Store rejects an output_addresses row for a
		// multisig output. Duplicate pubkeys are preserved positionally
		// here; Store applies identity-set deduplication at persistence.
		if len(classified.PubKeys) == 0 {
			return chain.Output{}, fmt.Errorf("multisig output classified with no pubkeys")
		}
		participants := make([]string, len(classified.PubKeys))
		for i, pk := range classified.PubKeys {
			addr, err := resolver.ResolvePubKeyAddress(ctx, hex.EncodeToString(pk))
			if err != nil {
				return chain.Output{}, fmt.Errorf("resolve multisig participant %d address: %w", i, err)
			}
			participants[i] = addr
		}
		out.ParticipantAddresses = participants

	case script.TypeP2PK:
		// Core deliberately omits an address for bare P2PK; resolve via
		// getdescriptorinfo("pkh(<pubkey>)") + deriveaddresses (the
		// Core-validated fallback — docs/ARCHITECTURE.md §7/§9). ScriptType
		// remains p2pk; this output is never relabeled as p2pkh.
		if len(classified.PubKeys) != 1 {
			return chain.Output{}, fmt.Errorf("p2pk output classified with %d pubkeys, want exactly 1", len(classified.PubKeys))
		}
		addr, err := resolver.ResolvePubKeyAddress(ctx, hex.EncodeToString(classified.PubKeys[0]))
		if err != nil {
			return chain.Output{}, fmt.Errorf("resolve p2pk address: %w", err)
		}
		out.Address = addr

	default:
		// Every other type — including a structural P2QPK output, which
		// Core itself only ever reports as generic "witness_unknown" — is
		// handled uniformly: Core's own reported address, exactly as
		// given, or empty if Core gave none. Never invented, never
		// derived (docs/ARCHITECTURE.md §8/§9/§11): TypeNullData/
		// TypeUnknown/TypeUnknownWitness outputs simply inherit whatever
		// Core did (or, in practice, did not) report.
		out.Address = raw.ScriptPubKey.Address
	}

	return out, nil
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
