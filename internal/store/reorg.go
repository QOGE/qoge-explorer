package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RollbackTo rolls the canonical chain back to ancestorHash — which must
// already be a persisted, canonical block — inside one PostgreSQL
// transaction. It does not fetch anything from Core; detecting a fork and
// choosing the common-ancestor hash is the future indexer's job (see
// docs/ARCHITECTURE.md §16). Depth policy (automatic handling up to 100
// blocks, operator halt beyond that) is also an indexer-layer decision —
// RollbackTo will roll back to whatever ancestor it's given, of any depth;
// a caller can compute the depth itself from Tip().Height minus the
// ancestor's height before calling this.
//
// Blocks above the ancestor are marked canonical = false (orphaned_at
// set), never deleted — along with their transactions, transaction
// variants, block occurrences, inputs, witness data, and outputs, all of
// which remain queryable forever as audit history.
//
// Canonical DERIVED state is reconstructed:
//   - utxo_state rows CREATED only in a now-orphaned block are deleted —
//     that output was never confirmed on the surviving chain.
//   - utxo_state rows SPENT by a now-orphaned transaction are restored to
//     unspent (unless already deleted by the point above).
//   - every address touched by either change has its cache recomputed
//     from source tables (see recomputeAddress).
//   - sync_state('main') is set to ancestorHash as the final write; its
//     trigger re-verifies ancestorHash is canonical (it always is — it was
//     never orphaned) and re-derives indexed_height from blocks.height.
//
// After RollbackTo returns, the future indexer can apply replacement
// blocks normally via ApplyBlock.
func (s *Store) RollbackTo(ctx context.Context, ancestorHash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin rollback to %s: %w", ancestorHash, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	// Acquire the same canonical-mutation lock ApplyBlock acquires first —
	// see lockCheckpoint (store.go). This serializes RollbackTo against
	// every concurrent ApplyBlock/RollbackTo call, in this process or any
	// other one sharing the database, for the lifetime of this
	// transaction. The checkpoint value itself isn't needed here (the
	// ancestor's height is re-derived from `blocks` below), only the lock.
	if _, err := lockCheckpoint(ctx, tx); err != nil {
		return fmt.Errorf("store: rollback to %s: %w", ancestorHash, err)
	}

	var ancestorHeight int64
	err = tx.QueryRow(ctx, `SELECT height FROM blocks WHERE hash = $1 AND canonical`, ancestorHash).Scan(&ancestorHeight)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrAncestorNotFound, ancestorHash)
	}
	if err != nil {
		return fmt.Errorf("store: rollback: find ancestor %s: %w", ancestorHash, err)
	}

	rows, err := tx.Query(ctx, `SELECT hash FROM blocks WHERE height > $1 AND canonical ORDER BY height`, ancestorHeight)
	if err != nil {
		return fmt.Errorf("store: rollback: list orphan blocks: %w", err)
	}
	var orphanHashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("store: rollback: scan orphan block: %w", err)
		}
		orphanHashes = append(orphanHashes, h)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: rollback: iterate orphan blocks: %w", err)
	}

	touched := map[string]struct{}{}

	if len(orphanHashes) > 0 {
		// 1. Outputs whose CREATING block is now orphaned never existed on
		//    the surviving chain — remove them from canonical UTXO state
		//    entirely. The underlying transaction_outputs row is untouched
		//    and remains queryable as audit history.
		removedAddrRows, err := tx.Query(ctx, `
			WITH removed AS (
				DELETE FROM utxo_state WHERE creation_block_hash = ANY($1)
				RETURNING txid, vout_index
			)
			SELECT DISTINCT oa.address
			FROM removed r JOIN output_addresses oa ON oa.txid = r.txid AND oa.vout_index = r.vout_index
		`, orphanHashes)
		if err := mergeTouchedAddresses(removedAddrRows, err, touched); err != nil {
			return fmt.Errorf("store: rollback: delete orphaned utxo creations: %w", err)
		}

		// 2. Spends made by a now-orphaned SPENDING transaction are undone
		//    — restored to unspent (a no-op for rows already deleted by
		//    step 1, since UPDATE only touches rows that still exist).
		restoredAddrRows, err := tx.Query(ctx, `
			WITH restored AS (
				UPDATE utxo_state
				SET spent = false, spending_txid = NULL, spending_vin_index = NULL,
				    spending_block_hash = NULL, spending_block_height = NULL
				WHERE spending_block_hash = ANY($1)
				RETURNING txid, vout_index
			)
			SELECT DISTINCT oa.address
			FROM restored r JOIN output_addresses oa ON oa.txid = r.txid AND oa.vout_index = r.vout_index
		`, orphanHashes)
		if err := mergeTouchedAddresses(restoredAddrRows, err, touched); err != nil {
			return fmt.Errorf("store: rollback: restore spent utxos: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE blocks SET canonical = false, orphaned_at = now() WHERE hash = ANY($1)
		`, orphanHashes); err != nil {
			return fmt.Errorf("store: rollback: mark blocks orphaned: %w", err)
		}
	}

	// Phase 2H.4a: same tracked-recompute + accumulated-delta pattern
	// ApplyBlock uses (internal/store/distribution.go) — RollbackTo's
	// distribution updates are derived from the SAME old->new balance
	// transitions that already drive utxo_state/addresses reconstruction
	// above, never a second independent mechanism.
	distDeltas := newDistributionDelta()
	for addr := range touched {
		if err := recomputeAddressTracked(ctx, tx, addr, distDeltas); err != nil {
			return fmt.Errorf("store: rollback: recompute address %s: %w", addr, err)
		}
	}
	if err := applyDistributionDeltas(ctx, tx, distDeltas); err != nil {
		return fmt.Errorf("store: rollback: %w", err)
	}
	if err := setDistributionAnchor(ctx, tx, &ancestorHash); err != nil {
		return fmt.Errorf("store: rollback: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'
	`, ancestorHash); err != nil {
		return fmt.Errorf("store: rollback: checkpoint: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: rollback: commit: %w", err)
	}
	return nil
}

// mergeTouchedAddresses drains rows (a single-column address result set)
// into touched. If queryErr is non-nil, rows is not touched (it may be
// nil) and queryErr is returned directly.
func mergeTouchedAddresses(rows pgx.Rows, queryErr error, touched map[string]struct{}) error {
	if queryErr != nil {
		return queryErr
	}
	defer rows.Close()
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return err
		}
		touched[addr] = struct{}{}
	}
	return rows.Err()
}
