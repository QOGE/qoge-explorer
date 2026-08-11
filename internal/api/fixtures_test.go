package api

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
)

func fakeHash(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func p2pkhScript(label string) []byte {
	sum := sha256.Sum256([]byte("script:" + label))
	s := make([]byte, 0, 25)
	s = append(s, 0x76, 0xa9, 0x14)
	s = append(s, sum[:20]...)
	s = append(s, 0x88, 0xac)
	return s
}

func coinbaseTx(label string, valueSats int64, addr string) chain.Transaction {
	txid := fakeHash(label + "-tx")
	return chain.Transaction{
		TxID: txid, WTxID: txid,
		Version: 1, LockTime: 0,
		Size: 100, VSize: 100, Weight: 400,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 0xffffffff},
		},
		Outputs: []chain.Output{
			{Index: 0, Value: chain.Amount(valueSats), ScriptPubKey: p2pkhScript(label), ScriptType: script.TypeP2PKH, Address: addr},
		},
	}
}

func spendTx(label, prevTxid string, prevVout uint32, valueSats int64, toAddr string) chain.Transaction {
	txid := fakeHash(label + "-tx")
	return chain.Transaction{
		TxID: txid, WTxID: txid,
		Version: 1, LockTime: 0,
		Size: 150, VSize: 150, Weight: 600,
		Inputs: []chain.Input{
			{
				Index:       0,
				PreviousOut: &chain.OutPoint{TxID: prevTxid, Index: prevVout},
				ScriptSig:   []byte{0x47, 0x30, 0x44, 0x02},
				Sequence:    0xffffffff,
			},
		},
		Outputs: []chain.Output{
			{Index: 0, Value: chain.Amount(valueSats), ScriptPubKey: p2pkhScript(label), ScriptType: script.TypeP2PKH, Address: toAddr},
		},
	}
}

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
