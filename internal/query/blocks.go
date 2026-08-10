package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// BlockSummary is one row of the canonical recent-block list, and the
// shared header fields for BlockDetail.
type BlockSummary struct {
	Hash         string  `json:"hash"`
	Height       int64   `json:"height"`
	PreviousHash *string `json:"previous_hash"`
	MerkleRoot   string  `json:"merkle_root"`
	Time         int64   `json:"time"`
	Bits         string  `json:"bits"`
	Difficulty   float64 `json:"difficulty"`
	Nonce        uint32  `json:"nonce"`
	Size         int     `json:"size"`
	Weight       int     `json:"weight"`
	TxCount      int     `json:"tx_count"`
	Canonical    bool    `json:"canonical"`
}

// BlockTxRef is one transaction's position within a block, in the actual
// on-chain order stored by internal/store (block_transactions.tx_index) —
// never reordered alphabetically or by txid.
type BlockTxRef struct {
	Index int    `json:"index"`
	TxID  string `json:"txid"`
	WTxID string `json:"wtxid"`
}

// BlockDetail is a single block plus its ordered transaction references.
type BlockDetail struct {
	BlockSummary
	Transactions []BlockTxRef `json:"transactions"`
}

// BlockPage is one bounded, keyset-paginated page of canonical blocks,
// newest first.
type BlockPage struct {
	Blocks []BlockSummary `json:"blocks"`
	// NextBeforeHeight, when non-nil, is the value a caller should pass as
	// beforeHeight to RecentBlocks to fetch the next page. Nil means this
	// page reached the end of the canonical chain (height 0).
	NextBeforeHeight *int64 `json:"next_before_height,omitempty"`
}

const blockColumns = `hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count, canonical`

func scanBlockSummary(row pgx.Row) (BlockSummary, error) {
	var b BlockSummary
	var nonce int64
	if err := row.Scan(&b.Hash, &b.Height, &b.PreviousHash, &b.MerkleRoot, &b.Time,
		&b.Bits, &b.Difficulty, &nonce, &b.Size, &b.Weight, &b.TxCount, &b.Canonical); err != nil {
		return BlockSummary{}, err
	}
	b.Nonce = uint32(nonce)
	return b, nil
}

// RecentBlocks returns canonical blocks newest-first, keyset-paginated by
// height: beforeHeight nil starts at the current canonical tip; a non-nil
// value returns only blocks strictly below it. pageSize is clamped to
// [1, MaxPageSize] — see clampPageSize. Uses ORDER BY height (an indexed,
// unique-per-canonical-block column — see blocks_height_canonical_uidx in
// migrations/0001_initial.up.sql) rather than OFFSET, so page N's cost does
// not grow with N.
func (s *Store) RecentBlocks(ctx context.Context, beforeHeight *int64, pageSize int) (BlockPage, error) {
	pageSize = clampPageSize(pageSize)

	rows, err := s.pool.Query(ctx, `
		SELECT `+blockColumns+`
		FROM blocks
		WHERE canonical AND ($1::bigint IS NULL OR height < $1)
		ORDER BY height DESC
		LIMIT $2
	`, beforeHeight, pageSize)
	if err != nil {
		return BlockPage{}, fmt.Errorf("query: recent blocks: %w", err)
	}
	defer rows.Close()

	var page BlockPage
	for rows.Next() {
		b, err := scanBlockSummary(rows)
		if err != nil {
			return BlockPage{}, fmt.Errorf("query: recent blocks: scan: %w", err)
		}
		page.Blocks = append(page.Blocks, b)
	}
	if err := rows.Err(); err != nil {
		return BlockPage{}, fmt.Errorf("query: recent blocks: %w", err)
	}

	if len(page.Blocks) == int(pageSize) && page.Blocks[len(page.Blocks)-1].Height > 0 {
		next := page.Blocks[len(page.Blocks)-1].Height
		page.NextBeforeHeight = &next
	}
	return page, nil
}

// BlockByHeight returns the CANONICAL block at height, or ErrNotFound if
// none exists (there is never an orphan-by-height lookup — orphaned blocks
// are only reachable by hash, via BlockByHash).
func (s *Store) BlockByHeight(ctx context.Context, height int64) (BlockDetail, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+blockColumns+` FROM blocks WHERE height = $1 AND canonical
	`, height)
	return s.blockDetailFromRow(ctx, row)
}

// BlockByHash returns the block with the given hash — canonical or
// orphaned, either is a valid historical/audit lookup. Callers must check
// BlockDetail.Canonical rather than assuming a hash lookup is always
// canonical.
func (s *Store) BlockByHash(ctx context.Context, hash string) (BlockDetail, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+blockColumns+` FROM blocks WHERE hash = $1
	`, hash)
	return s.blockDetailFromRow(ctx, row)
}

func (s *Store) blockDetailFromRow(ctx context.Context, row pgx.Row) (BlockDetail, error) {
	summary, err := scanBlockSummary(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return BlockDetail{}, ErrNotFound
	}
	if err != nil {
		return BlockDetail{}, fmt.Errorf("query: block: %w", err)
	}

	txs, err := s.blockTxRefs(ctx, summary.Hash)
	if err != nil {
		return BlockDetail{}, err
	}
	return BlockDetail{BlockSummary: summary, Transactions: txs}, nil
}

// blockTxRefs returns blockHash's transactions in actual on-chain order
// (block_transactions.tx_index), never reordered.
func (s *Store) blockTxRefs(ctx context.Context, blockHash string) ([]BlockTxRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tx_index, txid, wtxid FROM block_transactions
		WHERE block_hash = $1
		ORDER BY tx_index
	`, blockHash)
	if err != nil {
		return nil, fmt.Errorf("query: block transactions for %s: %w", blockHash, err)
	}
	defer rows.Close()

	var refs []BlockTxRef
	for rows.Next() {
		var r BlockTxRef
		if err := rows.Scan(&r.Index, &r.TxID, &r.WTxID); err != nil {
			return nil, fmt.Errorf("query: block transactions for %s: scan: %w", blockHash, err)
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: block transactions for %s: %w", blockHash, err)
	}
	return refs, nil
}
