package indexer

import (
	"context"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/store"
)

// reconcile ensures the local Store checkpoint is on Core's currently
// active chain before any forward syncing happens (task item 9). For an
// uninitialized store (Tip().Height == -1) there is nothing to reconcile —
// production sync always starts at genesis (task item 4), which the
// forward-sync loop in syncToTipLocked already achieves on its own by
// starting at local.Height+1 == 0.
//
// If the local tip's height is at or below Core's reported tip AND Core's
// hash at that height matches the local tip's hash, the local tip is
// confirmed on the active chain and reconcile returns immediately. Any
// other case — a hash mismatch at the local tip's height, or the local
// tip being taller than Core's current tip at all (a "tip retreat") — is
// handled by reorg.
func (idx *Indexer) reconcile(ctx context.Context) error {
	local, err := idx.store.Tip(ctx)
	if err != nil {
		return fmt.Errorf("indexer: read local tip: %w", err)
	}
	if local.Height < 0 {
		return nil // uninitialized store: forward sync starts at genesis
	}

	remoteHeight, err := idx.rpc.GetBlockCount(ctx)
	if err != nil {
		return fmt.Errorf("indexer: getblockcount: %w", err)
	}

	if local.Height <= remoteHeight {
		coreHash, err := idx.rpc.GetBlockHash(ctx, local.Height)
		if err != nil {
			return fmt.Errorf("indexer: getblockhash(%d): %w", local.Height, err)
		}
		if coreHash == local.Hash {
			return nil // local tip is still on Core's active chain
		}
	}

	return idx.reorg(ctx, local, remoteHeight)
}

// reorg performs bounded common-ancestor discovery and, if a stable
// ancestor is found within MaxAutomaticReorgDepth of the local tip, rolls
// back to it (task items 10-12). It never calls Store.RollbackTo before
// the depth policy has been checked, and it re-verifies the candidate
// ancestor hash against Core immediately before rolling back, discarding
// the candidate (via ErrRemoteChainMoved) if Core changed in the meantime.
func (idx *Indexer) reorg(ctx context.Context, local store.Checkpoint, remoteHeight int64) error {
	searchTop := local.Height
	if remoteHeight < searchTop {
		searchTop = remoteHeight
	}

	// Core's own tip is already more than MaxAutomaticReorgDepth blocks
	// behind the local tip: any ancestor within reach would already
	// violate the depth policy, so there is no point searching at all
	// (task item 11, "efficient search").
	if local.Height-searchTop > MaxAutomaticReorgDepth {
		return fmt.Errorf("%w: core tip height %d is %d blocks behind local tip %d",
			ErrReorgTooDeep, remoteHeight, local.Height-searchTop, local.Height)
	}

	floor := local.Height - MaxAutomaticReorgDepth
	if floor < 0 {
		floor = 0
	}

	ancestorHeight := int64(-1)
	ancestorHash := ""
	for h := searchTop; h >= floor; h-- {
		localHash, found, err := idx.store.CanonicalBlockHash(ctx, h)
		if err != nil {
			return fmt.Errorf("indexer: read local canonical hash at height %d: %w", h, err)
		}
		if !found {
			return fmt.Errorf("%w: no canonical local block at height %d (<= local tip %d)",
				ErrLocalChainGap, h, local.Height)
		}

		coreHash, err := idx.rpc.GetBlockHash(ctx, h)
		if err != nil {
			return fmt.Errorf("indexer: getblockhash(%d): %w", h, err)
		}

		if localHash == coreHash {
			ancestorHeight = h
			ancestorHash = localHash
			break
		}
	}

	if ancestorHeight < 0 {
		return fmt.Errorf("%w: no common ancestor found within %d blocks of local tip %d (height %d)",
			ErrReorgTooDeep, MaxAutomaticReorgDepth, local.Height, floor)
	}

	// Re-read Core's hash at the candidate ancestor height immediately
	// before rolling back: if Core changed branch again during the search
	// above, the candidate may be a mix of two different Core branches
	// and must be discarded rather than trusted (task item 10).
	recheck, err := idx.rpc.GetBlockHash(ctx, ancestorHeight)
	if err != nil {
		return fmt.Errorf("indexer: re-verify ancestor hash at %d: %w", ancestorHeight, err)
	}
	if recheck != ancestorHash {
		return ErrRemoteChainMoved
	}

	depth := local.Height - ancestorHeight
	idx.log.Info("reorg detected",
		"old_tip_height", local.Height,
		"old_tip_hash", local.Hash,
		"ancestor_height", ancestorHeight,
		"ancestor_hash", ancestorHash,
		"depth", depth,
	)

	if err := idx.store.RollbackTo(ctx, ancestorHash); err != nil {
		return fmt.Errorf("indexer: rollback to ancestor %s (height %d): %w", ancestorHash, ancestorHeight, err)
	}

	newTip, err := idx.store.Tip(ctx)
	if err != nil {
		return fmt.Errorf("indexer: read tip after rollback: %w", err)
	}
	if newTip.Height != ancestorHeight || newTip.Hash != ancestorHash {
		return fmt.Errorf("indexer: after rollback, tip is (height %d, hash %s), want (height %d, hash %s)",
			newTip.Height, newTip.Hash, ancestorHeight, ancestorHash)
	}

	idx.log.Info("reorg rollback complete, resuming forward sync",
		"ancestor_height", ancestorHeight, "ancestor_hash", ancestorHash)
	return nil
}
