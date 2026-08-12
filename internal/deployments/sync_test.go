package deployments

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/QOGE/qoge-explorer/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestSynchronizer(rpcClient RPCClient, confirmed ConfirmedTipReader, deploymentStore *Store) *Synchronizer {
	return New(rpcClient, confirmed, deploymentStore, 0, discardLogger())
}

func requireNeverPublished(t *testing.T, ctx context.Context, dstore *Store) {
	t.Helper()
	state, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Initialized {
		t.Fatalf("state.Initialized = true, want false (never published)")
	}
}

// TestRefreshOnce_ConfirmedIndexUninitialized is spec item 32.1: an
// uninitialized confirmed checkpoint (store.Checkpoint{Height: -1}) must
// skip publication.
func TestRefreshOnce_ConfirmedIndexUninitialized(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: -1, Hash: ""}}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{10},
		blockHashSeq:  []string{fakeHash("core-tip-10")},
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrConfirmedIndexNotReady) {
		t.Fatalf("refreshOnce error = %v, want ErrConfirmedIndexNotReady", err)
	}
	requireNeverPublished(t, ctx, dstore)
}

// TestRefreshOnce_ConfirmedIndexBehindCore is spec item 32.2: the
// confirmed PostgreSQL tip height does not match Core's active tip
// height -> no publication.
func TestRefreshOnce_ConfirmedIndexBehindCore(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 5, Hash: fakeHash("db-tip-5")}}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{10}, // Core is ahead of the confirmed checkpoint
		blockHashSeq:  []string{fakeHash("core-tip-10")},
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrConfirmedIndexNotReady) {
		t.Fatalf("refreshOnce error = %v, want ErrConfirmedIndexNotReady", err)
	}
	requireNeverPublished(t, ctx, dstore)
}

// TestRefreshOnce_InitialCoreHashDiffersFromDBHash is spec item 32.3:
// same height, but Core's hash disagrees with the confirmed checkpoint's
// hash (e.g. a same-height reorg) -> no publication.
func TestRefreshOnce_InitialCoreHashDiffersFromDBHash(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 10, Hash: fakeHash("db-hash-A")}}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{10},
		blockHashSeq:  []string{fakeHash("core-hash-B")},
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrConfirmedIndexNotReady) {
		t.Fatalf("refreshOnce error = %v, want ErrConfirmedIndexNotReady", err)
	}
	requireNeverPublished(t, ctx, dstore)
}

// TestRefreshOnce_ResponseHashDiffersFromQueriedHash is spec item 32.4:
// getdeploymentinfo's own reported hash disagrees with the confirmed
// checkpoint hash it was queried against -> discard, no publication.
func TestRefreshOnce_ResponseHashDiffersFromQueriedHash(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	dbHash := fakeHash("db-tip")
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 10, Hash: dbHash}, {Height: 10, Hash: dbHash}}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{10, 10},
		blockHashSeq:  []string{dbHash, dbHash},
		deploymentInfo: deploymentInfoResponse(fakeHash("wrong-hash"), 10, map[string]json.RawMessage{
			"p2qpk": p2qpkStartedFixture(),
		}),
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrDeploymentRace) {
		t.Fatalf("refreshOnce error = %v, want ErrDeploymentRace", err)
	}
	requireNeverPublished(t, ctx, dstore)
}

// TestRefreshOnce_ResponseHeightDiffersFromQueriedHeight is spec item
// 32.5.
func TestRefreshOnce_ResponseHeightDiffersFromQueriedHeight(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	dbHash := fakeHash("db-tip")
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 10, Hash: dbHash}, {Height: 10, Hash: dbHash}}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{10, 10},
		blockHashSeq:  []string{dbHash, dbHash},
		deploymentInfo: deploymentInfoResponse(dbHash, 99, map[string]json.RawMessage{
			"p2qpk": p2qpkStartedFixture(),
		}),
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrDeploymentRace) {
		t.Fatalf("refreshOnce error = %v, want ErrDeploymentRace", err)
	}
	requireNeverPublished(t, ctx, dstore)
}

// TestRefreshOnce_CoreTipMovedDuringAcquisition is spec item 32.6.
func TestRefreshOnce_CoreTipMovedDuringAcquisition(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	dbHash := fakeHash("db-tip")
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 10, Hash: dbHash}}} // same both times
	client := &fakeRPCClient{
		blockCountSeq: []int64{10, 11}, // moved between initial and final read
		blockHashSeq:  []string{dbHash, fakeHash("core-tip-moved")},
		deploymentInfo: deploymentInfoResponse(dbHash, 10, map[string]json.RawMessage{
			"p2qpk": p2qpkStartedFixture(),
		}),
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrDeploymentRace) {
		t.Fatalf("refreshOnce error = %v, want ErrDeploymentRace", err)
	}
	requireNeverPublished(t, ctx, dstore)
}

// TestRefreshOnce_DBTipMovedDuringAcquisition is spec item 32.7.
func TestRefreshOnce_DBTipMovedDuringAcquisition(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	dbHash := fakeHash("db-tip-move")
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{
		{Height: 10, Hash: dbHash},
		{Height: 11, Hash: fakeHash("db-tip-moved-on")},
	}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{10, 10}, // Core tip stays put
		blockHashSeq:  []string{dbHash, dbHash},
		deploymentInfo: deploymentInfoResponse(dbHash, 10, map[string]json.RawMessage{
			"p2qpk": p2qpkStartedFixture(),
		}),
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	err := s.refreshOnce(ctx)
	if !errors.Is(err, ErrDeploymentRace) {
		t.Fatalf("refreshOnce error = %v, want ErrDeploymentRace", err)
	}
	requireNeverPublished(t, ctx, dstore)
}

// TestRefreshOnce_TemporaryRPCErrorRetainsPreviousSnapshot is spec item
// 32.8: a temporary getdeploymentinfo RPC failure must retry-later
// without publishing, leaving whatever was previously committed intact.
func TestRefreshOnce_TemporaryRPCErrorRetainsPreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	goodHash := fakeHash("good-tip")
	goodGen, err := dstore.ReplaceSnapshot(ctx, candidateFor(5, goodHash, fixedTime(),
		p2qpkDeployment("started", 0, p2qpkStartedFixture())))
	if err != nil {
		t.Fatalf("seed ReplaceSnapshot: %v", err)
	}

	nextHash := fakeHash("next-tip")
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 6, Hash: nextHash}}}
	client := &fakeRPCClient{
		blockCountSeq:     []int64{6},
		blockHashSeq:      []string{nextHash},
		deploymentInfoErr: errors.New("simulated rpc failure: connection refused"),
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	if err := s.refreshOnce(ctx); err == nil {
		t.Fatal("refreshOnce: expected error from simulated RPC failure, got nil")
	}

	state, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Generation != goodGen {
		t.Errorf("Generation = %d after RPC failure, want unchanged %d", state.Generation, goodGen)
	}
	if state.CoreTipHash == nil || *state.CoreTipHash != goodHash {
		t.Errorf("CoreTipHash = %v after RPC failure, want unchanged %s", state.CoreTipHash, goodHash)
	}
}

// TestRefreshOnce_MalformedDeploymentObjectRetainsPreviousSnapshot is
// spec item 32.9: a malformed deployment object in Core's response must
// discard the whole candidate without publishing, leaving the previous
// snapshot intact.
func TestRefreshOnce_MalformedDeploymentObjectRetainsPreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	goodHash := fakeHash("good-tip-2")
	goodGen, err := dstore.ReplaceSnapshot(ctx, candidateFor(5, goodHash, fixedTime(),
		p2qpkDeployment("started", 0, p2qpkStartedFixture())))
	if err != nil {
		t.Fatalf("seed ReplaceSnapshot: %v", err)
	}

	nextHash := fakeHash("next-tip-2")
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 6, Hash: nextHash}}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{6},
		blockHashSeq:  []string{nextHash},
		deploymentInfo: deploymentInfoResponse(nextHash, 6, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("weird_unrecognized_status", "started", 0, nil, nil, nil),
		}),
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	if err := s.refreshOnce(ctx); err == nil {
		t.Fatal("refreshOnce: expected error from malformed deployment object, got nil")
	}

	state, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Generation != goodGen {
		t.Errorf("Generation = %d after malformed response, want unchanged %d", state.Generation, goodGen)
	}
}

// TestRefreshOnce_HappyPathPublishes proves the acquisition algorithm
// actually succeeds (not just every failure branch) when every anchor
// agrees: initial/final Core tip, initial/final confirmed tip, and
// getdeploymentinfo's own response anchor all match.
func TestRefreshOnce_HappyPathPublishes(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	tipHash := fakeHash("happy-tip")
	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: 42, Hash: tipHash}}}
	client := &fakeRPCClient{
		blockCountSeq: []int64{42},
		blockHashSeq:  []string{tipHash},
		deploymentInfo: deploymentInfoResponse(tipHash, 42, map[string]json.RawMessage{
			"p2qpk":  p2qpkStartedFixture(),
			"segwit": buriedFixture(true, i64Ptr(400_000)),
		}),
	}
	s := newTestSynchronizer(client, confirmed, dstore)

	if err := s.refreshOnce(ctx); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	state, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.Initialized {
		t.Fatal("Initialized = false after happy-path refreshOnce, want true")
	}
	if state.Generation != 1 {
		t.Errorf("Generation = %d, want 1", state.Generation)
	}
	if state.DeploymentCount != 1 {
		t.Errorf("DeploymentCount = %d, want 1 (buried must not be persisted)", state.DeploymentCount)
	}
	if state.CoreTipHeight == nil || *state.CoreTipHeight != 42 {
		t.Errorf("CoreTipHeight = %v, want 42", state.CoreTipHeight)
	}
}

// TestSynchronizer_RunStopsOnContextCancellation proves Run returns
// promptly on context cancellation rather than leaking a goroutine (spec
// item 22).
func TestSynchronizer_RunStopsOnContextCancellation(t *testing.T) {
	dstore, _, _, _ := newTestStores(t)

	confirmed := &fakeConfirmedTip{seq: []store.Checkpoint{{Height: -1, Hash: ""}}}
	client := &fakeRPCClient{blockCountSeq: []int64{0}, blockHashSeq: []string{fakeHash("x")}}
	s := New(client, confirmed, dstore, time.Hour, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancellation")
	}
}
