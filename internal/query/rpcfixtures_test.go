package query

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// fakeAddressResolver implements decode.AddressResolver deterministically,
// for the bare-P2PK vector this file's fixtures use — DecodeBlock/
// decodeOutput never call it for any type Core itself reports an address
// for (P2PKH/P2WPKH/P2TR/P2QPK/...), only for bare P2PK/multisig.
type fakeAddressResolver struct{}

func (fakeAddressResolver) ResolvePubKeyAddress(_ context.Context, pubKeyHex string) (string, error) {
	return "qResolved" + pubKeyHex[:8], nil
}

func qogeAmount(satoshis int64) json.Number {
	whole := satoshis / 100_000_000
	frac := satoshis % 100_000_000
	return json.Number(fmt.Sprintf("%d.%08d", whole, frac))
}

func strPtr(s string) *string   { return &s }
func i64Ptr(i int64) *int64     { return &i }
func intPtr(i int) *int         { return &i }
func u32Ptr(u uint32) *uint32   { return &u }
func f64Ptr(f float64) *float64 { return &f }

// p2pkScript builds a structurally valid 35-byte bare-P2PK scriptPubKey:
// <push 33><compressed pubkey><OP_CHECKSIG>.
func p2pkScript(label string) (script []byte, pubKey []byte) {
	pubKey = make([]byte, 33)
	pubKey[0] = 0x02
	copy(pubKey[1:], []byte(label + "................................")[:32])
	s := make([]byte, 0, 35)
	s = append(s, 0x21)
	s = append(s, pubKey...)
	s = append(s, 0xac)
	return s, pubKey
}

// witnessProgramScript builds <opVersion><push len(program)><program>, the
// wire shape of every SegWit-style witness output (P2WPKH/P2TR/P2QPK/
// unknown witness versions alike).
func witnessProgramScript(version int, program []byte) []byte {
	s := make([]byte, 0, 2+len(program))
	s = append(s, byte(0x50+version)) // OP_0=0x00 is a special case handled below
	if version == 0 {
		s[0] = 0x00
	}
	s = append(s, byte(len(program)))
	s = append(s, program...)
	return s
}

func nullDataScript(data []byte) []byte {
	s := []byte{0x6a} // OP_RETURN
	if len(data) > 0 {
		s = append(s, byte(len(data)))
		s = append(s, data...)
	}
	return s
}

func rawBlockFixture(hash string, height int64, prevHash string, txs ...rpc.RawTransaction) rpc.RawBlock {
	var prev *string
	if prevHash != "" {
		prev = &prevHash
	}
	nTx := len(txs)
	return rpc.RawBlock{
		Hash:              &hash,
		Height:            &height,
		PreviousBlockHash: prev,
		MerkleRoot:        strPtr(fakeHash("merkle:" + hash)),
		Time:              i64Ptr(1_700_000_000 + height),
		Bits:              strPtr("1d00ffff"),
		Difficulty:        f64Ptr(1.0),
		Nonce:             u32Ptr(uint32(height)),
		Size:              intPtr(300),
		Weight:            intPtr(1200),
		NTx:               &nTx,
		Tx:                txs,
	}
}

func rawCoinbaseTx(label string, coinbaseHex string, vout ...rpc.RawVout) rpc.RawTransaction {
	txid := fakeHash(label + "-tx")
	version := uint32(1)
	size, vsize, weight := 100, 100, 400
	lockTime := uint32(0)
	sequence := uint32(0xffffffff)
	return rpc.RawTransaction{
		TxID: &txid, Hash: &txid,
		Version: &version, Size: &size, VSize: &vsize, Weight: &weight, LockTime: &lockTime,
		Vin: []rpc.RawVin{
			{Coinbase: &coinbaseHex, Sequence: &sequence},
		},
		Vout: vout,
	}
}

func rawSpendVin(prevTxid string, prevVout uint32, scriptSigHex string, witness ...string) rpc.RawVin {
	sequence := uint32(0xffffffff)
	return rpc.RawVin{
		TxID: &prevTxid, Vout: &prevVout,
		ScriptSig:   &rpc.RawScriptSig{Hex: &scriptSigHex},
		Sequence:    &sequence,
		TxInWitness: witness,
	}
}

func rawSpendTx(label string, size, vsize, weight int, vin []rpc.RawVin, vout []rpc.RawVout) rpc.RawTransaction {
	txid := fakeHash(label + "-tx")
	wtxid := fakeHash(label + "-wtx")
	version := uint32(2)
	lockTime := uint32(0)
	return rpc.RawTransaction{
		TxID: &txid, Hash: &wtxid,
		Version: &version, Size: &size, VSize: &vsize, Weight: &weight, LockTime: &lockTime,
		Vin:  vin,
		Vout: vout,
	}
}

func rawVout(n uint32, valueSats int64, scriptHex []byte, coreType string, address *string) rpc.RawVout {
	h := hex.EncodeToString(scriptHex)
	return rpc.RawVout{
		Value: qogeAmount(valueSats),
		N:     &n,
		ScriptPubKey: &rpc.RawScriptPubKey{
			Hex: &h, Type: coreType, Address: address,
		},
	}
}
