package mempool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// DefaultPollInterval is used when a caller of New supplies a
// non-positive pollInterval. Conservative on purpose (spec item 31): no
// sub-second polling against Core.
const DefaultPollInterval = 10 * time.Second

// RPCClient is the narrow set of Core RPC operations the mempool
// synchronizer needs. *rpc.Client satisfies it in production; tests
// supply a deterministic fake instead of requiring a running qogecoind —
// see sync_test.go.
type RPCClient interface {
	GetBlockCount(ctx context.Context) (int64, error)
	GetBestBlockHash(ctx context.Context) (string, error)
	GetRawMempoolVerbose(ctx context.Context) (map[string]rpc.RawMempoolEntry, error)
	GetRawTransactionVerbose(ctx context.Context, txid string) (rpc.RawTransaction, error)
}

var _ RPCClient = (*rpc.Client)(nil)

// ConfirmedTipReader is the narrow read-only view of the confirmed
// PostgreSQL checkpoint the synchronizer needs, to require the confirmed
// index has actually caught up to Core's active tip before publishing a
// mempool snapshot (spec item 20 step C). *store.Store satisfies it in
// production.
type ConfirmedTipReader interface {
	Tip(ctx context.Context) (store.Checkpoint, error)
}

// Synchronizer orchestrates one mempool snapshot acquisition/publication
// cycle against Core RPC, an isolated mempool Store, and (read-only) the
// confirmed chain's checkpoint. It never mutates confirmed state — see
// docs/ARCHITECTURE.md §22.
type Synchronizer struct {
	rpc          RPCClient
	confirmed    ConfirmedTipReader
	store        *Store
	resolver     decode.AddressResolver
	pollInterval time.Duration
	log          *slog.Logger
}

// New builds a Synchronizer. pollInterval <= 0 falls back to
// DefaultPollInterval. log may be nil, in which case slog.Default() is
// used.
func New(client RPCClient, confirmed ConfirmedTipReader, mempoolStore *Store, resolver decode.AddressResolver, pollInterval time.Duration, log *slog.Logger) *Synchronizer {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Synchronizer{
		rpc:          client,
		confirmed:    confirmed,
		store:        mempoolStore,
		resolver:     resolver,
		pollInterval: pollInterval,
		log:          log,
	}
}

// Run polls refreshOnce every pollInterval until ctx is cancelled. Unlike
// indexer.Indexer.Run, Run never returns a fatal error: a mempool refresh
// failure of any kind (RPC error, race, malformed candidate) is logged
// and retried on the next cycle, preserving whatever snapshot was last
// committed — mempool failures must never halt confirmed indexing (spec
// items 28/29). Run returns only when ctx is done, so a caller can
// wg.Wait() for clean shutdown without a goroutine leak.
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
	case errors.Is(err, ErrConfirmedIndexBehind):
		s.log.Debug("mempool refresh skipped: confirmed index behind core's active tip")
	case errors.Is(err, ErrMempoolRace):
		s.log.Warn("mempool refresh skipped: transient race during acquisition", "reason", err)
	default:
		s.log.Warn("mempool refresh failed; retaining previous snapshot", "error", err)
	}
}

// refreshOnce runs exactly one acquisition/publication cycle (spec item
// 20, steps A-I):
//
//	A. read confirmed PostgreSQL checkpoint
//	B. read Core active-chain tip
//	C. require confirmed tip == Core tip, else ErrConfirmedIndexBehind
//	D. getrawmempool true
//	E/F. fetch + strictly decode every listed transaction
//	G/H. re-read Core tip and confirmed checkpoint
//	     require all four anchors (initial/final Core tip, initial/final
//	     DB tip) still agree, else ErrMempoolRace
//	I. atomically replace the mempool snapshot
//
// Core and PostgreSQL can never share one atomic snapshot (spec item 21)
// — the before/after anchor checks close the practical race window, they
// do not eliminate it mathematically. A genuine mismatch here just means
// this cycle publishes nothing; the caller retries next cycle.
func (s *Synchronizer) refreshOnce(ctx context.Context) error {
	initialDBTip, err := s.confirmed.Tip(ctx)
	if err != nil {
		return fmt.Errorf("mempool: read confirmed checkpoint: %w", err)
	}

	initialCoreHeight, initialCoreHash, err := s.coreTip(ctx)
	if err != nil {
		return err
	}

	if initialDBTip.Height != initialCoreHeight || initialDBTip.Hash != initialCoreHash {
		return ErrConfirmedIndexBehind
	}

	entries, err := s.rpc.GetRawMempoolVerbose(ctx)
	if err != nil {
		return fmt.Errorf("mempool: getrawmempool true: %w", err)
	}

	candidateTxs, err := s.fetchAndDecode(ctx, entries)
	if err != nil {
		return err
	}

	finalCoreHeight, finalCoreHash, err := s.coreTip(ctx)
	if err != nil {
		return err
	}
	finalDBTip, err := s.confirmed.Tip(ctx)
	if err != nil {
		return fmt.Errorf("mempool: re-read confirmed checkpoint: %w", err)
	}

	if initialCoreHeight != finalCoreHeight || initialCoreHash != finalCoreHash {
		return fmt.Errorf("%w: core active tip moved during acquisition (%d/%s -> %d/%s)",
			ErrMempoolRace, initialCoreHeight, initialCoreHash, finalCoreHeight, finalCoreHash)
	}
	if initialDBTip.Height != finalDBTip.Height || initialDBTip.Hash != finalDBTip.Hash {
		return fmt.Errorf("%w: confirmed checkpoint moved during acquisition (%d/%s -> %d/%s)",
			ErrMempoolRace, initialDBTip.Height, initialDBTip.Hash, finalDBTip.Height, finalDBTip.Hash)
	}

	candidate := Candidate{
		CoreTipHeight: finalCoreHeight,
		CoreTipHash:   finalCoreHash,
		Transactions:  candidateTxs,
	}

	generation, err := s.store.ReplaceSnapshot(ctx, candidate)
	if err != nil {
		return fmt.Errorf("mempool: replace snapshot: %w", err)
	}

	var totalVSize int
	var totalFee int64
	for _, txn := range candidateTxs {
		totalVSize += txn.VSize
		totalFee += txn.FeeSatoshis
	}
	s.log.Info("mempool snapshot published",
		"generation", generation,
		"tx_count", len(candidateTxs),
		"total_vsize", totalVSize,
		"total_fee_satoshis", totalFee,
		"core_tip_height", finalCoreHeight,
	)
	return nil
}

// coreTip reads Core's current active-chain height and hash. The two
// calls are not atomic with each other (spec item 21) — this is simply
// the closest available approximation, exactly like
// indexer.Indexer.confirmCaughtUp's use of a single GetBlockHash call.
func (s *Synchronizer) coreTip(ctx context.Context) (height int64, hash string, err error) {
	height, err = s.rpc.GetBlockCount(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("mempool: getblockcount: %w", err)
	}
	hash, err = s.rpc.GetBestBlockHash(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("mempool: getbestblockhash: %w", err)
	}
	return height, hash, nil
}

// fetchAndDecode fetches the full verbose transaction for every txid in
// entries and strictly decodes it, sequentially (spec item 32: no
// unbounded per-transaction goroutines). A transaction that has become
// confirmed (raw.BlockHash != nil) or has disappeared (RPC error) between
// listing and fetching is a normal mempool race, not corruption — this
// whole candidate is discarded via ErrMempoolRace so the caller retries
// on a later, more consistent cycle (spec item 18).
func (s *Synchronizer) fetchAndDecode(ctx context.Context, entries map[string]rpc.RawMempoolEntry) ([]CandidateTransaction, error) {
	txids := make([]string, 0, len(entries))
	for txid := range entries {
		txids = append(txids, txid)
	}
	sort.Strings(txids) // deterministic order; not consensus-meaningful

	candidateTxs := make([]CandidateTransaction, 0, len(txids))
	for _, txid := range txids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		entry := entries[txid]

		raw, err := s.rpc.GetRawTransactionVerbose(ctx, txid)
		if err != nil {
			return nil, fmt.Errorf("%w: transaction %s disappeared or became unavailable during fetch: %v", ErrMempoolRace, txid, err)
		}
		if raw.BlockHash != nil {
			return nil, fmt.Errorf("%w: transaction %s was confirmed (block %s) during fetch", ErrMempoolRace, txid, *raw.BlockHash)
		}

		txn, err := decode.DecodeTransaction(ctx, raw, s.resolver)
		if err != nil {
			return nil, fmt.Errorf("mempool: decode transaction %s: %w", txid, err)
		}
		if txn.IsCoinbase {
			return nil, fmt.Errorf("mempool: transaction %s is coinbase-shaped; Core's mempool must never report a coinbase transaction", txid)
		}

		if entry.Fees == nil {
			return nil, fmt.Errorf("mempool: transaction %s: getrawmempool entry missing required field %q", txid, "fees")
		}
		fee, err := decode.DecodeAmount(entry.Fees.Base)
		if err != nil {
			return nil, fmt.Errorf("mempool: transaction %s: decode fee: %w", txid, err)
		}
		entryTime, err := requireInt64(entry.Time, txid, "time")
		if err != nil {
			return nil, err
		}

		candidateTxs = append(candidateTxs, CandidateTransaction{
			Transaction: txn,
			FeeSatoshis: fee.Satoshis(),
			EntryTime:   entryTime,
			EntryHeight: entry.Height,
			Replaceable: entry.BIP125Replaceable,
			Depends:     entry.Depends,
		})
	}

	return candidateTxs, nil
}

func requireInt64(p *int64, txid, field string) (int64, error) {
	if p == nil {
		return 0, fmt.Errorf("mempool: transaction %s: getrawmempool entry missing required field %q", txid, field)
	}
	return *p, nil
}
