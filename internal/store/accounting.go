package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// applyBlockAccounting computes and idempotently persists block's
// block_accounting row, inside the same transaction ApplyBlock is already
// running. blockFeeTotal is the exact sum of this block's non-coinbase
// transaction fees, as already computed by applyTransaction from resolved
// canonical prevouts — never a second, independently derived figure (see
// docs/ARCHITECTURE.md §26 "Fee source"). The coinbase output total is
// summed directly from block.Transactions[0].Outputs — validateBlockShape
// (called earlier in ApplyBlock) has already proven transaction 0 exists
// and is coinbase, for every block this function is ever called with.
//
// accounting.ComputeBlockFacts performs the one rejection this function can
// produce: ErrCoinbaseOverclaim if the decoded coinbase output total
// exceeds subsidy + fees, which Core's own ConnectBlock would never have
// accepted (src/validation.cpp "bad-cb-amount") — seeing it here means the
// already-Core-validated block and this package's decoded model disagree,
// a data-shape bug, not chain-state corruption to silently tolerate.
// Underclaiming (a positive unclaimed reward) is ordinary, valid state and
// is never rejected.
func (s *Store) applyBlockAccounting(ctx context.Context, tx pgx.Tx, block chain.Block, blockFeeTotal int64) error {
	coinbaseOutputTotal, ok := sumOutputValues(block.Transactions[0].Outputs)
	if !ok {
		return fmt.Errorf("%w: block %s coinbase output total", ErrAmountOverflow, block.Hash)
	}

	facts, err := s.accountingSchedule.ComputeBlockFacts(block.Hash, block.Height, blockFeeTotal, coinbaseOutputTotal)
	if err != nil {
		return fmt.Errorf("block accounting: %w", err)
	}

	// Idempotent insert, same pattern as every other immutable identity in
	// this package (insertOrVerifyIdempotent): a replay of the exact same
	// block reapplies identical facts and is a safe no-op; contradictory
	// facts for an already-persisted block_hash is ErrImmutableConflict —
	// block_accounting rows are never overwritten (see
	// docs/ARCHITECTURE.md §26 "Idempotent insert"). Shared with
	// BackfillBlockAccounting (backfill.go) via the same SQL constants, so
	// live indexing and backfill can never persist this row differently.
	_, err = insertOrVerifyIdempotent(ctx, tx, "block_accounting "+facts.BlockHash,
		blockAccountingInsertSQL, blockAccountingVerifySQL,
		facts.BlockHash, facts.SubsidySatoshis, facts.FeeSatoshis, facts.CoinbaseOutputSatoshis, facts.UnclaimedRewardSatoshis,
	)
	return err
}

// blockAccountingInsertSQL / blockAccountingVerifySQL are the exact
// INSERT ... ON CONFLICT DO NOTHING / verify-match pair
// insertOrVerifyIdempotent expects (see store.go) — shared verbatim between
// applyBlockAccounting (live ApplyBlock path) and BackfillBlockAccounting
// (backfill.go), so a block's accounting row is persisted identically
// regardless of which path writes it first.
const (
	blockAccountingInsertSQL = `
		INSERT INTO block_accounting (block_hash, subsidy_satoshis, fee_satoshis, coinbase_output_satoshis, unclaimed_reward_satoshis)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (block_hash) DO NOTHING`
	blockAccountingVerifySQL = `
		SELECT subsidy_satoshis = $2 AND fee_satoshis = $3 AND coinbase_output_satoshis = $4 AND unclaimed_reward_satoshis = $5
		FROM block_accounting WHERE block_hash = $1`
)

// sumOutputValues sums every output's value, returning ok=false instead of
// silently wrapping on int64 overflow — same checked-accumulation policy as
// applyTransaction's input/output sums.
func sumOutputValues(outputs []chain.Output) (int64, bool) {
	var sum int64
	for _, out := range outputs {
		s, ok := addChecked(sum, out.Value.Satoshis())
		if !ok {
			return 0, false
		}
		sum = s
	}
	return sum, true
}

// BlockAccounting is a read-only view of one block_accounting row, exposed
// for tests and for a future 2H.2 read surface — internal/query, not this
// package, will be the one to build a public API/HTML presentation on top
// of it (Phase 2H.1 is write/foundation only).
type BlockAccounting struct {
	BlockHash               string
	SubsidySatoshis         int64
	FeeSatoshis             int64
	CoinbaseOutputSatoshis  int64
	UnclaimedRewardSatoshis int64
}

// GetBlockAccounting returns the block_accounting row for blockHash, or nil
// if none exists (e.g. an un-backfilled legacy block, or a hash that was
// never applied at all).
func (s *Store) GetBlockAccounting(ctx context.Context, blockHash string) (*BlockAccounting, error) {
	var a BlockAccounting
	err := s.pool.QueryRow(ctx, `
		SELECT block_hash, subsidy_satoshis, fee_satoshis, coinbase_output_satoshis, unclaimed_reward_satoshis
		FROM block_accounting WHERE block_hash = $1
	`, blockHash).Scan(&a.BlockHash, &a.SubsidySatoshis, &a.FeeSatoshis, &a.CoinbaseOutputSatoshis, &a.UnclaimedRewardSatoshis)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get block accounting %s: %w", blockHash, err)
	}
	return &a, nil
}

// AccountingCompleteness reports whether every currently stored block (both
// canonical and orphaned — block_accounting is immutable historical
// metadata regardless of current canonical status) has exactly one
// block_accounting row. It is a read-only diagnostic for tests and the
// backfill command, not a public API (Phase 2H.1 exposes no public
// accounting surface — see docs/ARCHITECTURE.md §26).
//
// Equal counts alone would not prove one-to-one coverage on their own (two
// blocks missing rows while two rows point at nonexistent blocks could
// coincidentally balance) — MissingCount is instead computed directly via a
// LEFT JOIN ... WHERE block_accounting.block_hash IS NULL, which counts
// actual missing rows rather than inferring them from a count comparison.
// block_accounting.block_hash's FK to blocks(hash) already makes the
// opposite defect (an accounting row for a nonexistent block) structurally
// impossible.
type AccountingCompleteness struct {
	BlockCount      int64
	AccountingCount int64
	MissingCount    int64
}

// CheckAccountingCompleteness computes AccountingCompleteness by direct
// query against the pool (not cached, not paginated — intended for tests
// and an operator-facing backfill report, not a hot path).
func CheckAccountingCompleteness(ctx context.Context, pool *pgxpool.Pool) (AccountingCompleteness, error) {
	var c AccountingCompleteness
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks`).Scan(&c.BlockCount); err != nil {
		return AccountingCompleteness{}, fmt.Errorf("store: count blocks: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_accounting`).Scan(&c.AccountingCount); err != nil {
		return AccountingCompleteness{}, fmt.Errorf("store: count block_accounting: %w", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM blocks b
		LEFT JOIN block_accounting ba ON ba.block_hash = b.hash
		WHERE ba.block_hash IS NULL
	`).Scan(&c.MissingCount); err != nil {
		return AccountingCompleteness{}, fmt.Errorf("store: count blocks missing accounting: %w", err)
	}
	return c, nil
}
