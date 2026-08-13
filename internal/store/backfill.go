package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// backfillAccountingAdvisoryLockNamespace is the fixed first key of the
// two-key PostgreSQL advisory lock BackfillAccounting takes
// (pg_try_advisory_lock(namespace, schema_key)). Advisory locks are a
// global (per-database, not per-schema) namespace of plain integers, so the
// namespace alone is NOT sufficient to identify which backfill run this
// is — see schemaScopedLockKey, which combines this namespace with a key
// derived from the explorer schema actually being backfilled.
const backfillAccountingAdvisoryLockNamespace int32 = 728341001

// ErrBackfillAlreadyRunning means another BackfillAccounting call already
// holds the advisory lock for the SAME explorer schema — either a
// concurrent process or a concurrent goroutine in this one. BackfillAccounting
// refuses to run two overlapping passes over the same schema rather than
// let them race each other's reads of `blocks`/`transactions`/
// `transaction_outputs` against each other. A concurrent backfill running
// against a DIFFERENT explorer schema in the same database does not hold
// this lock and is unaffected (see schemaScopedLockKey).
var ErrBackfillAlreadyRunning = errors.New("store: another backfill-accounting run holds the advisory lock for this explorer schema")

// ErrSchemaIdentityUnavailable means BackfillAccounting could not determine
// which PostgreSQL schema this Store's connection is actually operating
// against (current_schema() returned NULL/empty). The advisory lock's
// safety depends entirely on being scoped to the right schema, so
// BackfillAccounting fails closed here rather than falling back to a
// schema-independent global key or any other default.
var ErrSchemaIdentityUnavailable = errors.New("store: backfill accounting: could not determine current PostgreSQL schema")

// schemaScopedLockKey reads the schema `conn`'s session actually resolves
// unqualified names against (current_schema(), i.e. the first existing
// schema on search_path) and derives a deterministic PostgreSQL advisory
// lock key pair from it: a fixed namespace
// (backfillAccountingAdvisoryLockNamespace) plus hashtext(schema name).
//
// This makes the lock schema-scoped rather than database-global: two
// BackfillAccounting runs against the SAME schema always compute the same
// key pair and correctly exclude each other, while two runs against
// DIFFERENT schemas in the same database (this project's PostgreSQL
// package tests each use their own throwaway schema, per
// dbtest_test.go's newTestSchema) compute different key pairs and do not
// contend — matching the fact that they read and write entirely disjoint
// tables. hashtext() is deterministic for a given input across processes,
// connections, and Store instances, so no process-local randomness is
// involved.
//
// Returns ErrSchemaIdentityUnavailable if current_schema() is NULL/empty,
// rather than silently falling back to an arbitrary or schema-independent
// key.
func schemaScopedLockKey(ctx context.Context, conn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) (namespace, schemaKey int32, schemaName string, err error) {
	var name *string
	if err := conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&name); err != nil {
		return 0, 0, "", fmt.Errorf("store: backfill accounting: read current_schema: %w", err)
	}
	if name == nil || *name == "" {
		return 0, 0, "", ErrSchemaIdentityUnavailable
	}
	var key int32
	if err := conn.QueryRow(ctx, `SELECT hashtext($1)`, *name).Scan(&key); err != nil {
		return 0, 0, "", fmt.Errorf("store: backfill accounting: hash schema name: %w", err)
	}
	return backfillAccountingAdvisoryLockNamespace, key, *name, nil
}

// ErrIncompleteAccountingSource means the already-indexed PostgreSQL data
// BackfillBlockAccounting needs for one block is present but incomplete in
// a way that would otherwise silently understate that block's true fees or
// coinbase output total:
//
//   - migrations/0001_initial.up.sql permits transactions.fee_satoshis to
//     be NULL for a non-coinbase transaction; SQL SUM() silently ignores
//     NULL rows, which would turn a genuinely-unknown fee into an
//     under-counted (but plausible-looking) fee total and a correspondingly
//     inflated, WRONG unclaimed_reward_satoshis.
//   - a coinbase transaction with zero transaction_outputs rows is
//     indistinguishable, from a bare COALESCE(SUM(...), 0), from a miner
//     who legitimately claimed nothing — but the former means this
//     backfill's own source data is incomplete, not that the chain
//     recorded a zero-value coinbase.
//
// BackfillBlockAccounting refuses to guess in either case: it returns this
// error and writes no block_accounting row, rather than silently treating
// missing data as zero.
var ErrIncompleteAccountingSource = errors.New("store: backfill accounting: incomplete source data")

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
// BackfillAccounting takes a session-level, schema-scoped PostgreSQL
// advisory lock (pg_try_advisory_lock(namespace, schema_key), see
// schemaScopedLockKey) for its entire duration, which prevents two
// overlapping BackfillAccounting runs against the SAME explorer schema (in
// this process or any other one sharing the database) from racing each
// other — returning ErrBackfillAlreadyRunning immediately if the lock is
// already held, rather than silently computing across a moving result
// set. Because the lock key is derived from the schema, two
// BackfillAccounting runs against DIFFERENT explorer schemas in the same
// database (as this project's PostgreSQL-backed package tests deliberately
// use, one throwaway schema per test) do not contend with each other at
// all — they read and write entirely disjoint tables, so there is nothing
// to serialize. The lock is released automatically if the holding
// connection is lost (crash, network partition), so a killed backfill
// process can never leave a stale lock behind.
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

	namespace, schemaKey, _, err := schemaScopedLockKey(ctx, conn)
	if err != nil {
		return BackfillAccountingResult{}, fmt.Errorf("store: backfill accounting: %w", err)
	}

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, namespace, schemaKey).Scan(&locked); err != nil {
		return BackfillAccountingResult{}, fmt.Errorf("store: backfill accounting: acquire advisory lock: %w", err)
	}
	if !locked {
		return BackfillAccountingResult{}, ErrBackfillAlreadyRunning
	}
	defer func() {
		// Best-effort: if this fails, the lock is still released when conn
		// itself is closed/returned — pg_try_advisory_lock is session-
		// scoped, not held open-endedly by a leaked reference. Same
		// (namespace, schemaKey) pair used to acquire it.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, namespace, schemaKey)
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

	// count(*) alongside the SUM lets us tell "a coinbase whose outputs
	// legitimately sum to zero" apart from "no transaction_outputs rows
	// exist for this coinbase at all" (incomplete source data) — a bare
	// COALESCE(SUM(...), 0) cannot distinguish the two. See
	// ErrIncompleteAccountingSource.
	var coinbaseOutputRowCount int64
	var coinbaseOutputTotal int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(value_satoshis), 0) FROM transaction_outputs WHERE txid = $1
	`, coinbaseTxid).Scan(&coinbaseOutputRowCount, &coinbaseOutputTotal); err != nil {
		return false, fmt.Errorf("store: backfill accounting: sum coinbase outputs for block %s: %w", blockHash, err)
	}
	if coinbaseOutputRowCount == 0 {
		return false, fmt.Errorf("%w: block %s coinbase %s has zero transaction_outputs rows",
			ErrIncompleteAccountingSource, blockHash, coinbaseTxid)
	}

	// count(*) is every non-coinbase occurrence in this block;
	// count(t.fee_satoshis) is standard SQL COUNT(column) semantics — it
	// counts only the NON-NULL values. A mismatch means at least one
	// non-coinbase transaction in this block has fee_satoshis = NULL
	// (permitted by the frozen 0001 schema), which SUM() would otherwise
	// have silently treated as zero. See ErrIncompleteAccountingSource.
	var nonCoinbaseCount, nonCoinbaseWithFeeCount int64
	var feeTotal int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(t.fee_satoshis), COALESCE(SUM(t.fee_satoshis), 0)
		FROM block_transactions bt
		JOIN transactions t ON t.txid = bt.txid
		WHERE bt.block_hash = $1 AND NOT t.is_coinbase
	`, blockHash).Scan(&nonCoinbaseCount, &nonCoinbaseWithFeeCount, &feeTotal); err != nil {
		return false, fmt.Errorf("store: backfill accounting: sum fees for block %s: %w", blockHash, err)
	}
	if nonCoinbaseWithFeeCount != nonCoinbaseCount {
		return false, fmt.Errorf("%w: block %s has %d of %d non-coinbase occurrence(s) with NULL fee_satoshis",
			ErrIncompleteAccountingSource, blockHash, nonCoinbaseCount-nonCoinbaseWithFeeCount, nonCoinbaseCount)
	}

	facts, err := s.accountingSchedule.ComputeBlockFacts(blockHash, height, feeTotal, coinbaseOutputTotal)
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
