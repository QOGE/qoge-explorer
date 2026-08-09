package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Phase 2B.2 block-application engine: it persists
// already-parsed chain.Block/chain.Transaction data into the frozen
// PostgreSQL schema (migrations/0001_initial.up.sql), one canonical block
// at a time. Store deliberately knows nothing about Qogecoin Core RPC or
// historical synchronization — see docs/ARCHITECTURE.md §16 for the
// boundary this package sits behind.
//
// Store is not an ORM or a generic repository: it exposes exactly the
// operations a future indexer needs (ApplyBlock, Tip, RollbackTo, GetUTXO)
// and keeps every SQL/table-ordering concern internal.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps an already-connected pool (see Connect) in a Store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Sentinel errors ApplyBlock/RollbackTo return, wrapped with contextual
// detail via fmt.Errorf("%w: ...", ...) — callers should use errors.Is
// against these rather than matching error strings.
var (
	// ErrImmutableConflict means a block claimed data for an already-
	// persisted immutable identity (block hash, txid, wtxid, or a specific
	// input/output/witness item) that contradicts what's already stored.
	// Reapplying IDENTICAL data for the same identity is not this error —
	// see docs/ARCHITECTURE.md §16 "Idempotent replay."
	ErrImmutableConflict = errors.New("store: immutable record conflict")

	// ErrMissingPrevout means a non-coinbase input's previous output has
	// no utxo_state row at all. Store never invents a missing previous
	// output — see docs/ARCHITECTURE.md §16.
	ErrMissingPrevout = errors.New("store: missing previous output")

	// ErrDoubleSpend means a non-coinbase input claims to spend a
	// utxo_state row that is already spent by a DIFFERENT transaction/
	// input than this one.
	ErrDoubleSpend = errors.New("store: output already spent")

	// ErrNegativeFee means sum(input values) - sum(output values) came out
	// negative for a non-coinbase transaction — an internal indexing
	// inconsistency (see task item 8), not a valid chain state.
	ErrNegativeFee = errors.New("store: computed fee is negative")

	// ErrAncestorNotFound means RollbackTo was asked to roll back to a
	// block hash that either doesn't exist or isn't currently canonical.
	ErrAncestorNotFound = errors.New("store: rollback ancestor block not found or not canonical")

	// ErrWitnessIdentityMismatch means a transaction's WTxID/TxID equality
	// contradicts whether any of its inputs actually carry witness data
	// (task item 4: wtxid == txid if and only if the transaction has no
	// witness data at all).
	ErrWitnessIdentityMismatch = errors.New("store: wtxid/txid does not match witness presence")
)

// Checkpoint is the durable sync_state('main') row: the block the store
// currently considers fully indexed. Height is -1 and Hash is empty for an
// explorer that has never synced (the migration's bootstrap row).
type Checkpoint struct {
	Height int64
	Hash   string
}

// Tip returns the current sync_state('main') checkpoint.
func (s *Store) Tip(ctx context.Context) (Checkpoint, error) {
	var cp Checkpoint
	var hash *string
	err := s.pool.QueryRow(ctx,
		`SELECT indexed_height, indexed_block_hash FROM sync_state WHERE name = 'main'`,
	).Scan(&cp.Height, &hash)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("store: read tip: %w", err)
	}
	if hash != nil {
		cp.Hash = *hash
	}
	return cp, nil
}

// UTXO is the canonical, mutable spend state of one transaction output —
// a read-only view of one utxo_state row.
type UTXO struct {
	TxID                string
	VoutIndex           int
	CreationBlockHash   string
	CreationBlockHeight int64
	Spent               bool
	SpendingTxID        string
	SpendingVinIndex    int
	SpendingBlockHash   string
	SpendingBlockHeight int64
}

// GetUTXO returns the current utxo_state row for (txid, voutIndex), or nil
// if no such row exists (the output was never created on the canonical
// chain, or its creating block has since been rolled back).
func (s *Store) GetUTXO(ctx context.Context, txid string, voutIndex int) (*UTXO, error) {
	var u UTXO
	var spendingTxID, spendingBlockHash *string
	var spendingVinIndex, spendingBlockHeight *int64
	err := s.pool.QueryRow(ctx, `
		SELECT txid, vout_index, creation_block_hash, creation_block_height,
		       spent, spending_txid, spending_vin_index, spending_block_hash, spending_block_height
		FROM utxo_state WHERE txid = $1 AND vout_index = $2
	`, txid, voutIndex).Scan(
		&u.TxID, &u.VoutIndex, &u.CreationBlockHash, &u.CreationBlockHeight,
		&u.Spent, &spendingTxID, &spendingVinIndex, &spendingBlockHash, &spendingBlockHeight,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get utxo %s:%d: %w", txid, voutIndex, err)
	}
	if spendingTxID != nil {
		u.SpendingTxID = *spendingTxID
	}
	if spendingVinIndex != nil {
		u.SpendingVinIndex = int(*spendingVinIndex)
	}
	if spendingBlockHash != nil {
		u.SpendingBlockHash = *spendingBlockHash
	}
	if spendingBlockHeight != nil {
		u.SpendingBlockHeight = *spendingBlockHeight
	}
	return &u, nil
}

// insertOrVerifyIdempotent runs insertSQL — an
// "INSERT ... ON CONFLICT (<natural key>) DO NOTHING" statement — and
// returns nil if the row was freshly inserted. If the row already existed
// (0 rows affected), it runs verifySQL with the SAME args: a SELECT
// returning one boolean column that is true iff the existing row's
// non-key columns match what this call intended to insert. A false result
// means the same natural key was claimed with contradictory data, which is
// reported as ErrImmutableConflict rather than silently overwritten — see
// docs/ARCHITECTURE.md §16 "Idempotent replay" and "Immutable conflict
// semantics."
func insertOrVerifyIdempotent(ctx context.Context, tx pgx.Tx, what string, insertSQL string, verifySQL string, args ...any) error {
	tag, err := tx.Exec(ctx, insertSQL, args...)
	if err != nil {
		return fmt.Errorf("store: insert %s: %w", what, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var matches bool
	if err := tx.QueryRow(ctx, verifySQL, args...).Scan(&matches); err != nil {
		return fmt.Errorf("store: verify idempotent %s: %w", what, err)
	}
	if !matches {
		return fmt.Errorf("%w: %s", ErrImmutableConflict, what)
	}
	return nil
}
