package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/QOGE/qoge-explorer/internal/decode"
)

// applyHeight fetches, decodes, and applies exactly one block at height,
// following task item 7's sequential pipeline:
//
//  1. hash1 = GetBlockHash(height)
//  2. raw   = GetBlockVerbose2(hash1)
//  3. block = DecodeBlock(raw)
//  4. verify block.Height == height && block.Hash == hash1
//  5. hash2 = GetBlockHash(height)
//  6. require hash2 == hash1 before ApplyBlock
//  7. Store.ApplyBlock(block)
//
// Step 5/6 closes the race where Core reorganizes between step 1 and the
// point ApplyBlock would otherwise commit: if hash2 != hash1, this returns
// ErrRemoteChainMoved without ever calling ApplyBlock, and the caller
// restarts reconciliation rather than treating it as corruption (task item
// 8).
func (idx *Indexer) applyHeight(ctx context.Context, height int64) error {
	fetchStart := time.Now()
	hash1, err := idx.rpc.GetBlockHash(ctx, height)
	if err != nil {
		return fmt.Errorf("indexer: getblockhash(%d): %w", height, err)
	}

	raw, err := idx.rpc.GetBlockVerbose2(ctx, hash1)
	if err != nil {
		return fmt.Errorf("indexer: getblock(%d, %s) verbose 2: %w", height, hash1, err)
	}
	fetchElapsed := time.Since(fetchStart)

	decodeStart := time.Now()
	block, err := decode.DecodeBlock(ctx, raw, idx.resolver)
	if err != nil {
		return fmt.Errorf("indexer: decode block %d (%s): %w", height, hash1, err)
	}
	decodeElapsed := time.Since(decodeStart)

	if block.Height != height {
		return fmt.Errorf("indexer: decoded block height %d does not match requested height %d (hash %s)",
			block.Height, height, hash1)
	}
	if block.Hash != hash1 {
		return fmt.Errorf("indexer: decoded block hash %s does not match requested hash %s (height %d)",
			block.Hash, hash1, height)
	}

	hash2, err := idx.rpc.GetBlockHash(ctx, height)
	if err != nil {
		return fmt.Errorf("indexer: getblockhash(%d) race recheck: %w", height, err)
	}
	if hash2 != hash1 {
		idx.log.Warn("remote chain moved during block fetch; discarding stale block",
			"height", height, "first_hash", hash1, "second_hash", hash2)
		return ErrRemoteChainMoved
	}

	applyStart := time.Now()
	if err := idx.store.ApplyBlock(ctx, block); err != nil {
		return fmt.Errorf("indexer: apply block %d (%s): %w", height, hash1, err)
	}
	applyElapsed := time.Since(applyStart)

	idx.log.Debug("applied block",
		"height", height, "hash", hash1,
		"fetch_ms", fetchElapsed.Milliseconds(),
		"decode_ms", decodeElapsed.Milliseconds(),
		"apply_ms", applyElapsed.Milliseconds(),
	)
	return nil
}
