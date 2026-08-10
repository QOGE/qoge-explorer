package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// ─── deterministic fixture builders ─────────────────────────────────────

// fakeHash returns a deterministic, valid-shaped (64 lowercase hex chars)
// hash from label — the same synthetic-fixture spirit as
// internal/store/invariants_test.go's hash64.
func fakeHash(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

// p2pkhScript builds a structurally valid 25-byte P2PKH scriptPubKey
// (OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG) so real
// script.Classify/decode.DecodeBlock accept it exactly as they would real
// Core wire data — the fixtures below never bypass those already-reviewed
// components.
func p2pkhScript(label string) []byte {
	sum := sha256.Sum256([]byte("script:" + label))
	s := make([]byte, 0, 25)
	s = append(s, 0x76, 0xa9, 0x14)
	s = append(s, sum[:20]...)
	s = append(s, 0x88, 0xac)
	return s
}

// qogeAmount renders satoshis as the exact decimal-QOGE string Core's own
// ValueFromAmount emits (see decode.DecodeAmount) — pure integer
// arithmetic, never float64.
func qogeAmount(satoshis int64) json.Number {
	whole := satoshis / 100_000_000
	frac := satoshis % 100_000_000
	return json.Number(fmt.Sprintf("%d.%08d", whole, frac))
}

// fakeCoinbaseTx builds a single-input (coinbase), single-output (P2PKH)
// RawTransaction — the minimal shape decode.DecodeTransaction/Store.
// ApplyBlock accept, deliberately avoiding any prevout/fee bookkeeping so
// indexer orchestration tests aren't also re-testing Store's UTXO logic.
func fakeCoinbaseTx(label string, valueSatoshis int64) rpc.RawTransaction {
	txid := fakeHash(label + "-tx")
	version := uint32(1)
	size := 100
	vsize := 100
	weight := 400
	locktime := uint32(0)
	coinbaseHex := "51"
	sequence := uint32(0xffffffff)
	n := uint32(0)
	scriptHex := hex.EncodeToString(p2pkhScript(label))
	addr := "qFake" + label

	return rpc.RawTransaction{
		TxID:     &txid,
		Hash:     &txid, // no witness data: wtxid == txid
		Version:  &version,
		Size:     &size,
		VSize:    &vsize,
		Weight:   &weight,
		LockTime: &locktime,
		Vin: []rpc.RawVin{
			{Coinbase: &coinbaseHex, Sequence: &sequence},
		},
		Vout: []rpc.RawVout{
			{Value: qogeAmount(valueSatoshis), N: &n, ScriptPubKey: &rpc.RawScriptPubKey{Hex: &scriptHex, Address: &addr}},
		},
	}
}

// fakeBlock is one synthetic block: enough of getblock verbose=2's shape
// to round-trip through the real decode.DecodeBlock/Store.ApplyBlock path.
type fakeBlock struct {
	hash     string
	prevHash string
	height   int64
	txs      []rpc.RawTransaction
}

// buildBlock constructs a single-coinbase-transaction fakeBlock. label
// must be unique per intended block (it seeds the hash, txid, and output
// script/address deterministically) — tests build explicit chains by
// wiring each block's prevHash to the previous block's hash themselves, so
// branch/flip-back scenarios are fully explicit rather than inferred from
// naming.
func buildBlock(label string, height int64, prevHash string) *fakeBlock {
	return &fakeBlock{
		hash:     fakeHash(label),
		prevHash: prevHash,
		height:   height,
		txs:      []rpc.RawTransaction{fakeCoinbaseTx(label, 50_00000000)},
	}
}

func (b *fakeBlock) raw() rpc.RawBlock {
	hash := b.hash
	height := b.height
	merkleRoot := fakeHash("merkle:" + b.hash)
	blockTime := int64(1_700_000_000 + b.height)
	bits := "1d00ffff"
	difficulty := 1.0
	nonce := uint32(b.height)
	size := 250
	weight := 1000
	nTx := len(b.txs)

	var prevPtr *string
	if b.prevHash != "" {
		p := b.prevHash
		prevPtr = &p
	}

	return rpc.RawBlock{
		Hash:              &hash,
		Height:            &height,
		PreviousBlockHash: prevPtr,
		MerkleRoot:        &merkleRoot,
		Time:              &blockTime,
		Bits:              &bits,
		Difficulty:        &difficulty,
		Nonce:             &nonce,
		Size:              &size,
		Weight:            &weight,
		NTx:               &nTx,
		Tx:                b.txs,
	}
}

// ─── fake RPC client ─────────────────────────────────────────────────────

// fakeRPC is a deterministic, in-memory stand-in for RPCClient. It
// maintains an "active chain" (what GetBlockCount/GetBlockHash report) and
// a superset "known blocks" registry (what GetBlockVerbose2 can resolve by
// hash, including orphaned/replaced branches — mirroring real Core, which
// keeps orphan block data around). Tests mutate the active chain between —
// or, via hashQueue, within — calls to simulate reorgs and remote-chain
// races without any timing dependence.
type fakeRPC struct {
	mu sync.Mutex

	active      []*fakeBlock          // index i = height i
	byHash      map[string]*fakeBlock // every block ever registered, any branch
	rawOverride map[string]rpc.RawBlock

	// hashQueue[height] is a one-shot-per-call queue of hashes to return
	// from GetBlockHash(height) before falling back to active[height].hash
	// — used to simulate the exact race task item 7/8 describes (the
	// height's hash differs between two calls within one applyHeight, or
	// during ancestor-discovery recheck).
	hashQueue map[int64][]string

	hashErrOnce     map[int64]error
	verbose2ErrOnce map[string]error
	countQueue      []int64 // one-shot queue of GetBlockCount results before falling back to len(active)-1

	blockOnCount bool
	gateEnter    chan struct{}
	gateRelease  chan struct{}

	hashCalls     map[int64]int
	countCalls    int
	verbose2Calls int
}

func newFakeRPC() *fakeRPC {
	return &fakeRPC{
		byHash:          map[string]*fakeBlock{},
		rawOverride:     map[string]rpc.RawBlock{},
		hashQueue:       map[int64][]string{},
		hashErrOnce:     map[int64]error{},
		verbose2ErrOnce: map[string]error{},
		hashCalls:       map[int64]int{},
	}
}

// setActiveChain replaces the current active chain wholesale and registers
// every block in it (so GetBlockVerbose2 can resolve them). blocks[i] must
// have height == i.
func (f *fakeRPC) setActiveChain(blocks ...*fakeBlock) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = append([]*fakeBlock{}, blocks...)
	for _, b := range blocks {
		f.byHash[b.hash] = b
	}
}

// registerOrphan makes blocks resolvable by GetBlockVerbose2 without
// changing the active chain — the "Core still has the data, it's just not
// on the active chain" case.
func (f *fakeRPC) registerOrphan(blocks ...*fakeBlock) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range blocks {
		f.byHash[b.hash] = b
	}
}

func (f *fakeRPC) queueHashOnce(height int64, hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashQueue[height] = append(f.hashQueue[height], hash)
}

func (f *fakeRPC) queueHashErrOnce(height int64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashErrOnce[height] = err
}

func (f *fakeRPC) queueVerbose2ErrOnce(hash string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verbose2ErrOnce[hash] = err
}

func (f *fakeRPC) queueCountOnce(height int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countQueue = append(f.countQueue, height)
}

func (f *fakeRPC) setRawOverride(hash string, raw rpc.RawBlock) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawOverride[hash] = raw
}

// enableCountGate makes every future GetBlockCount call block until
// releaseGate is called, first signaling gateEnter so a test can
// deterministically know the call is in flight (used only by the
// no-overlapping-sync-passes test).
func (f *fakeRPC) enableCountGate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockOnCount = true
	f.gateEnter = make(chan struct{}, 1)
	f.gateRelease = make(chan struct{})
}

func (f *fakeRPC) waitGateEnter() {
	<-f.gateEnter
}

func (f *fakeRPC) releaseGate() {
	close(f.gateRelease)
}

func (f *fakeRPC) GetBlockCount(ctx context.Context) (int64, error) {
	f.mu.Lock()
	f.countCalls++
	blockOnCount := f.blockOnCount
	gateEnter := f.gateEnter
	gateRelease := f.gateRelease
	f.mu.Unlock()

	if blockOnCount {
		select {
		case gateEnter <- struct{}{}:
		default:
		}
		<-gateRelease
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.countQueue) > 0 {
		v := f.countQueue[0]
		f.countQueue = f.countQueue[1:]
		return v, nil
	}
	return int64(len(f.active)) - 1, nil
}

func (f *fakeRPC) GetBlockHash(ctx context.Context, height int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashCalls[height]++

	if err, ok := f.hashErrOnce[height]; ok {
		delete(f.hashErrOnce, height)
		return "", err
	}
	if q := f.hashQueue[height]; len(q) > 0 {
		f.hashQueue[height] = q[1:]
		return q[0], nil
	}
	if height < 0 || height >= int64(len(f.active)) {
		return "", fmt.Errorf("fake rpc: height %d out of range (tip %d)", height, len(f.active)-1)
	}
	return f.active[height].hash, nil
}

func (f *fakeRPC) GetBlockVerbose2(ctx context.Context, hash string) (rpc.RawBlock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verbose2Calls++

	if err, ok := f.verbose2ErrOnce[hash]; ok {
		delete(f.verbose2ErrOnce, hash)
		return rpc.RawBlock{}, err
	}
	if raw, ok := f.rawOverride[hash]; ok {
		return raw, nil
	}
	b, ok := f.byHash[hash]
	if !ok {
		return rpc.RawBlock{}, fmt.Errorf("fake rpc: unknown block hash %s", hash)
	}
	return b.raw(), nil
}

func (f *fakeRPC) callCounts() (hash map[int64]int, count, verbose2 int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := make(map[int64]int, len(f.hashCalls))
	for k, v := range f.hashCalls {
		h[k] = v
	}
	return h, f.countCalls, f.verbose2Calls
}

var _ RPCClient = (*fakeRPC)(nil)
