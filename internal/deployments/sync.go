package deployments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// DefaultPollInterval mirrors internal/mempool's conservative-by-design
// default. Deployment state changes on the order of blocks/weeks, not
// seconds — sub-second polling against Core is never warranted here
// (spec item 23).
const DefaultPollInterval = 30 * time.Second

// RPCClient is the narrow set of Core RPC operations the deployment
// observer needs. *rpc.Client satisfies it in production; tests supply a
// deterministic fake instead of requiring a running qogecoind — see
// sync_test.go.
type RPCClient interface {
	GetBlockCount(ctx context.Context) (int64, error)
	GetBlockHash(ctx context.Context, height int64) (string, error)
	GetDeploymentInfo(ctx context.Context, blockHash string) (rpc.RawDeploymentInfo, error)
}

var _ RPCClient = (*rpc.Client)(nil)

// ConfirmedTipReader is the narrow read-only view of the confirmed
// PostgreSQL checkpoint the observer needs, identical in shape to
// internal/mempool.ConfirmedTipReader (duplicated rather than shared:
// this package never imports internal/mempool, and internal/mempool
// never imports this package). *store.Store satisfies it in production.
type ConfirmedTipReader interface {
	Tip(ctx context.Context) (store.Checkpoint, error)
}

// Synchronizer orchestrates one deployment snapshot acquisition/
// publication cycle against Core RPC, an isolated deployment Store, and
// (read-only) the confirmed chain's checkpoint. It never mutates
// confirmed or mempool state — see docs/ARCHITECTURE.md §24.
type Synchronizer struct {
	rpc          RPCClient
	confirmed    ConfirmedTipReader
	store        *Store
	pollInterval time.Duration
	log          *slog.Logger
}

// New builds a Synchronizer. pollInterval <= 0 falls back to
// DefaultPollInterval. log may be nil, in which case slog.Default() is
// used.
func New(client RPCClient, confirmed ConfirmedTipReader, deploymentStore *Store, pollInterval time.Duration, log *slog.Logger) *Synchronizer {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Synchronizer{
		rpc:          client,
		confirmed:    confirmed,
		store:        deploymentStore,
		pollInterval: pollInterval,
		log:          log,
	}
}

// Run polls refreshOnce every pollInterval until ctx is cancelled. Like
// mempool.Synchronizer.Run, Run never returns a fatal error: a deployment
// observation failure of any kind (RPC error, malformed response, a
// race, a DB write failure) is logged and retried on the next cycle,
// preserving whatever snapshot was last committed — deployment failures
// must never halt confirmed indexing or mempool observation (spec item
// 21). Run returns only when ctx is done, so a caller can wg.Wait() for
// clean shutdown without a goroutine leak.
func (s *Synchronizer) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		if err := s.refreshOnce(ctx); err != nil {
			s.logRefreshOutcome(err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(s.pollInterval):
		}
	}
}

func (s *Synchronizer) logRefreshOutcome(err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Shutting down; nothing to log.
	case errors.Is(err, ErrConfirmedIndexNotReady):
		s.log.Debug("deployment observation skipped: confirmed index uninitialized or behind core's active tip")
	case errors.Is(err, ErrDeploymentRace):
		s.log.Warn("deployment observation skipped: transient race during acquisition", "reason", err)
	default:
		s.log.Warn("deployment observation failed; retaining previous snapshot", "error", err)
	}
}

// refreshOnce runs exactly one acquisition/publication cycle (spec item
// 15, steps A-G):
//
//	A. read the confirmed PostgreSQL checkpoint; an uninitialized
//	   checkpoint (Height < 0) skips publication
//	B. read Core's active tip (GetBlockCount + GetBlockHash(height));
//	   require it equal the confirmed checkpoint, else skip — historical
//	   indexing continues regardless, this cycle simply publishes
//	   nothing (spec item 24)
//	C. call getdeploymentinfo against the EXPLICIT confirmed block hash
//	   (never Core's implicit moving tip); require the response's own
//	   hash/height anchor matches what was queried
//	D. strictly decode every deployment; keep only type=="bip9"
//	E/F. re-read Core's active tip and the confirmed checkpoint
//	G. require all anchors (initial confirmed tip, initial Core tip,
//	   getdeploymentinfo's own anchor, final Core tip, final confirmed
//	   tip) still agree, else discard the candidate
//
// Core and PostgreSQL can never share one atomic snapshot (spec item 16)
// — the before/after anchor checks close the practical race window, they
// do not eliminate it mathematically. A genuine mismatch here just means
// this cycle publishes nothing; the caller retries next cycle.
func (s *Synchronizer) refreshOnce(ctx context.Context) error {
	initialDBTip, err := s.confirmed.Tip(ctx)
	if err != nil {
		return fmt.Errorf("deployments: read confirmed checkpoint: %w", err)
	}

	initialCoreHeight, initialCoreHash, err := s.coreTip(ctx)
	if err != nil {
		return err
	}

	// initialDBTip.Height is -1 for an explorer that has never synced
	// (store.Checkpoint's documented zero state), which can never equal
	// Core's non-negative active-tip height — this single comparison
	// therefore also covers the "confirmed index uninitialized" case
	// (spec item 15 step A), exactly like internal/mempool.refreshOnce's
	// equivalent check.
	if initialDBTip.Height != initialCoreHeight || initialDBTip.Hash != initialCoreHash {
		return ErrConfirmedIndexNotReady
	}

	observedAt := time.Now().UTC()
	info, err := s.rpc.GetDeploymentInfo(ctx, initialDBTip.Hash)
	if err != nil {
		return fmt.Errorf("deployments: getdeploymentinfo %s: %w", initialDBTip.Hash, err)
	}

	decoded, err := DecodeDeploymentInfo(info)
	if err != nil {
		return err
	}

	// Core was explicitly asked about initialDBTip.Hash; its response
	// must self-report that SAME hash/height. A disagreement here is a
	// wire-integrity problem, not an ordinary race — treated the same as
	// a race (candidate discarded, retried later) since either way
	// publishing would anchor the snapshot incorrectly (spec item 15
	// step C).
	if decoded.Height != initialDBTip.Height || decoded.Hash != initialDBTip.Hash {
		return fmt.Errorf("%w: getdeploymentinfo response anchor %d/%s disagrees with the queried confirmed checkpoint %d/%s",
			ErrDeploymentRace, decoded.Height, decoded.Hash, initialDBTip.Height, initialDBTip.Hash)
	}

	finalCoreHeight, finalCoreHash, err := s.coreTip(ctx)
	if err != nil {
		return err
	}
	finalDBTip, err := s.confirmed.Tip(ctx)
	if err != nil {
		return fmt.Errorf("deployments: re-read confirmed checkpoint: %w", err)
	}

	if initialCoreHeight != finalCoreHeight || initialCoreHash != finalCoreHash {
		return fmt.Errorf("%w: core active tip moved during acquisition (%d/%s -> %d/%s)",
			ErrDeploymentRace, initialCoreHeight, initialCoreHash, finalCoreHeight, finalCoreHash)
	}
	if initialDBTip.Height != finalDBTip.Height || initialDBTip.Hash != finalDBTip.Hash {
		return fmt.Errorf("%w: confirmed checkpoint moved during acquisition (%d/%s -> %d/%s)",
			ErrDeploymentRace, initialDBTip.Height, initialDBTip.Hash, finalDBTip.Height, finalDBTip.Hash)
	}

	candidate := Candidate{
		CoreTipHeight: finalCoreHeight,
		CoreTipHash:   finalCoreHash,
		ObservedAt:    observedAt,
		Deployments:   decoded.Deployments,
	}

	generation, err := s.store.ReplaceSnapshot(ctx, candidate)
	if err != nil {
		return fmt.Errorf("deployments: replace snapshot: %w", err)
	}

	s.log.Info("deployment snapshot published",
		"generation", generation,
		"deployment_count", len(decoded.Deployments),
		"core_tip_height", finalCoreHeight,
	)
	return nil
}

// coreTip reads Core's current active-chain height and the hash of the
// block at that height (spec item 15 step B: GetBlockCount then
// GetBlockHash(height), not GetBestBlockHash — mirrors
// indexer.Indexer.confirmCaughtUp's height-indexed lookup style). The two
// calls are not atomic with each other (spec item 16) — this is simply
// the closest available approximation.
func (s *Synchronizer) coreTip(ctx context.Context) (height int64, hash string, err error) {
	height, err = s.rpc.GetBlockCount(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("deployments: getblockcount: %w", err)
	}
	hash, err = s.rpc.GetBlockHash(ctx, height)
	if err != nil {
		return 0, "", fmt.Errorf("deployments: getblockhash(%d): %w", height, err)
	}
	return height, hash, nil
}
