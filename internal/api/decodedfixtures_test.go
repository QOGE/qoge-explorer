package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// The helpers in this file mirror internal/query/rpcfixtures_test.go: they
// drive a block through the REAL decode.DecodeBlock -> Store.ApplyBlock
// pipeline instead of hand-assembling a chain.Block with a hand-picked
// ScriptType, so that HTTP-layer tests exercising script classification
// (P2QPK in particular) prove the actual decoder assigns it, not a test
// fixture's say-so. Duplicated per-package on purpose — see dbtest_test.go's
// note on internal/query/internal/store boundary duplication.

// fakeAddressResolver implements decode.AddressResolver deterministically
// for bare P2PK/multisig outputs; unused by every fixture in this file
// (which sticks to P2PKH/P2QPK, both of which Core reports an address for
// directly) but required to satisfy decode.DecodeBlock's signature.
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

// witnessProgramScript builds <opVersion><push len(program)><program>, the
// wire shape of every SegWit-style witness output (P2WPKH/P2TR/P2QPK/
// unknown witness versions alike).
func witnessProgramScript(version int, program []byte) []byte {
	s := make([]byte, 0, 2+len(program))
	s = append(s, byte(0x50+version))
	if version == 0 {
		s[0] = 0x00
	}
	s = append(s, byte(len(program)))
	s = append(s, program...)
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

// decodedP2QPKFixture drives genesis -> block1 -> block2 through the REAL
// decode.DecodeBlock -> Store.ApplyBlock pipeline, where block2 contains a
// transaction spending block1's coinbase with a structurally valid P2QPK
// output (witness v2, 32-byte program) and a 2-item witness stack shaped
// like a real P2QPK spend: [17,088-byte signature, 32-byte pubkey]. The
// decoder — not the fixture — assigns script.TypeP2QPK, because the output
// script bytes are the real OP_2 PUSH32 <program> wire shape, not an
// arbitrary placeholder.
type decodedP2QPKFixture struct {
	genesisHash, block1Hash, block2Hash string
	block1CBTxid                        string
	spendTxid, spendWtxid               string
	sigItem, pubkeyItem, program        []byte
}

func buildDecodedP2QPKFixture(t *testing.T, ctx context.Context, st *store.Store) decodedP2QPKFixture {
	t.Helper()
	resolver := fakeAddressResolver{}

	gAddr := "qDP2QPKGenesis"
	genesisScript := p2pkhScript("dp2qpk-genesis")
	genesisRaw := rawBlockFixture(fakeHash("dp2qpk-genesis"), 0, "",
		rawCoinbaseTx("dp2qpk-genesis-cb", "51", rawVout(0, 100_00000000, genesisScript, "pubkeyhash", &gAddr)),
	)
	genesisBlock, err := decode.DecodeBlock(ctx, genesisRaw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(genesis): %v", err)
	}
	if err := st.ApplyBlock(ctx, genesisBlock); err != nil {
		t.Fatalf("ApplyBlock(genesis): %v", err)
	}

	b1Addr := "qDP2QPKBlock1CB"
	b1cbScript := p2pkhScript("dp2qpk-block1-cb")
	block1Raw := rawBlockFixture(fakeHash("dp2qpk-block1"), 1, genesisBlock.Hash,
		rawCoinbaseTx("dp2qpk-block1-cb", "51", rawVout(0, 50_00000000, b1cbScript, "pubkeyhash", &b1Addr)),
	)
	block1, err := decode.DecodeBlock(ctx, block1Raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(block1): %v", err)
	}
	if err := st.ApplyBlock(ctx, block1); err != nil {
		t.Fatalf("ApplyBlock(block1): %v", err)
	}
	block1CBTxid := *block1Raw.Tx[0].TxID

	cb2Addr := "qDP2QPKBlock2CB"
	cb2Script := p2pkhScript("dp2qpk-block2-cb")
	cb2 := rawCoinbaseTx("dp2qpk-block2-cb", "51", rawVout(0, 50_00000000, cb2Script, "pubkeyhash", &cb2Addr))

	sigItem := make([]byte, script.P2QPKSignatureLength)
	for i := range sigItem {
		sigItem[i] = 0xab
	}
	pubkeyItem := make([]byte, script.P2QPKPublicKeyLength)
	for i := range pubkeyItem {
		pubkeyItem[i] = 0xcd
	}
	program := make([]byte, script.P2QPKProgramLength)
	for i := range program {
		program[i] = 0xef
	}
	p2qpkAddr := "qDP2QPKDest"

	vin := []rpc.RawVin{
		rawSpendVin(block1CBTxid, 0, "", hex.EncodeToString(sigItem), hex.EncodeToString(pubkeyItem)),
	}
	vout := []rpc.RawVout{
		rawVout(0, 40_00000000, witnessProgramScript(script.P2QPKWitnessVersion, program), "witness_unknown", &p2qpkAddr),
	}
	spend := rawSpendTx("dp2qpk-spend", 17200, 4310, 17240, vin, vout)

	block2Raw := rawBlockFixture(fakeHash("dp2qpk-block2"), 2, block1.Hash, cb2, spend)
	block2, err := decode.DecodeBlock(ctx, block2Raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(block2): %v", err)
	}
	if err := st.ApplyBlock(ctx, block2); err != nil {
		t.Fatalf("ApplyBlock(block2): %v", err)
	}

	return decodedP2QPKFixture{
		genesisHash:  genesisBlock.Hash,
		block1Hash:   block1.Hash,
		block2Hash:   block2.Hash,
		block1CBTxid: block1CBTxid,
		spendTxid:    *spend.TxID,
		spendWtxid:   *spend.Hash,
		sigItem:      sigItem,
		pubkeyItem:   pubkeyItem,
		program:      program,
	}
}
