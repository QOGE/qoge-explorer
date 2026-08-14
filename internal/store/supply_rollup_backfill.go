package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// backfillSupplyRollupAdvisoryLockNamespace is a namespace DISTINCT from
// backfillAccountingAdvisoryLockNamespace (backfill.go), so a concurrent
// backfill-accounting run and a concurrent backfill-supply-rollup run
// against the same schema do not contend for the SAME advisory lock key —
// only two backfill-supply-rollup runs against the SAME schema do (see
// schemaScopedLockKey, reused verbatim from backfill.go).
//
// This does NOT mean the two commands are independent of each other's
// DATA: backfill-supply-rollup READS block_accounting (backfill-accounting
// WRITES it), so a supply-rollup backfill genuinely depends on
// backfill-accounting having already completed for every canonical block —
// this dependency is enforced by BackfillSupplyRollup's own preflight
// (ErrSupplyRollupSourceIncomplete below), not by the lock. Using separate
// lock namespaces instead of one shared lock remains safe specifically
// because: block_accounting rows are immutable once written (never
// UPDATEd — §26), so a supply-rollup backfill running concurrently with an
// UNRELATED backfill-accounting pass over a DIFFERENT set of blocks cannot
// observe a row change mid-read; and if backfill-accounting's pass is
// still incomplete for the canonical set this command needs, the
// source-completeness preflight fails closed and nothing is published —
// never a half-computed rollup silently built from a partially-backfilled
// accounting table.
const backfillSupplyRollupAdvisoryLockNamespace int32 = 728341002

// ErrSupplyRollupBackfillAlreadyRunning means another BackfillSupplyRollup
// call already holds the advisory lock for the SAME explorer schema.
var ErrSupplyRollupBackfillAlreadyRunning = errors.New("store: another backfill-supply-rollup run holds the advisory lock for this explorer schema")

// ErrSupplyRollupCanonicalShapeInvalid means the canonical block set does
// not exactly match sync_state's implied range [0, indexed_height] —
// canonical count, minimum canonical height, or maximum canonical height
// disagrees. Unlike the live ApplyBlock path (which only ever sees one
// block at a time and structurally cannot produce this), a backfill reads
// the WHOLE canonical set up front, so this scan is a cheap, one-time,
// operator-path integrity gate (task item 29) — never run against the hot
// public read path.
var ErrSupplyRollupCanonicalShapeInvalid = errors.New("store: backfill supply rollup: canonical chain shape does not match sync_state")

// ErrSupplyRollupCanonicalAncestryInvalid means the canonical block SET has
// the right shape (count/min/max — ErrSupplyRollupCanonicalShapeInvalid
// above) but is NOT one single parent-linked chain: some canonical block's
// prev_hash does not equal the canonical block's hash at the immediately
// lower height (or, for height 0, is not NULL). The height-ordered shape
// check alone cannot catch this — a set of canonical blocks at heights
// 0..N could, in principle, disagree with each other about ancestry while
// still satisfying count==N+1, min==0, max==N (e.g. a stray prev_hash
// pointing at an orphaned block instead of the actual canonical
// predecessor). supplyRollupStagingSQL's SUM(...) OVER (ORDER BY height)
// silently assumes the canonical height sequence IS the one valid
// parent-linked chain — this preflight mechanically proves that assumption
// before any window aggregation runs, rather than trusting it.
var ErrSupplyRollupCanonicalAncestryInvalid = errors.New("store: backfill supply rollup: canonical chain ancestry is not a single parent-linked chain")

// ErrSupplyRollupSourceIncomplete means at least one canonical block has no
// block_accounting row — BackfillSupplyRollup refuses to guess a missing
// per-block fact as zero (same fail-closed policy as
// ErrIncompleteAccountingSource in backfill.go). Run `backfill-accounting`
// first.
var ErrSupplyRollupSourceIncomplete = errors.New("store: backfill supply rollup: one or more canonical blocks are missing a block_accounting row")

// ErrSupplyRollupCrossCheckFailed means the tip cumulative values
// BackfillSupplyRollup just computed via the set-based window aggregation
// do NOT match an INDEPENDENTLY computed full scan (direct utxo_state
// aggregate, or direct block_accounting aggregate) — task items 31/32.
// BackfillSupplyRollup refuses to publish ANY rollup row in this case (the
// whole transaction is rolled back, task item 30): a wrong rollup silently
// served to a future public reader would be worse than no rollup at all.
var ErrSupplyRollupCrossCheckFailed = errors.New("store: backfill supply rollup: independently computed cross-check does not match the freshly built rollup — refusing to publish")

// SupplyRollupBackfillResult summarizes one successful BackfillSupplyRollup
// run. The zero value (CanonicalBlocks == 0) with a nil error means
// sync_state was still uninitialized — nothing to backfill yet.
type SupplyRollupBackfillResult struct {
	CanonicalBlocks int64  // every canonical block considered
	Inserted        int64  // block_supply_rollup rows freshly written this run
	Verified        int64  // rows that already existed and matched
	TipHash         string // canonical tip's block hash (empty if CanonicalBlocks == 0)
	TipHeight       int64  // canonical tip's height (0 if CanonicalBlocks == 0, ambiguous with a real height-0-only chain — check CanonicalBlocks first)
}

// BackfillSupplyRollup reconstructs and publishes block_supply_rollup for
// the ENTIRE current canonical chain (genesis through sync_state's tip —
// task item 25; orphaned blocks are never backfilled here, only ever
// gaining a rollup via live re-promotion through Store.ApplyBlock), using
// only already-indexed PostgreSQL data. It never calls Core RPC and never
// recomputes the subsidy schedule — every monetary fact it uses was already
// computed once, by Store.ApplyBlock or BackfillAccounting, into
// block_accounting (task item 14/26 — no second fee/subsidy algorithm).
//
// # Atomicity and false-data protection (task item 30)
//
// The ENTIRE operation — shape/completeness preflight, the set-based
// cumulative computation, the insert, and both independent cross-checks —
// runs inside ONE PostgreSQL transaction. If anything fails at any point,
// the whole transaction rolls back and NOT ONE row is published; a partial
// or wrong rollup can never become visible to a concurrent reader. This is
// option "A" from the task spec (build + validate + persist in one
// transaction) rather than a separate staging-then-publish transaction
// pair — simpler, and sufficient because this whole command is meant to
// run with indexing stopped (see cmd/qoge-explorer's backfill-supply-rollup
// usage text), so there is no concurrent writer to race against for the
// transaction's duration.
//
// # Concurrency (task item 34)
//
// Uses pg_try_advisory_xact_lock (transaction-scoped: automatically
// released at commit or rollback, no manual unlock bookkeeping needed) with
// a schema-scoped key from schemaScopedLockKey (backfill.go) under a
// DIFFERENT namespace than backfill-accounting's lock — the two commands
// don't contend for the same LOCK, but backfill-supply-rollup genuinely
// READS block_accounting (which backfill-accounting writes); see
// backfillSupplyRollupAdvisoryLockNamespace's doc comment for exactly why
// separate lock namespaces remain safe despite that data dependency. Two
// backfill-supply-rollup runs against the SAME schema are mechanically
// excluded by this lock; runs against different schemas are independent.
//
// # Preflight (task item 29)
//
//  1. If sync_state is uninitialized (indexed_height == -1): nothing to do,
//     returns a zero SupplyRollupBackfillResult and nil error.
//  2. Otherwise, the canonical block set must be EXACTLY [0, indexed_height]
//     — count == indexed_height+1, min height == 0, max height ==
//     indexed_height (the same three-condition proof used by
//     docs/ARCHITECTURE.md §27's canonical-anchor discussion; count alone
//     is insufficient — a {1,2,3} canonical set with indexed_height=2 has
//     count=3=want but min=1). This full-chain scan is acceptable ONLY
//     here, on this operator/backfill path — never on the hot public read
//     path (task item 29).
//  3. The canonical block set must also be exactly ONE parent-linked
//     chain, not merely height-contiguous: genesis's prev_hash is NULL,
//     and every other canonical block's prev_hash equals the canonical
//     block's hash at height-1 (findCanonicalAncestryBreach,
//     ErrSupplyRollupCanonicalAncestryInvalid). The shape check above
//     proves the canonical set occupies exactly [0, indexed_height]; this
//     check additionally proves those blocks actually agree with each
//     other about ancestry — a height-contiguous set could otherwise
//     satisfy the shape check while one block's prev_hash points at an
//     orphan instead of its true canonical predecessor, which the
//     window-function computation below would silently aggregate across
//     as if it were a single valid chain.
//  4. Every canonical block must already have a block_accounting row (run
//     `backfill-accounting` first if not).
//
// # Set-based computation (task item 26/27/28)
//
// A single window-function query (supplyRollupStagingSQL) computes every
// canonical block's cumulative values in one pass — SUM(...) OVER (ORDER BY
// height) — materialized into a transaction-scoped TEMP TABLE, never one
// Go/SQL round trip per block. Per-block excluded-output value is computed
// by joining blocks -> block_transactions -> transaction_outputs (task item
// 27 — NOT a global per-txid attribution, since a txid can have more than
// one historical block occurrence), filtered to canonical block occurrences
// only, mirroring script.IsUnspendable exactly: height 0 excludes
// everything; height > 0 excludes an output iff its scriptPubKey is empty
// of a leading OP_RETURN (raw byte 0x6a) or longer than
// script.MaxScriptSize (task item 28 — raw scriptPubKey bytes, never
// script_type).
//
// cumulative_utxo_set_value is derived by subtraction
// (coinbase - fee - excluded), the same UTXO algebra applySupplyRollup uses
// (supply_rollup.go) — never accumulated independently — so the UTXO
// identity holds by construction here too.
//
// # Idempotency (task item 33)
//
// Already-existing rows are compared against the freshly computed values
// (supplyRollupContradictionSQL); any mismatch aborts the WHOLE run with
// ErrImmutableConflict before anything is inserted. If none mismatch, the
// insert (ON CONFLICT (block_hash) DO NOTHING) fills exactly the missing
// rows and leaves already-correct ones untouched — a repeated run is a safe
// no-op once fully backfilled.
//
// # Cross-checks (task item 31/32)
//
// Before committing, the tip's freshly computed cumulative UTXO value is
// compared against an INDEPENDENT direct full scan of utxo_state joined to
// transaction_outputs (the same expensive query
// internal/query.utxoSetValueFrom used before this phase's architecture
// pivot — acceptable ONCE here, never per HTTP request), and the tip's
// cumulative monetary totals are compared against an independent direct
// SUM over canonical block_accounting. Either mismatch is
// ErrSupplyRollupCrossCheckFailed and aborts the whole transaction — see
// "Atomicity" above.
func BackfillSupplyRollup(ctx context.Context, pool *pgxpool.Pool) (SupplyRollupBackfillResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	schemaKey, _, err := schemaScopedLockKey(ctx, tx)
	if err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: %w", err)
	}
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1, $2)`, backfillSupplyRollupAdvisoryLockNamespace, schemaKey).Scan(&locked); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: acquire advisory lock: %w", err)
	}
	if !locked {
		return SupplyRollupBackfillResult{}, ErrSupplyRollupBackfillAlreadyRunning
	}

	var indexedHeight int64
	if err := tx.QueryRow(ctx, `SELECT indexed_height FROM sync_state WHERE name = 'main'`).Scan(&indexedHeight); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: read sync_state: %w", err)
	}
	if indexedHeight == -1 {
		if err := tx.Commit(ctx); err != nil {
			return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: commit (nothing to do): %w", err)
		}
		return SupplyRollupBackfillResult{}, nil
	}

	var canonicalCount int64
	var minHeight, maxHeight *int64
	if err := tx.QueryRow(ctx, `SELECT count(*), min(height), max(height) FROM blocks WHERE canonical`).Scan(&canonicalCount, &minHeight, &maxHeight); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: canonical shape scan: %w", err)
	}
	if want := indexedHeight + 1; canonicalCount != want {
		return SupplyRollupBackfillResult{}, fmt.Errorf("%w: canonical block count = %d, want %d (indexed_height+1)",
			ErrSupplyRollupCanonicalShapeInvalid, canonicalCount, want)
	}
	if minHeight == nil || *minHeight != 0 {
		return SupplyRollupBackfillResult{}, fmt.Errorf("%w: minimum canonical height = %s, want 0",
			ErrSupplyRollupCanonicalShapeInvalid, formatHeightPtr(minHeight))
	}
	if maxHeight == nil || *maxHeight != indexedHeight {
		return SupplyRollupBackfillResult{}, fmt.Errorf("%w: maximum canonical height = %s, want %d (indexed_height)",
			ErrSupplyRollupCanonicalShapeInvalid, formatHeightPtr(maxHeight), indexedHeight)
	}

	if breach, err := findCanonicalAncestryBreach(ctx, tx); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: canonical ancestry scan: %w", err)
	} else if breach != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("%w: child %s (height %d) has prev_hash %s, want canonical parent %s",
			ErrSupplyRollupCanonicalAncestryInvalid, breach.childHash, breach.childHeight,
			formatNullableHash(breach.observedPrevHash), formatNullableHash(breach.expectedPrevHash))
	}

	var missingAccounting int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM blocks b
		LEFT JOIN block_accounting ba ON ba.block_hash = b.hash
		WHERE b.canonical AND ba.block_hash IS NULL
	`).Scan(&missingAccounting); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: accounting completeness scan: %w", err)
	}
	if missingAccounting != 0 {
		return SupplyRollupBackfillResult{}, fmt.Errorf("%w: %d canonical block(s) missing block_accounting",
			ErrSupplyRollupSourceIncomplete, missingAccounting)
	}

	if _, err := tx.Exec(ctx, supplyRollupStagingSQL); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: build staging: %w", err)
	}

	var contradictions int64
	if err := tx.QueryRow(ctx, supplyRollupContradictionSQL).Scan(&contradictions); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: contradiction check: %w", err)
	}
	if contradictions != 0 {
		return SupplyRollupBackfillResult{}, fmt.Errorf("%w: %d existing block_supply_rollup row(s) disagree with the freshly computed values",
			ErrImmutableConflict, contradictions)
	}

	insertTag, err := tx.Exec(ctx, supplyRollupInsertFromStagingSQL)
	if err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: insert: %w", err)
	}
	inserted := insertTag.RowsAffected()

	var tipHash string
	var tipHeight, cumSubsidy, cumFee, cumCoinbase, cumUnclaimed, cumUTXO int64
	if err := tx.QueryRow(ctx, `
		SELECT hash, height, cum_subsidy, cum_fee, cum_coinbase, cum_unclaimed, cum_utxo
		FROM supply_rollup_staging ORDER BY height DESC LIMIT 1
	`).Scan(&tipHash, &tipHeight, &cumSubsidy, &cumFee, &cumCoinbase, &cumUnclaimed, &cumUTXO); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: read tip from staging: %w", err)
	}

	var directUTXO int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(o.value_satoshis), 0)
		FROM utxo_state u JOIN transaction_outputs o ON o.txid = u.txid AND o.vout_index = u.vout_index
		WHERE NOT u.spent
	`).Scan(&directUTXO); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: direct utxo cross-check scan: %w", err)
	}
	if directUTXO != cumUTXO {
		return SupplyRollupBackfillResult{}, fmt.Errorf("%w: tip cumulative utxo value %d != direct utxo_state aggregate %d",
			ErrSupplyRollupCrossCheckFailed, cumUTXO, directUTXO)
	}

	var directSubsidy, directFee, directCoinbase, directUnclaimed int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(ba.subsidy_satoshis),0), COALESCE(SUM(ba.fee_satoshis),0),
		       COALESCE(SUM(ba.coinbase_output_satoshis),0), COALESCE(SUM(ba.unclaimed_reward_satoshis),0)
		FROM block_accounting ba JOIN blocks b ON b.hash = ba.block_hash
		WHERE b.canonical
	`).Scan(&directSubsidy, &directFee, &directCoinbase, &directUnclaimed); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: direct accounting cross-check scan: %w", err)
	}
	if directSubsidy != cumSubsidy || directFee != cumFee || directCoinbase != cumCoinbase || directUnclaimed != cumUnclaimed {
		return SupplyRollupBackfillResult{}, fmt.Errorf(
			"%w: tip cumulative accounting (subsidy=%d fee=%d coinbase=%d unclaimed=%d) != direct block_accounting aggregate (subsidy=%d fee=%d coinbase=%d unclaimed=%d)",
			ErrSupplyRollupCrossCheckFailed, cumSubsidy, cumFee, cumCoinbase, cumUnclaimed, directSubsidy, directFee, directCoinbase, directUnclaimed,
		)
	}

	if err := tx.Commit(ctx); err != nil {
		return SupplyRollupBackfillResult{}, fmt.Errorf("store: backfill supply rollup: commit: %w", err)
	}

	return SupplyRollupBackfillResult{
		CanonicalBlocks: canonicalCount,
		Inserted:        inserted,
		Verified:        canonicalCount - inserted,
		TipHash:         tipHash,
		TipHeight:       tipHeight,
	}, nil
}

// formatHeightPtr renders a nullable height for an error message — "none"
// for nil rather than a bare "<nil>".
func formatHeightPtr(h *int64) string {
	if h == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *h)
}

// formatNullableHash renders a nullable hash for an error message — "NULL"
// for nil (genesis's expected/observed prev_hash) rather than a bare
// "<nil>".
func formatNullableHash(h *string) string {
	if h == nil {
		return "NULL"
	}
	return *h
}

// ancestryBreach describes the first canonical block found whose prev_hash
// does not match its canonical predecessor (see
// ErrSupplyRollupCanonicalAncestryInvalid).
type ancestryBreach struct {
	childHash        string
	childHeight      int64
	observedPrevHash *string
	expectedPrevHash *string
}

// findCanonicalAncestryBreach proves the canonical block set forms exactly
// ONE parent-linked chain, not merely a height-contiguous set (task item 2
// of the ancestry correction): genesis (height 0) must have prev_hash
// NULL, and every other canonical block's prev_hash must equal the
// canonical block's hash at height-1. Because
// blocks_height_canonical_uidx (migration 0001, frozen) already guarantees
// at most one canonical block per height, and the caller has already
// proven the canonical set is exactly the contiguous range
// [0, indexed_height] (canonical count/min/max — checked immediately
// before this call), lag(hash) OVER (ORDER BY height) unambiguously
// identifies "the canonical block one height below" for every row — no
// join ambiguity is possible.
//
// This is NOT redundant with the shape check: a set of canonical blocks at
// heights 0..N can satisfy count==N+1, min==0, max==N while one of them
// has a prev_hash pointing at an orphaned block instead of its actual
// canonical predecessor (e.g. external corruption, or a hand-edited
// fixture) — supplyRollupStagingSQL's SUM(...) OVER (ORDER BY height)
// would silently window-aggregate across that break as if it were a valid
// chain. This scan proves that assumption before any aggregation runs.
//
// Returns the first breach found (deterministic: lowest height), or nil if
// none exists. This is an O(chain-size) scan, acceptable ONLY on this
// operator/backfill path (task item 9 of the ancestry correction — never
// the hot public read path, and never Store.ApplyBlock, which already
// proves continuity incrementally one block at a time via
// checkCanonicalContinuity).
func findCanonicalAncestryBreach(ctx context.Context, tx pgx.Tx) (*ancestryBreach, error) {
	var breach ancestryBreach
	err := tx.QueryRow(ctx, `
		WITH canonical_chain AS (
			SELECT hash, height, prev_hash,
			       lag(hash) OVER (ORDER BY height) AS expected_prev_hash
			FROM blocks
			WHERE canonical
		)
		SELECT hash, height, prev_hash, expected_prev_hash
		FROM canonical_chain
		WHERE (height = 0 AND prev_hash IS NOT NULL)
		   OR (height > 0 AND prev_hash IS DISTINCT FROM expected_prev_hash)
		ORDER BY height
		LIMIT 1
	`).Scan(&breach.childHash, &breach.childHeight, &breach.observedPrevHash, &breach.expectedPrevHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &breach, nil
}

// supplyRollupStagingSQL builds supply_rollup_staging, a transaction-scoped
// TEMP TABLE (auto-dropped at commit/rollback — never left behind, never
// visible outside this transaction) holding one row per CANONICAL block
// with its own excluded-output value and every cumulative field, computed
// via a single window-function pass. See BackfillSupplyRollup's doc
// comment for the exact semantics.
//
// The unspendable-output CASE mirrors script.IsUnspendable byte-for-byte
// (task item 28): 0x6a is OP_RETURN (Core's fixed opcode value, not
// expected to ever change — same raw byte this codebase's own test
// fixtures already hardcode, e.g. apply_test.go's nullOut), 10000 is
// script.MaxScriptSize.
const supplyRollupStagingSQL = `
CREATE TEMP TABLE supply_rollup_staging
ON COMMIT DROP
AS
WITH canonical_chain AS (
	SELECT b.hash, b.height, ba.subsidy_satoshis, ba.fee_satoshis, ba.coinbase_output_satoshis, ba.unclaimed_reward_satoshis
	FROM blocks b
	JOIN block_accounting ba ON ba.block_hash = b.hash
	WHERE b.canonical
),
excluded_per_block AS (
	SELECT bt.block_hash,
	       COALESCE(SUM(
	           CASE WHEN b.height = 0
	                     OR octet_length(o.script_pubkey) > 10000
	                     OR (octet_length(o.script_pubkey) > 0 AND get_byte(o.script_pubkey, 0) = 106)
	                THEN o.value_satoshis ELSE 0 END
	       ), 0) AS excluded_satoshis
	FROM blocks b
	JOIN block_transactions bt ON bt.block_hash = b.hash
	JOIN transaction_outputs o ON o.txid = bt.txid
	WHERE b.canonical
	GROUP BY bt.block_hash
),
joined AS (
	SELECT c.hash, c.height, c.subsidy_satoshis, c.fee_satoshis, c.coinbase_output_satoshis, c.unclaimed_reward_satoshis,
	       COALESCE(e.excluded_satoshis, 0) AS excluded_satoshis
	FROM canonical_chain c
	LEFT JOIN excluded_per_block e ON e.block_hash = c.hash
)
SELECT hash, height, excluded_satoshis,
       SUM(subsidy_satoshis) OVER w AS cum_subsidy,
       SUM(fee_satoshis) OVER w AS cum_fee,
       SUM(coinbase_output_satoshis) OVER w AS cum_coinbase,
       SUM(unclaimed_reward_satoshis) OVER w AS cum_unclaimed,
       SUM(excluded_satoshis) OVER w AS cum_excluded,
       SUM(coinbase_output_satoshis) OVER w - SUM(fee_satoshis) OVER w - SUM(excluded_satoshis) OVER w AS cum_utxo
FROM joined
WINDOW w AS (ORDER BY height)
`

// supplyRollupContradictionSQL counts already-existing block_supply_rollup
// rows whose stored values disagree with supply_rollup_staging's freshly
// computed values for the same block_hash — see task item 33/20.
const supplyRollupContradictionSQL = `
SELECT count(*)
FROM block_supply_rollup r
JOIN supply_rollup_staging s ON s.hash = r.block_hash
WHERE r.excluded_output_satoshis != s.excluded_satoshis
   OR r.cumulative_subsidy_satoshis != s.cum_subsidy
   OR r.cumulative_fee_satoshis != s.cum_fee
   OR r.cumulative_coinbase_output_satoshis != s.cum_coinbase
   OR r.cumulative_unclaimed_reward_satoshis != s.cum_unclaimed
   OR r.cumulative_excluded_output_satoshis != s.cum_excluded
   OR r.cumulative_utxo_set_value_satoshis != s.cum_utxo
`

// supplyRollupInsertFromStagingSQL fills exactly the missing
// block_supply_rollup rows (ON CONFLICT DO NOTHING — never overwrites an
// already-verified-matching row, task item 20/33).
const supplyRollupInsertFromStagingSQL = `
INSERT INTO block_supply_rollup (
	block_hash, excluded_output_satoshis,
	cumulative_subsidy_satoshis, cumulative_fee_satoshis,
	cumulative_coinbase_output_satoshis, cumulative_unclaimed_reward_satoshis,
	cumulative_excluded_output_satoshis, cumulative_utxo_set_value_satoshis
)
SELECT hash, excluded_satoshis, cum_subsidy, cum_fee, cum_coinbase, cum_unclaimed, cum_excluded, cum_utxo
FROM supply_rollup_staging
ON CONFLICT (block_hash) DO NOTHING
`
