package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/accounting"
	"github.com/jackc/pgx/v5"
)

// backfillAccountingAdvisoryLockKey is an arbitrary, fixed PostgreSQL
// advisory-lock key reserved for BackfillAccounting. Advisory locks are a
// global (per-database) namespace of plain integers with no built-in
// collision protection against other unrelated uses — this package uses
// exactly one key, exclusively for this purpose, so a fixed constant is
// sufficient.
const backfillAccountingAdvisoryLockKey int64 = 728341001

// ErrBackfillAlreadyRunning means another BackfillAccounting call already
// holds the advisory lock — either a concurrent process or a concurrent
// goroutine in this one. BackfillAccounting refuses to run two overlapping
// passes rather than let them race each other's reads of `blocks`/
// `transactions`/`transaction_outputs` against each other.
var ErrBackfillAlreadyRunning = errors.New("store: another backfill-accounting run holds the advisory lock")

// BackfillAccountingResult summarizes one BackfillAccounting run.
type BackfillAccountingResult struct {
	TotalBlocks int // every block considered, canonical and orphaned alike
	Inserted    int // block_accounting rows freshly written this run
	Verified    int // block_accounting rows that already existed and matched
}

// BackfillAccounting reconstructs and idempotently persists block_accounting
// for EVERY block currently in `blocks` — canonical and orphaned alike,
// since block_accounting is immutable historical metadata independent of
// current canonical status (see docs/ARCHITECTURE.md §26) — using only
// already-indexed PostgreSQL data. It never calls Core RPC.
//
// # Concurrency policy
//
// BackfillAccounting takes a session-level PostgreSQL advisory lock
// (pg_try_advisory_lock) for its entire duration, which prevents two
// overlapping BackfillAccounting runs (in this process or any other one
// sharing the database) from racing each other — returning
// ErrBackfillAlreadyRunning immediately if the lock is already held,
// rather than silently computing across a moving result set. The lock is
// released automatically if the holding connection is lost (crash,
// network partition), so a killed backfill process can never leave a
// stale lock behind.
//
// This does NOT, and cannot, serialize against a concurrently RUNNING
// `qoge-explorer index` process: the live indexer's ApplyBlock
// serializes against other ApplyBlock/RollbackTo calls via a DIFFERENT
// mechanism (lockCheckpoint's row-level lock on sync_state), which this
// read-heavy backfill deliberately does not acquire (acquiring it for the
// whole backfill duration would block live indexing for as long as the
// backfill takes, which is the opposite of what an operator running this
// against a large existing chain wants). It is an OPERATIONAL
// REQUIREMENT, not a mechanically enforced one, that `index` is stopped
// before running backfill-accounting — see cmd/qoge-explorer's backfill-
// accounting usage text and docs/ARCHITECTURE.md §26.
//
// # Idempotency and restartability
//
// Each block's accounting row is computed and persisted independently
// (BackfillBlockAccounting), through the SAME insertOrVerifyIdempotent
// path ApplyBlock itself uses (accounting.go): an already-correct row is
// verified and left alone (counted in Verified), a missing row is freshly
// inserted (counted in Inserted), and a contradictory existing row fails
// loudly with ErrImmutableConflict rather than being silently overwritten.
// Running BackfillAccounting twice therefore produces identical database
// state, and an interrupted run can simply be re-run: blocks already
// backfilled re-verify (cheaply) rather than needing a separate progress
// counter to skip them.
func (s *Store) BackfillAccounting(ctx context.Context) (BackfillAccountingResult, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return BackfillAccountingResult{}, fmt.Errorf("store: backfill accounting: acquire lock connection: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, backfillAccountingAdvisoryLockKey).Scan(&locked); err != nil {
		return BackfillAccountingResult{}, fmt.Errorf("store: backfill accounting: acquire advisory lock: %w", err)
	}
	if !locked {
		return BackfillAccountingResult{}, ErrBackfillAlreadyRunning
	}
	defer func() {
		// Best-effort: if this fails, the lock is still released when conn
		// itself is closed/returned — pg_try_advisory_lock is session-
		// scoped, not held open-endedly by a leaked reference.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, backfillAccountingAdvisoryLockKey)
	}()

	rows, err := s.pool.Query(ctx, `SELECT hash FROM blocks ORDER BY height, hash`)
	if err != nil {
		return BackfillAccountingResult{}, fmt.Errorf("store: backfill accounting: list blocks: %w", err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return BackfillAccountingResult{}, fmt.Errorf("store: backfill accounting: scan block hash: %w", err)
		}
		hashes = append(hashes, h)
	}
	if err := rows.Err(); err != nil {
		return BackfillAccountingResult{}, fmt.Errorf("store: backfill accounting: iterate blocks: %w", err)
	}

	var result BackfillAccountingResult
	for _, h := range hashes {
		fresh, err := s.BackfillBlockAccounting(ctx, h)
		if err != nil {
			return result, err
		}
		result.TotalBlocks++
		if fresh {
			result.Inserted++
		} else {
			result.Verified++
		}
	}
	return result, nil
}

// BackfillBlockAccounting reconstructs block_accounting for exactly one
// already-indexed block, using only PostgreSQL data — never Core RPC — and
// persists it through the same idempotent path applyBlockAccounting uses.
// Returns fresh=true if the row was freshly inserted, fresh=false if an
// already-existing row was verified to match (see insertOrVerifyIdempotent
// for what "match" means, and ErrImmutableConflict for what happens when it
// doesn't).
//
// Fees and the coinbase output total are both derived strictly through
// this block's OWN occurrence (block_transactions), not the global
// transactions table — the same txid can have more than one historical
// block occurrence (e.g. across a reorg an indexer already resolved before
// this backfill ever ran), and accounting belongs to a specific block
// occurrence, not a txid in the abstract (see docs/ARCHITECTURE.md §26
// "Backfill fees"/"Backfill coinbase").
func (s *Store) BackfillBlockAccounting(ctx context.Context, blockHash string) (fresh bool, err error) {
	var height int64
	if err := s.pool.QueryRow(ctx, `SELECT height FROM blocks WHERE hash = $1`, blockHash).Scan(&height); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("store: backfill accounting: no block %s", blockHash)
		}
		return false, fmt.Errorf("store: backfill accounting: read block %s: %w", blockHash, err)
	}

	var coinbaseTxid string
	var isCoinbase bool
	err = s.pool.QueryRow(ctx, `
		SELECT t.txid, t.is_coinbase
		FROM block_transactions bt
		JOIN transactions t ON t.txid = bt.txid
		WHERE bt.block_hash = $1 AND bt.tx_index = 0
	`, blockHash).Scan(&coinbaseTxid, &isCoinbase)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("store: backfill accounting: block %s has no tx_index 0 occurrence", blockHash)
	}
	if err != nil {
		return false, fmt.Errorf("store: backfill accounting: read block %s coinbase occurrence: %w", blockHash, err)
	}
	if !isCoinbase {
		return false, fmt.Errorf("store: backfill accounting: block %s tx_index 0 (%s) is not coinbase", blockHash, coinbaseTxid)
	}

	var coinbaseOutputTotal int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(value_satoshis), 0) FROM transaction_outputs WHERE txid = $1
	`, coinbaseTxid).Scan(&coinbaseOutputTotal); err != nil {
		return false, fmt.Errorf("store: backfill accounting: sum coinbase outputs for block %s: %w", blockHash, err)
	}

	var feeTotal int64
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(t.fee_satoshis), 0)
		FROM block_transactions bt
		JOIN transactions t ON t.txid = bt.txid
		WHERE bt.block_hash = $1 AND NOT t.is_coinbase
	`, blockHash).Scan(&feeTotal); err != nil {
		return false, fmt.Errorf("store: backfill accounting: sum fees for block %s: %w", blockHash, err)
	}

	facts, err := accounting.ComputeBlockFacts(blockHash, height, feeTotal, coinbaseOutputTotal)
	if err != nil {
		return false, fmt.Errorf("store: backfill accounting: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: backfill accounting: begin %s: %w", blockHash, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	fresh, err = insertOrVerifyIdempotent(ctx, tx, "block_accounting "+facts.BlockHash,
		blockAccountingInsertSQL, blockAccountingVerifySQL,
		facts.BlockHash, facts.SubsidySatoshis, facts.FeeSatoshis, facts.CoinbaseOutputSatoshis, facts.UnclaimedRewardSatoshis,
	)
	if err != nil {
		return false, fmt.Errorf("store: backfill accounting: persist %s: %w", blockHash, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: backfill accounting: commit %s: %w", blockHash, err)
	}
	return fresh, nil
}
