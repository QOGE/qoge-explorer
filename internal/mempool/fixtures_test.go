package mempool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// fakeHash deterministically derives a 64-char lowercase hex hash from a
// human-readable label — the same pattern internal/query's fixtures_test.go
// uses, reproduced here rather than imported (test-only files aren't
// importable across packages).
func fakeHash(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func strPtr(s string) *string { return &s }
func i64Ptr(i int64) *int64   { return &i }
func intPtr(i int) *int       { return &i }
func u32Ptr(u uint32) *uint32 { return &u }
func boolPtr(b bool) *bool    { return &b }

func qogeAmount(satoshis int64) json.Number {
	whole := satoshis / 100_000_000
	frac := satoshis % 100_000_000
	return json.Number(fmt.Sprintf("%d.%08d", whole, frac))
}

func jsonNum(s string) json.Number { return json.Number(s) }

// p2pkhScript builds a structurally valid 25-byte P2PKH scriptPubKey.
func p2pkhScript(label string) []byte {
	sum := sha256.Sum256([]byte("script:" + label))
	s := make([]byte, 0, 25)
	s = append(s, 0x76, 0xa9, 0x14)
	s = append(s, sum[:20]...)
	s = append(s, 0x88, 0xac)
	return s
}

// witnessProgramScript builds <opVersion><push len(program)><program>, the
// wire shape of every SegWit-style witness output (P2WPKH/P2TR/P2QPK/
// unknown witness versions alike).
func witnessProgramScript(version int, program []byte) []byte {
	s := make([]byte, 0, 2+len(program))
	if version == 0 {
		s = append(s, 0x00)
	} else {
		s = append(s, byte(0x50+version))
	}
	s = append(s, byte(len(program)))
	s = append(s, program...)
	return s
}

// nullDataScript builds OP_RETURN <push data>, using a direct push opcode
// for data up to 75 bytes and OP_PUSHDATA1 (0x4c <len> <data>) beyond
// that — script.matchNullData's isPushOnly helper only recognizes these
// exact push encodings (Core's CScript::GetOp shape), so a raw
// byte(len(data)) length prefix above 75 is not a valid push at all and
// would misclassify the script as TypeUnknown rather than TypeNullData.
func nullDataScript(data []byte) []byte {
	s := []byte{0x6a} // OP_RETURN
	if len(data) == 0 {
		return s
	}
	if len(data) <= 75 {
		s = append(s, byte(len(data)))
	} else if len(data) <= 255 {
		s = append(s, 0x4c, byte(len(data))) // OP_PUSHDATA1
	} else {
		panic("nullDataScript: fixture data too large for this helper")
	}
	s = append(s, data...)
	return s
}

// compressedPubKey builds a structurally valid 33-byte compressed pubkey
// push value (0x02/0x03 prefix + 32 deterministic bytes).
func compressedPubKey(label string) []byte {
	sum := sha256.Sum256([]byte("pubkey:" + label))
	pk := make([]byte, 33)
	pk[0] = 0x02
	copy(pk[1:], sum[:])
	return pk
}

// multisigScript builds a bare 1-of-N CHECKMULTISIG scriptPubKey:
// OP_1 <pubkey1>...<pubkeyN> OP_N OP_CHECKMULTISIG.
func multisigScript(pubKeys [][]byte) []byte {
	s := []byte{0x51} // OP_1 (m=1)
	for _, pk := range pubKeys {
		s = append(s, byte(len(pk)))
		s = append(s, pk...)
	}
	s = append(s, byte(0x50+len(pubKeys))) // OP_N (n=len(pubKeys))
	s = append(s, 0xae)                    // OP_CHECKMULTISIG
	return s
}

func rawVout(n uint32, valueSats int64, scriptBytes []byte, coreType string, address *string) rpc.RawVout {
	h := hex.EncodeToString(scriptBytes)
	return rpc.RawVout{
		Value: qogeAmount(valueSats),
		N:     &n,
		ScriptPubKey: &rpc.RawScriptPubKey{
			Hex: &h, Type: coreType, Address: address,
		},
	}
}

func rawSpendVin(prevTxid string, prevVout uint32, scriptSigHex string, witness ...string) rpc.RawVin {
	sequence := uint32(0xfffffffd) // RBF-signaling, matching a realistic mempool tx
	return rpc.RawVin{
		TxID: &prevTxid, Vout: &prevVout,
		ScriptSig:   &rpc.RawScriptSig{Hex: &scriptSigHex},
		Sequence:    &sequence,
		TxInWitness: witness,
	}
}

// rawMempoolTx builds a non-coinbase rpc.RawTransaction — every mempool
// fixture transaction's shape — with a distinct txid and wtxid whenever
// witness data is present (Core's "hash" is the witness-inclusive
// serialization; "txid" is not), and identical when it's not.
func rawMempoolTx(label string, vin []rpc.RawVin, vout []rpc.RawVout) rpc.RawTransaction {
	txid := fakeHash(label + "-txid")
	wtxid := txid
	hasWitness := false
	for _, v := range vin {
		if len(v.TxInWitness) > 0 {
			hasWitness = true
			break
		}
	}
	if hasWitness {
		wtxid = fakeHash(label + "-wtxid")
	}
	version := uint32(2)
	lockTime := uint32(0)
	size, vsize, weight := 250, 200, 800
	return rpc.RawTransaction{
		TxID: &txid, Hash: &wtxid,
		Version: &version, Size: &size, VSize: &vsize, Weight: &weight, LockTime: &lockTime,
		Vin:  vin,
		Vout: vout,
	}
}

// rawMempoolTxWithTxID is rawMempoolTx but with an explicit, caller-chosen
// txid/wtxid rather than one derived from a label — needed for fixtures
// that must control lexicographic ordering deterministically (e.g. a
// child transaction whose txid sorts before its parent's).
func rawMempoolTxWithTxID(txid string, vin []rpc.RawVin, vout []rpc.RawVout) rpc.RawTransaction {
	wtxid := txid
	version := uint32(2)
	lockTime := uint32(0)
	size, vsize, weight := 250, 200, 800
	return rpc.RawTransaction{
		TxID: &txid, Hash: &wtxid,
		Version: &version, Size: &size, VSize: &vsize, Weight: &weight, LockTime: &lockTime,
		Vin:  vin,
		Vout: vout,
	}
}

// mempoolEntry builds the getrawmempool-verbose metadata for one
// transaction, mirroring the live shape confirmed against a real
// Qogecoin Core node during this phase's manual check.
func mempoolEntry(vsize, weight int, feeSats int64, entryTime int64, entryHeight int64, depends []string, replaceable bool) rpc.RawMempoolEntry {
	return rpc.RawMempoolEntry{
		VSize:  intPtr(vsize),
		Weight: intPtr(weight),
		Time:   i64Ptr(entryTime),
		Height: i64Ptr(entryHeight),
		Fees:   &rpc.MempoolFees{Base: qogeAmount(feeSats)},
		Depends: func() []string {
			if depends == nil {
				return []string{}
			}
			return depends
		}(),
		BIP125Replaceable: boolPtr(replaceable),
	}
}

// confirmedBlockFixture builds a single, structurally valid genesis-shaped
// confirmed chain.Block: one coinbase transaction, one P2PKH output paying
// valueSats to addr. Used by tests that need real confirmed PostgreSQL
// state (via store.ApplyBlock) to prove mempool operations never touch it.
func confirmedBlockFixture(t *testing.T, _ context.Context, label string, height int64, prevHash string, addr string, valueSats int64) chain.Block {
	t.Helper()
	txid := fakeHash(label + "-cb-tx")
	txn := chain.Transaction{
		TxID:       txid,
		WTxID:      txid,
		Version:    1,
		LockTime:   0,
		Size:       100,
		VSize:      100,
		Weight:     400,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 0xffffffff},
		},
		Outputs: []chain.Output{
			{
				Index:        0,
				Value:        chain.Amount(valueSats),
				ScriptPubKey: p2pkhScript(label),
				ScriptType:   script.TypeP2PKH,
				Address:      addr,
			},
		},
	}
	return chain.Block{
		Hash:         fakeHash(label),
		Height:       height,
		PreviousHash: prevHash,
		MerkleRoot:   fakeHash("merkle:" + label),
		Time:         1_700_000_000 + height,
		Bits:         "1d00ffff",
		Difficulty:   1.0,
		Nonce:        uint32(height),
		Size:         300,
		Weight:       1200,
		TxCount:      1,
		Transactions: []chain.Transaction{txn},
	}
}

type fakeAddressResolver struct{}

func (fakeAddressResolver) ResolvePubKeyAddress(_ context.Context, pubKeyHex string) (string, error) {
	return "qResolved" + pubKeyHex[:8], nil
}
