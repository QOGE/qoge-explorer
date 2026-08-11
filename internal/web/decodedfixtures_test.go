package web

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

// The helpers in this file mirror internal/query/rpcfixtures_test.go and
// internal/api/decodedfixtures_test.go — duplicated per-package on purpose
// (see dbtest_test.go's note): they drive a block through the REAL
// rpc.RawBlock -> decode.DecodeBlock -> Store.ApplyBlock pipeline, so
// script_type is the decoder's own classification, never a fixture's
// say-so — the exact property test S/T (P2QPK/P2TR distinct rendering)
// needs to prove anything about the web layer.

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

// decodedFixture drives genesis -> block1 -> block2 through the REAL
// decode.DecodeBlock -> Store.ApplyBlock pipeline, where block2's spend
// transaction has a 2-item witness stack shaped like a real P2QPK spend
// ([17,088-byte signature, 32-byte pubkey]) and four structurally distinct
// outputs: P2QPK (v2/32), P2TR (v1/32), P2WPKH (v0/20), and OP_RETURN.
type decodedFixture struct {
	genesisHash, block1Hash, block2Hash string
	block1CBTxid                        string
	spendTxid, spendWtxid               string
	sigItem, pubkeyItem, p2qpkProgram   []byte
}

func buildDecodedFixture(t *testing.T, ctx context.Context, st *store.Store) decodedFixture {
	t.Helper()
	resolver := fakeAddressResolver{}

	gAddr := "qWebDecGenesis"
	genesisScript := p2pkhScript("web-dec-genesis")
	genesisRaw := rawBlockFixture(fakeHash("web-dec-genesis"), 0, "",
		rawCoinbaseTx("web-dec-genesis-cb", "51", rawVout(0, 100_00000000, genesisScript, "pubkeyhash", &gAddr)),
	)
	genesisBlock, err := decode.DecodeBlock(ctx, genesisRaw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(genesis): %v", err)
	}
	if err := st.ApplyBlock(ctx, genesisBlock); err != nil {
		t.Fatalf("ApplyBlock(genesis): %v", err)
	}

	b1Addr := "qWebDecBlock1CB"
	b1cbScript := p2pkhScript("web-dec-block1-cb")
	block1Raw := rawBlockFixture(fakeHash("web-dec-block1"), 1, genesisBlock.Hash,
		rawCoinbaseTx("web-dec-block1-cb", "51", rawVout(0, 50_00000000, b1cbScript, "pubkeyhash", &b1Addr)),
	)
	block1, err := decode.DecodeBlock(ctx, block1Raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(block1): %v", err)
	}
	if err := st.ApplyBlock(ctx, block1); err != nil {
		t.Fatalf("ApplyBlock(block1): %v", err)
	}
	block1CBTxid := *block1Raw.Tx[0].TxID

	cb2Addr := "qWebDecBlock2CB"
	cb2Script := p2pkhScript("web-dec-block2-cb")
	cb2 := rawCoinbaseTx("web-dec-block2-cb", "51", rawVout(0, 50_00000000, cb2Script, "pubkeyhash", &cb2Addr))

	sigItem := make([]byte, script.P2QPKSignatureLength)
	for i := range sigItem {
		sigItem[i] = 0xab
	}
	pubkeyItem := make([]byte, script.P2QPKPublicKeyLength)
	for i := range pubkeyItem {
		pubkeyItem[i] = 0xcd
	}
	p2qpkProgram := make([]byte, script.P2QPKProgramLength)
	for i := range p2qpkProgram {
		p2qpkProgram[i] = 0xef
	}
	p2trProgram := make([]byte, 32)
	for i := range p2trProgram {
		p2trProgram[i] = 0x11
	}
	p2wpkhProgram := make([]byte, 20)
	for i := range p2wpkhProgram {
		p2wpkhProgram[i] = 0x22
	}

	p2qpkAddr := "qWebDecP2QPKDest"
	p2trAddr := "qWebDecP2TRDest"
	p2wpkhAddr := "qWebDecP2WPKHDest"

	vin := []rpc.RawVin{
		rawSpendVin(block1CBTxid, 0, "", hex.EncodeToString(sigItem), hex.EncodeToString(pubkeyItem)),
	}
	vout := []rpc.RawVout{
		rawVout(0, 25_00000000, witnessProgramScript(script.P2QPKWitnessVersion, p2qpkProgram), "witness_unknown", &p2qpkAddr),
		rawVout(1, 10_00000000, witnessProgramScript(1, p2trProgram), "witness_v1_taproot", &p2trAddr),
		rawVout(2, 5_00000000, witnessProgramScript(0, p2wpkhProgram), "witness_v0_keyhash", &p2wpkhAddr),
		rawVout(3, 0, nullDataScript([]byte("qoge")), "nulldata", nil),
	}
	spend := rawSpendTx("web-dec-spend", 17200, 4310, 17240, vin, vout)

	block2Raw := rawBlockFixture(fakeHash("web-dec-block2"), 2, block1.Hash, cb2, spend)
	block2, err := decode.DecodeBlock(ctx, block2Raw, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock(block2): %v", err)
	}
	if err := st.ApplyBlock(ctx, block2); err != nil {
		t.Fatalf("ApplyBlock(block2): %v", err)
	}

	return decodedFixture{
		genesisHash:  genesisBlock.Hash,
		block1Hash:   block1.Hash,
		block2Hash:   block2.Hash,
		block1CBTxid: block1CBTxid,
		spendTxid:    *spend.TxID,
		spendWtxid:   *spend.Hash,
		sigItem:      sigItem,
		pubkeyItem:   pubkeyItem,
		p2qpkProgram: p2qpkProgram,
	}
}

func nullDataScript(data []byte) []byte {
	s := []byte{0x6a}
	if len(data) > 0 {
		s = append(s, byte(len(data)))
		s = append(s, data...)
	}
	return s
}
