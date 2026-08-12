package deployments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/mempool"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// The confirmed-chain and mempool fixture helpers in this file mirror
// internal/query/fixtures_test.go and internal/query/mempool_fixtures_test.go
// — duplicated per-package on purpose (test-only files aren't importable
// across packages; see dbtest_test.go's equivalent note).

// fakeHash deterministically derives a 64-char lowercase hex hash from a
// human-readable label, so fixtures stay short and reproducible without
// ever colliding across labels in the same test.
func fakeHash(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

// p2pkhScript builds a structurally valid 25-byte P2PKH scriptPubKey.
func p2pkhScript(label string) []byte {
	sum := sha256.Sum256([]byte("script:" + label))
	s := make([]byte, 0, 25)
	s = append(s, 0x76, 0xa9, 0x14)
	s = append(s, sum[:20]...)
	s = append(s, 0x88, 0xac)
	return s
}

// coinbaseTx builds a single-input (coinbase), single-output (P2PKH)
// transaction paying valueSats to addr.
func coinbaseTx(label string, valueSats int64, addr string) chain.Transaction {
	txid := fakeHash(label + "-tx")
	return chain.Transaction{
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
}

// block assembles a chain.Block header around txs, with deterministic
// filler header fields derived from label/height.
func block(label string, height int64, prevHash string, txs ...chain.Transaction) chain.Block {
	return chain.Block{
		Hash:         fakeHash(label),
		Height:       height,
		PreviousHash: prevHash,
		MerkleRoot:   fakeHash("merkle:" + label),
		Time:         1_700_000_000 + height,
		Bits:         "1d00ffff",
		Difficulty:   1.0,
		Nonce:        uint32(height),
		Size:         200 + 100*len(txs),
		Weight:       800 + 400*len(txs),
		TxCount:      len(txs),
		Transactions: txs,
	}
}

// fakeAddressResolver satisfies decode.AddressResolver deterministically,
// mirroring internal/web/decodedfixtures_test.go's helper of the same
// name.
type fakeAddressResolver struct{}

func (fakeAddressResolver) ResolvePubKeyAddress(_ context.Context, pubKeyHex string) (string, error) {
	return "qResolved" + pubKeyHex[:8], nil
}

func qogeAmount(satoshis int64) json.Number {
	whole := satoshis / 100_000_000
	frac := satoshis % 100_000_000
	return json.Number(fmt.Sprintf("%d.%08d", whole, frac))
}

func rawSpendVin(prevTxid string, prevVout uint32, scriptSigHex string) rpc.RawVin {
	sequence := uint32(0xffffffff)
	return rpc.RawVin{
		TxID: &prevTxid, Vout: &prevVout,
		ScriptSig: &rpc.RawScriptSig{Hex: &scriptSigHex},
		Sequence:  &sequence,
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

func simpleMempoolRawTx(label string) rpc.RawTransaction {
	addr := "q" + label
	txid := fakeHash(label + "-tx")
	version := uint32(2)
	lockTime := uint32(0)
	size, vsize, weight := 150, 150, 600
	return rpc.RawTransaction{
		TxID: &txid, Hash: &txid,
		Version: &version, Size: &size, VSize: &vsize, Weight: &weight, LockTime: &lockTime,
		Vin:  []rpc.RawVin{rawSpendVin(fakeHash(label+"-prev"), 0, "473044")},
		Vout: []rpc.RawVout{rawVout(0, 1_00000000, p2pkhScript(label), "pubkeyhash", &addr)},
	}
}

func mempoolCandidateTx(t *testing.T, ctx context.Context, raw rpc.RawTransaction, feeSats, entryTime int64) mempool.CandidateTransaction {
	t.Helper()
	txn, err := decode.DecodeTransaction(ctx, raw, fakeAddressResolver{})
	if err != nil {
		t.Fatalf("DecodeTransaction: %v", err)
	}
	return mempool.CandidateTransaction{
		Transaction: txn,
		FeeSatoshis: feeSats,
		EntryTime:   entryTime,
	}
}

func mempoolCandidate(coreTipHeight int64, coreTipHash string, txs ...mempool.CandidateTransaction) mempool.Candidate {
	return mempool.Candidate{
		CoreTipHeight: coreTipHeight,
		CoreTipHash:   coreTipHash,
		Transactions:  txs,
	}
}
