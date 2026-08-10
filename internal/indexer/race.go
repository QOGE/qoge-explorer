package indexer

import (
	"context"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
)

// getBlockCount wraps rpc.GetBlockCount and rejects a negative result as
// malformed RPC data. Core can never legitimately report a negative
// height; silently accepting one could make an uninitialized store
// (Tip().Height == -1) appear accidentally "caught up" against it (review
// round: task item 10).
func (idx *Indexer) getBlockCount(ctx context.Context) (int64, error) {
	count, err := idx.rpc.GetBlockCount(ctx)
	if err != nil {
		return 0, err
	}
	if count < 0 {
		return 0, fmt.Errorf("indexer: getblockcount returned a negative height (%d): malformed RPC data", count)
	}
	return count, nil
}

// classifyHashUnavailable is called after a GetBlockHash(height) RPC call
// fails. Core legitimately errors on GetBlockHash for a height above its
// CURRENT active tip — which is exactly what happens when Core's chain
// retreats (a shorter branch becomes active) between the moment a target
// height was chosen and the moment it's requested. This re-reads
// GetBlockCount to distinguish that normal race from a genuine transport/
// RPC failure:
//
//   - if Core's freshly-read height is now below the requested height,
//     this is remote chain movement — ErrRemoteChainMoved, safe to retry
//     via an immediate reconciliation restart;
//   - otherwise origErr is preserved as a terminal error, unchanged.
//
// Never string-matches Core's error message — the classification is
// entirely by comparing heights (review round: task item 3).
func (idx *Indexer) classifyHashUnavailable(ctx context.Context, height int64, origErr error) error {
	newHeight, countErr := idx.getBlockCount(ctx)
	if countErr != nil {
		// Can't even determine Core's current height right now: preserve
		// the original error rather than inventing a diagnosis.
		return fmt.Errorf("indexer: getblockhash(%d): %w", height, origErr)
	}
	if newHeight < height {
		return ErrRemoteChainMoved
	}
	return fmt.Errorf("indexer: getblockhash(%d): %w", height, origErr)
}

// diagnoseNonSequential is called when Store.ApplyBlock rejects a fetched
// block with store.ErrNonSequentialBlock, for a block whose own hash has
// already passed applyHeight's two-GetBlockHash race check (it was stable
// at the moment it was fetched). A non-sequential rejection is NOT
// automatically a corruption/integrity failure: Core may have
// reorganized — including a SAME-HEIGHT branch replacement, where a
// different block now sits at `height` entirely — or the local checkpoint
// may have moved via a concurrent writer (Store's canonical-mutation lock
// explicitly permits other writers sharing the same database — see
// docs/ARCHITECTURE.md §16 and §18 "Multi-writer policy"), possibly to a
// DIFFERENT HASH at the SAME height, in the window between the last
// reconcile() and this ApplyBlock call. This distinguishes that normal
// orchestration race (ErrRemoteChainMoved, safe to retry) from a
// genuinely self-contradictory fetched block (a terminal error) — every
// ErrNonSequentialBlock is diagnosed individually, never blindly mapped to
// a retry, which could hide a reproducibly malformed Core/decoder result
// forever (review round: task item 2).
//
// Step 1 re-checks the fetched block's own CURRENT identity at `height`
// before reasoning about its parent at all: if Core no longer reports
// `block.Hash` as current at `height` — e.g. a same-height branch
// replacement made this exact block obsolete — that alone fully explains
// the rejection, independent of whatever the local checkpoint's height or
// hash happen to be. Only once that identity is confirmed does diagnosis
// proceed to the parent/checkpoint comparison, bracketed by a second read
// of the same height (mirroring applyHeight's own two-hash philosophy) so
// a second reorg arriving mid-diagnosis can't produce a mixed-snapshot
// terminal judgment (review round: task item 3, final hardening round).
func (idx *Indexer) diagnoseNonSequential(ctx context.Context, height int64, block chain.Block, applyErr error) error {
	currentBlockHash1, err := idx.rpc.GetBlockHash(ctx, height)
	if err != nil {
		return idx.classifyHashUnavailable(ctx, height, err)
	}
	if currentBlockHash1 != block.Hash {
		// Core no longer reports this exact block as current at height —
		// e.g. a same-height branch replacement (whether or not the local
		// checkpoint has been updated to reflect it yet). Not malformed
		// data; restart reconciliation.
		return ErrRemoteChainMoved
	}

	currentLocal, err := idx.store.Tip(ctx)
	if err != nil {
		return fmt.Errorf("indexer: read local tip while diagnosing non-sequential block %d: %w", height, err)
	}

	// The local checkpoint no longer matches what this height assumed
	// (height-1) — another writer committed a block, rolled back, or
	// otherwise advanced canonical state concurrently. Restart
	// reconciliation rather than reasoning further about a stale view.
	if currentLocal.Height != height-1 {
		return ErrRemoteChainMoved
	}

	// Bracket the parent read with a second check of the block's own
	// identity at height: if it changed between the two reads, a second
	// reorg arrived mid-diagnosis and this observation is a mix of two
	// different Core snapshots — discard it rather than drawing a
	// terminal conclusion from it.
	currentParentHash, err := idx.rpc.GetBlockHash(ctx, currentLocal.Height)
	if err != nil {
		return idx.classifyHashUnavailable(ctx, currentLocal.Height, err)
	}
	currentBlockHash2, err := idx.rpc.GetBlockHash(ctx, height)
	if err != nil {
		return idx.classifyHashUnavailable(ctx, height, err)
	}
	if currentBlockHash2 != block.Hash {
		return ErrRemoteChainMoved
	}

	if currentParentHash != currentLocal.Hash {
		// Core reorganized below the block being applied.
		return ErrRemoteChainMoved
	}

	if block.PreviousHash != currentLocal.Hash {
		// block.Hash was confirmed stably current at `height` across both
		// reads, and Core/the local checkpoint agree on the parent at
		// height-1 — yet the fetched block's own PreviousHash contradicts
		// that stable parent. This is not a race; the fetched
		// representation is internally contradictory. Halt rather than
		// retry forever.
		return fmt.Errorf("indexer: block %d (%s) claims parent %s but the stable, currently-active parent at height %d is %s: %w",
			height, block.Hash, block.PreviousHash, currentLocal.Height, currentLocal.Hash, applyErr)
	}

	// Every known race explanation checked out clean — Core and the local
	// checkpoint agree, and the fetched block's identity/parent match
	// both, stably. Store's rejection is therefore unexplained by any
	// known orchestration race; surface it as-is rather than guessing
	// further.
	return fmt.Errorf("indexer: apply block %d (%s): %w", height, block.Hash, applyErr)
}
