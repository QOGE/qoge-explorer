package mempool

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestSynchronizer(rpcClient RPCClient, confirmed ConfirmedTipReader, mempoolStore *Store) *Synchronizer {
	return New(rpcClient, confirmed, mempoolStore, fakeAddressResolver{}, 0, discardLogger())
}

// simpleRawMempoolTx builds a single-input, single-output non-coinbase
// rpc.RawTransaction (no witness) — the common shape most of this file's
// race-fixture tests need, plus the matching getrawmempool entry.
func simpleRawMempoolTx(label string, feeSats int64) (rpc.RawTransaction, rpc.RawMempoolEntry) {
	prevTxid := fakeHash(label + "-prev")
	addr := "qSync" + label
	vin := []rpc.RawVin{rawSpendVin(prevTxid, 0, "473044")}
	vout := []rpc.RawVout{rawVout(0, 10_00000000, p2pkhScript(label), "pubkeyhash", &addr)}
	raw := rawMempoolTx(label, vin, vout)
	// vsize/weight must agree with rawMempoolTx's own hardcoded 200/800 —
	// fetchAndDecode now cross-checks getrawmempool's vsize/weight against
	// the strictly decoded getrawtransaction response (spec item 9).
	entry := mempoolEntry(200, 800, feeSats, 1_700_000_000, 100, nil, true)
	return raw, entry
}

// TestRefreshOnce_ConfirmedIndexBehindCore is spec item 39.1: the
// confirmed PostgreSQL tip does not match Core's active tip -> no
// publication.
func TestRefreshOnce_ConfirmedIndexBehindCore(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 5, Hash: fakeHash("db-tip-5")}}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{10}, // Core is ahead of the confirmed checkpoint
		bestHashSeq:   []string{fakeHash("core-tip-10")},
	}
	s := newTestSynchronizer(client, confirmed, mstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrConfirmedIndexBehind) {
		t.Fatalf("refreshOnce error = %v, want ErrConfirmedIndexBehind", err)
	}
	state, stateErr := mstore.State(ctx)
	if stateErr != nil {
		t.Fatalf("State: %v", stateErr)
	}
	if state.Initialized {
		t.Fatalf("state.Initialized = true after a skipped-due-to-behind refresh, want false")
	}
}

// TestRefreshOnce_CoreTipMovedDuringAcquisition is spec item 39.2.
func TestRefreshOnce_CoreTipMovedDuringAcquisition(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	tip := store.Checkpoint{Height: 10, Hash: fakeHash("db-tip-10")}
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{tip}} // same both times
	client := &fakeRPCClient{
		blockCountSeq:  []int64{10, 11}, // moved between initial and final read
		bestHashSeq:    []string{tip.Hash, tip.Hash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{},
	}
	s := newTestSynchronizer(client, confirmed, mstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrMempoolRace) {
		t.Fatalf("refreshOnce error = %v, want ErrMempoolRace", err)
	}
	requireNeverPublished(t, ctx, mstore)
}

// TestRefreshOnce_DBTipMovedDuringAcquisition is spec item 39.3.
func TestRefreshOnce_DBTipMovedDuringAcquisition(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	coreHash := fakeHash("db-tip-move-core")
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{
		{Height: 10, Hash: coreHash},
		{Height: 11, Hash: fakeHash("db-tip-moved-on")},
	}}
	client := &fakeRPCClient{
		blockCountSeq:  []int64{10}, // Core tip stays put
		bestHashSeq:    []string{coreHash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{},
	}
	s := newTestSynchronizer(client, confirmed, mstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrMempoolRace) {
		t.Fatalf("refreshOnce error = %v, want ErrMempoolRace", err)
	}
	requireNeverPublished(t, ctx, mstore)
}

// TestRefreshOnce_ListedTransactionDisappears is spec item 39.4.
func TestRefreshOnce_ListedTransactionDisappears(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	tip := store.Checkpoint{Height: 10, Hash: fakeHash("db-tip-disappear")}
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}

	raw, entry := simpleRawMempoolTx("Disappear", 1000)
	client := &fakeRPCClient{
		blockCountSeq:  []int64{10},
		bestHashSeq:    []string{tip.Hash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{*raw.TxID: entry},
		rawTxErrs:      map[string]error{*raw.TxID: errors.New("No such mempool or blockchain transaction")},
	}
	s := newTestSynchronizer(client, confirmed, mstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrMempoolRace) {
		t.Fatalf("refreshOnce error = %v, want ErrMempoolRace", err)
	}
	requireNeverPublished(t, ctx, mstore)
}

// TestRefreshOnce_ListedTransactionBecameConfirmed is spec item 39.5.
func TestRefreshOnce_ListedTransactionBecameConfirmed(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	tip := store.Checkpoint{Height: 10, Hash: fakeHash("db-tip-confirmed-race")}
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}

	raw, entry := simpleRawMempoolTx("BecameConfirmed", 1000)
	confirmedRaw := raw
	blockHash := fakeHash("some-new-block")
	confirmedRaw.BlockHash = &blockHash

	client := &fakeRPCClient{
		blockCountSeq:  []int64{10},
		bestHashSeq:    []string{tip.Hash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{*raw.TxID: entry},
		rawTxs:         map[string]rpc.RawTransaction{*raw.TxID: confirmedRaw},
	}
	s := newTestSynchronizer(client, confirmed, mstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrMempoolRace) {
		t.Fatalf("refreshOnce error = %v, want ErrMempoolRace", err)
	}
	requireNeverPublished(t, ctx, mstore)
}

// TestRefreshOnce_TemporaryRPCErrorRetainsPreviousSnapshot is spec item
// 39.6: a transient getrawmempool RPC error must not clear or otherwise
// disturb whatever snapshot was already committed.
func TestRefreshOnce_TemporaryRPCErrorRetainsPreviousSnapshot(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	tip := store.Checkpoint{Height: 10, Hash: fakeHash("db-tip-rpcerr")}

	// First, a real successful publication.
	raw, entry := simpleRawMempoolTx("BeforeError", 1000)
	confirmedOK := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}
	clientOK := &fakeRPCClient{
		blockCountSeq:  []int64{10},
		bestHashSeq:    []string{tip.Hash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{*raw.TxID: entry},
		rawTxs:         map[string]rpc.RawTransaction{*raw.TxID: raw},
	}
	sOK := newTestSynchronizer(clientOK, confirmedOK, mstore)
	if err := sOK.refreshOnce(ctx); err != nil {
		t.Fatalf("initial successful refreshOnce: %v", err)
	}
	stateBefore, err := mstore.State(ctx)
	if err != nil {
		t.Fatalf("State before: %v", err)
	}

	// Now a temporary RPC error on the next cycle.
	confirmedErr := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}
	clientErr := &fakeRPCClient{
		blockCountSeq: []int64{10},
		bestHashSeq:   []string{tip.Hash},
		mempoolErr:    errors.New("connection refused"),
	}
	sErr := newTestSynchronizer(clientErr, confirmedErr, mstore)
	if err := sErr.refreshOnce(ctx); err == nil {
		t.Fatalf("refreshOnce with a temporary RPC error: got nil error")
	} else if errors.Is(err, ErrConfirmedIndexBehind) || errors.Is(err, ErrMempoolRace) {
		t.Fatalf("refreshOnce error = %v, want a plain RPC failure (neither sentinel)", err)
	}

	stateAfter, err := mstore.State(ctx)
	if err != nil {
		t.Fatalf("State after: %v", err)
	}
	if stateAfter.Generation != stateBefore.Generation || stateAfter.TxCount != stateBefore.TxCount {
		t.Fatalf("state changed after a transient RPC error: before=%+v after=%+v", stateBefore, stateAfter)
	}
	requireMempoolTxExists(t, ctx, pool, *raw.TxID, true) // previous snapshot preserved
}

// requireNeverPublished asserts the mempool cache is still in its
// UNINITIALIZED bootstrap state — used by every "no publication" race
// test above, none of which ever call ReplaceSnapshot successfully.
func requireNeverPublished(t *testing.T, ctx context.Context, mstore *Store) {
	t.Helper()
	state, err := mstore.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Initialized || state.Generation != 0 {
		t.Fatalf("state = %+v, want uninitialized/generation=0 (no publication should have happened)", state)
	}
}

// TestRefreshOnce_ExactFeeParsing is spec item 40: Core's fee JSON must be
// converted to exact satoshis, never through float64. Every accepted
// value below round-trips exactly; every rejected value must discard the
// whole candidate rather than substitute a rounded/zero value.
func TestRefreshOnce_ExactFeeParsing(t *testing.T) {
	accepted := []struct {
		name    string
		feeJSON string
		wantSat int64
	}{
		{"zero", "0", 0},
		{"one_satoshi", "0.00000001", 1},
		{"one_qoge", "1.00000000", 100_000_000},
		{"eight_decimals", "12.34567890", 1_234_567_890},
	}
	for _, tc := range accepted {
		t.Run("accept_"+tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			mstore := NewStore(pool)

			tip := store.Checkpoint{Height: 10, Hash: fakeHash("fee-tip-" + tc.name)}
			confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}
			raw, entry := simpleRawMempoolTx("Fee"+tc.name, 0)
			entry.Fees = &rpc.MempoolFees{Base: jsonNum(tc.feeJSON)}
			client := &fakeRPCClient{
				blockCountSeq:  []int64{10},
				bestHashSeq:    []string{tip.Hash},
				mempoolEntries: map[string]rpc.RawMempoolEntry{*raw.TxID: entry},
				rawTxs:         map[string]rpc.RawTransaction{*raw.TxID: raw},
			}
			s := newTestSynchronizer(client, confirmed, mstore)
			if err := s.refreshOnce(ctx); err != nil {
				t.Fatalf("refreshOnce: %v", err)
			}
			var gotFee int64
			if err := pool.QueryRow(ctx, `SELECT fee_satoshis FROM mempool_transactions WHERE txid = $1`, *raw.TxID).Scan(&gotFee); err != nil {
				t.Fatalf("read fee_satoshis: %v", err)
			}
			if gotFee != tc.wantSat {
				t.Fatalf("fee_satoshis = %d, want %d", gotFee, tc.wantSat)
			}
		})
	}

	rejected := []string{
		"0.000000001",  // 9 fractional digits
		"-0.00000001",  // negative
		"1e8",          // exponent notation
		"not-a-number", // malformed
	}
	for _, feeJSON := range rejected {
		t.Run("reject_"+feeJSON, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			mstore := NewStore(pool)

			tip := store.Checkpoint{Height: 10, Hash: fakeHash("fee-reject-" + feeJSON)}
			confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}
			raw, entry := simpleRawMempoolTx("FeeReject", 0)
			entry.Fees = &rpc.MempoolFees{Base: jsonNum(feeJSON)}
			client := &fakeRPCClient{
				blockCountSeq:  []int64{10},
				bestHashSeq:    []string{tip.Hash},
				mempoolEntries: map[string]rpc.RawMempoolEntry{*raw.TxID: entry},
				rawTxs:         map[string]rpc.RawTransaction{*raw.TxID: raw},
			}
			s := newTestSynchronizer(client, confirmed, mstore)
			if err := s.refreshOnce(ctx); err == nil {
				t.Fatalf("refreshOnce with fee %q: got nil error, want rejection", feeJSON)
			}
			requireNeverPublished(t, ctx, mstore)
		})
	}
}

// TestRefreshOnce_CoinbaseShapedMempoolTransactionRejected: Core's mempool
// must never report a coinbase-shaped transaction, but the synchronizer
// defends against it anyway (defense in depth, mirroring the store-level
// check in store_test.go).
func TestRefreshOnce_CoinbaseShapedMempoolTransactionRejected(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	tip := store.Checkpoint{Height: 10, Hash: fakeHash("coinbase-in-mempool")}
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}

	txid := fakeHash("coinbase-mempool-tx")
	addr := "qCoinbaseInMempool"
	version, lockTime := uint32(1), uint32(0)
	size, vsize, weight := 100, 100, 400
	coinbaseHex := "51"
	sequence := uint32(0xffffffff)
	raw := rpc.RawTransaction{
		TxID: &txid, Hash: &txid,
		Version: &version, Size: &size, VSize: &vsize, Weight: &weight, LockTime: &lockTime,
		Vin:  []rpc.RawVin{{Coinbase: &coinbaseHex, Sequence: &sequence}},
		Vout: []rpc.RawVout{rawVout(0, 10_00000000, p2pkhScript("coinbase-in-mempool"), "pubkeyhash", &addr)},
	}
	entry := mempoolEntry(100, 400, 0, 1_700_000_000, 10, nil, false)

	client := &fakeRPCClient{
		blockCountSeq:  []int64{10},
		bestHashSeq:    []string{tip.Hash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{txid: entry},
		rawTxs:         map[string]rpc.RawTransaction{txid: raw},
	}
	s := newTestSynchronizer(client, confirmed, mstore)

	if err := s.refreshOnce(ctx); err == nil {
		t.Fatalf("refreshOnce with a coinbase-shaped mempool transaction: got nil error, want rejection")
	}
	requireNeverPublished(t, ctx, mstore)
}

// TestRefreshOnce_ChildBeforeParentLexicalOrder is the required
// synchronizer-level regression test for the dependency-ordering bug:
// fetchAndDecode itself lexically sorts txids before fetching/decoding
// them, so this drives the REAL getrawmempool -> lexical sort -> decode
// -> ReplaceSnapshot pipeline with a child transaction whose txid sorts
// BEFORE its parent's — exactly the shape that broke the old single-pass
// Store insertion.
func TestRefreshOnce_ChildBeforeParentLexicalOrder(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	childTxid := "0000000000000000000000000000000000000000000000000000000000000001"
	parentTxid := "fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe"
	if childTxid >= parentTxid {
		t.Fatalf("fixture bug: childTxid must sort before parentTxid")
	}

	childAddr := "qLexicalChild"
	parentAddr := "qLexicalParent"
	childRaw := rawMempoolTxWithTxID(childTxid,
		[]rpc.RawVin{rawSpendVin(fakeHash("lexical-child-prev"), 0, "473044")},
		[]rpc.RawVout{rawVout(0, 5_00000000, p2pkhScript("lexical-child"), "pubkeyhash", &childAddr)},
	)
	parentRaw := rawMempoolTxWithTxID(parentTxid,
		[]rpc.RawVin{rawSpendVin(fakeHash("lexical-parent-prev"), 0, "473044")},
		[]rpc.RawVout{rawVout(0, 10_00000000, p2pkhScript("lexical-parent"), "pubkeyhash", &parentAddr)},
	)
	childEntry := mempoolEntry(200, 800, 500, 1_700_000_000, 100, []string{parentTxid}, true)
	parentEntry := mempoolEntry(200, 800, 1000, 1_700_000_000, 100, nil, true)

	tip := store.Checkpoint{Height: 10, Hash: fakeHash("lexical-order-tip")}
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{10},
		bestHashSeq:   []string{tip.Hash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{
			childTxid:  childEntry,
			parentTxid: parentEntry,
		},
		rawTxs: map[string]rpc.RawTransaction{
			childTxid:  childRaw,
			parentTxid: parentRaw,
		},
	}
	s := newTestSynchronizer(client, confirmed, mstore)

	if err := s.refreshOnce(ctx); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	state, err := mstore.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.Initialized || state.Generation != 1 || state.TxCount != 2 {
		t.Fatalf("state = %+v, want initialized=true generation=1 tx_count=2", state)
	}

	var dependsOn string
	if err := pool.QueryRow(ctx, `SELECT depends_on_txid FROM mempool_dependencies WHERE txid = $1`, childTxid).Scan(&dependsOn); err != nil {
		t.Fatalf("read mempool_dependencies: %v", err)
	}
	if dependsOn != parentTxid {
		t.Fatalf("child depends_on_txid = %s, want %s", dependsOn, parentTxid)
	}
}

// TestRefreshOnce_RPCIdentityMismatchRejected is spec item 8: Core
// answering a getrawtransaction(A) request with transaction B's data must
// discard the whole candidate rather than ever attaching mempool entry
// A's fee/time/dependency metadata to decoded transaction B.
func TestRefreshOnce_RPCIdentityMismatchRejected(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	mstore := NewStore(pool)

	// First, a real successful publication, to prove it's retained.
	rawGood, entryGood := simpleRawMempoolTx("IdentityGood", 1000)
	tip := store.Checkpoint{Height: 10, Hash: fakeHash("identity-tip")}
	confirmedGood := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}
	clientGood := &fakeRPCClient{
		blockCountSeq:  []int64{10},
		bestHashSeq:    []string{tip.Hash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{*rawGood.TxID: entryGood},
		rawTxs:         map[string]rpc.RawTransaction{*rawGood.TxID: rawGood},
	}
	sGood := newTestSynchronizer(clientGood, confirmedGood, mstore)
	if err := sGood.refreshOnce(ctx); err != nil {
		t.Fatalf("initial successful refreshOnce: %v", err)
	}
	stateBefore, err := mstore.State(ctx)
	if err != nil {
		t.Fatalf("State before: %v", err)
	}

	// getrawmempool lists txid A; GetRawTransactionVerbose(A) deliberately
	// returns a structurally valid transaction that decodes to a
	// DIFFERENT txid B.
	requestedTxid := fakeHash("identity-mismatch-requested-A")
	otherAddr := "qIdentityMismatchB"
	mismatchedRaw := rawMempoolTx("identity-mismatch-actual-B",
		[]rpc.RawVin{rawSpendVin(fakeHash("identity-mismatch-prev"), 0, "473044")},
		[]rpc.RawVout{rawVout(0, 1_00000000, p2pkhScript("identity-mismatch"), "pubkeyhash", &otherAddr)},
	)
	if *mismatchedRaw.TxID == requestedTxid {
		t.Fatalf("fixture bug: mismatchedRaw.TxID must differ from requestedTxid")
	}
	entry := mempoolEntry(200, 800, 1000, 1_700_000_000, 100, nil, true)

	confirmedMismatch := &fakeConfirmedTip{seq: []store.Checkpoint{tip}}
	clientMismatch := &fakeRPCClient{
		blockCountSeq:  []int64{10},
		bestHashSeq:    []string{tip.Hash},
		mempoolEntries: map[string]rpc.RawMempoolEntry{requestedTxid: entry},
		rawTxs:         map[string]rpc.RawTransaction{requestedTxid: mismatchedRaw},
	}
	sMismatch := newTestSynchronizer(clientMismatch, confirmedMismatch, mstore)

	err = sMismatch.refreshOnce(ctx)
	if !errors.Is(err, ErrRPCIdentityMismatch) {
		t.Fatalf("refreshOnce error = %v, want ErrRPCIdentityMismatch", err)
	}

	stateAfter, err := mstore.State(ctx)
	if err != nil {
		t.Fatalf("State after: %v", err)
	}
	if stateAfter.Generation != stateBefore.Generation || stateAfter.TxCount != stateBefore.TxCount {
		t.Fatalf("state changed after an identity-mismatch response: before=%+v after=%+v", stateBefore, stateAfter)
	}
	requireMempoolTxExists(t, ctx, pool, *rawGood.TxID, true)        // previous snapshot preserved
	requireMempoolTxExists(t, ctx, pool, *mismatchedRaw.TxID, false) // never substituted in under the requested id or its own
	requireMempoolTxExists(t, ctx, pool, requestedTxid, false)
}
