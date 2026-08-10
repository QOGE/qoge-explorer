package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// DefaultPollInterval is used when a caller of New supplies a
// non-positive pollInterval.
const DefaultPollInterval = 10 * time.Second

// Indexer coordinates already-reviewed components — rpc.Client (via the
// narrow RPCClient interface), decode.DecodeBlock, and internal/store —
// into the fixed pipeline documented in doc.go. It holds no chain-shape
// knowledge of its own: every byte of consensus/decoding/persistence logic
// belongs to the packages it calls.
type Indexer struct {
	rpc          RPCClient
	store        *store.Store
	resolver     decode.AddressResolver
	pollInterval time.Duration
	log          *slog.Logger

	// syncMu enforces "no overlapping sync passes" (task item 15/21.R):
	// SyncToTip fails fast with ErrSyncInProgress rather than blocking
	// when another pass is already running.
	syncMu sync.Mutex
}

// New builds an Indexer. pollInterval <= 0 falls back to
// DefaultPollInterval. log may be nil, in which case slog.Default() is
// used.
func New(client RPCClient, st *store.Store, resolver decode.AddressResolver, pollInterval time.Duration, log *slog.Logger) *Indexer {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Indexer{
		rpc:          client,
		store:        st,
		resolver:     resolver,
		pollInterval: pollInterval,
		log:          log,
	}
}

// SyncToTip reconciles the local Store checkpoint against Core's current
// canonical chain and, on success, has fully caught up: the local tip is
// on Core's active branch and matches the tip height Core reported at some
// point during the call (task item 14 — Core may keep advancing during a
// long historical sync; SyncToTip re-checks after reaching any previously
// observed target rather than returning early on a stale snapshot).
//
// SyncToTip never runs two passes concurrently on the same Indexer —
// ErrSyncInProgress is returned immediately if one is already running.
func (idx *Indexer) SyncToTip(ctx context.Context) error {
	if !idx.syncMu.TryLock() {
		return ErrSyncInProgress
	}
	defer idx.syncMu.Unlock()
	return idx.syncToTipLocked(ctx)
}

func (idx *Indexer) syncToTipLocked(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := idx.reconcile(ctx); err != nil {
			if errors.Is(err, ErrRemoteChainMoved) {
				continue
			}
			return err
		}

		local, err := idx.store.Tip(ctx)
		if err != nil {
			return fmt.Errorf("indexer: read local tip: %w", err)
		}

		target, err := idx.rpc.GetBlockCount(ctx)
		if err != nil {
			return fmt.Errorf("indexer: getblockcount: %w", err)
		}

		if local.Height >= target {
			return nil // caught up: on Core's active chain, at or past the observed target
		}

		idx.logSyncStart(local.Height, target)

		chainMoved, err := idx.applyRange(ctx, local.Height+1, target)
		if err != nil {
			return err
		}
		if chainMoved {
			continue // restart reconciliation from the top
		}
		// Reached target this pass; loop back to re-check whether Core
		// advanced further or changed branch since the snapshot above.
	}
}

// applyRange sequentially applies heights [from, to]. It returns
// chainMoved=true (with a nil error) if ErrRemoteChainMoved interrupted
// the range partway through — the caller restarts reconciliation rather
// than treating this as a terminal error.
func (idx *Indexer) applyRange(ctx context.Context, from, to int64) (chainMoved bool, err error) {
	total := to - from + 1
	for h := from; h <= to; h++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := idx.applyHeight(ctx, h); err != nil {
			if errors.Is(err, ErrRemoteChainMoved) {
				return true, nil
			}
			return false, err
		}
		idx.maybeLogProgress(from, h, to, total)
	}
	return false, nil
}

// Run is a thin polling loop around SyncToTip: sync/reconcile to Core's
// current tip, wait pollInterval, repeat, until ctx is cancelled. A
// deterministic decoder/store/local-integrity/deep-reorg error halts and
// is returned — Run prefers failing safely (letting a process supervisor
// restart it) over endlessly retrying an error whose meaning is unknown.
// A chain-moved race is retried immediately inside SyncToTip itself, never
// surfaced here, so Run never waits a full poll interval for that case.
func (idx *Indexer) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		if err := idx.SyncToTip(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("indexer: sync pass failed: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(idx.pollInterval):
		}
	}
}

func (idx *Indexer) logSyncStart(startHeight, target int64) {
	if startHeight >= target {
		return
	}
	idx.log.Info("historical sync starting",
		"start_height", startHeight+1,
		"target_height", target,
		"blocks_remaining", target-startHeight,
	)
}

// maybeLogProgress logs periodically (every 1000 blocks, and always on the
// final block) rather than once per block, to avoid flooding output during
// a long historical sync.
func (idx *Indexer) maybeLogProgress(from, current, target, total int64) {
	const logEvery = 1000
	done := current - from + 1
	if done%logEvery != 0 && current != target {
		return
	}
	pct := float64(0)
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}
	idx.log.Info("historical sync progress",
		"current_height", current,
		"target_height", target,
		"percent", fmt.Sprintf("%.2f", pct),
		"blocks_remaining", target-current,
	)
}
