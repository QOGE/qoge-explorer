package indexer

import "errors"

// MaxAutomaticReorgDepth is the deepest reorg the indexer will resolve on
// its own. Deliberately a code constant, not an environment variable: an
// operator must not be able to accidentally weaken this safety policy by
// misconfiguring the process environment (docs/ARCHITECTURE.md §18).
const MaxAutomaticReorgDepth = 100

var (
	// ErrReorgTooDeep means either Core's tip is already more than
	// MaxAutomaticReorgDepth blocks behind the local tip, or no common
	// ancestor was found within that many blocks of the local tip. The
	// database is left unchanged; an operator must intervene.
	ErrReorgTooDeep = errors.New("indexer: reorg deeper than automatic policy allows")

	// ErrLocalChainGap means common-ancestor discovery found no canonical
	// Store block at a height at or below the local tip — a local
	// integrity failure, never "no match found." The database is left
	// unchanged.
	ErrLocalChainGap = errors.New("indexer: local canonical chain has a gap at or below the current tip")

	// ErrRemoteChainMoved is an internal control-flow sentinel: it means
	// Core's canonical chain changed mid-orchestration (between two
	// GetBlockHash calls for the same height, or between ancestor
	// discovery and the pre-rollback recheck). It is not a corruption
	// signal — callers within this package react to it by restarting
	// reconciliation immediately, and it never escapes SyncToTip/Run as a
	// terminal error.
	ErrRemoteChainMoved = errors.New("indexer: remote chain changed during sync orchestration")

	// ErrNetworkMismatch means Core's getblockchaininfo "chain" field does
	// not exactly equal the configured QOGE_NETWORK.
	ErrNetworkMismatch = errors.New("indexer: core network does not match configured QOGE_NETWORK")

	// ErrPrunedNode means Core reported pruned=true. Historical sync
	// requires a full, non-pruned node.
	ErrPrunedNode = errors.New("indexer: core node is pruned; full non-pruned node required for historical sync")

	// ErrInitialBlockDownload means Core reported initialblockdownload=true.
	// Indexing during IBD would index an unstable, partial history; the
	// operator should wait for IBD to complete and retry.
	ErrInitialBlockDownload = errors.New("indexer: core node is still in initial block download")

	// ErrSyncInProgress means SyncToTip was called while another
	// SyncToTip call on the same Indexer was already running. The
	// indexer never runs overlapping sync passes.
	ErrSyncInProgress = errors.New("indexer: sync already in progress")
)
