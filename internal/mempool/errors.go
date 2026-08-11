package mempool

import "errors"

var (
	// ErrConfirmedIndexBehind means the confirmed PostgreSQL checkpoint
	// does not currently match Core's active-chain tip (docs/
	// ARCHITECTURE.md §22, spec item 20 step C). Publishing a mempool
	// snapshot while historical indexing is still catching up would
	// anchor it to a confirmed-state view Phase 2F.2 cannot yet trust
	// against. refreshOnce returns this instead of publishing; Run treats
	// it as an ordinary skip (debug-logged), never a failure.
	ErrConfirmedIndexBehind = errors.New("mempool: confirmed index is behind core's active tip; skipping publication")

	// ErrMempoolRace means Core's mempool or active chain changed during
	// snapshot acquisition — the tip moved, a listed transaction
	// disappeared, or a listed transaction became confirmed mid-fetch
	// (docs/ARCHITECTURE.md §22, spec items 18/20/21). This is a normal,
	// expected condition, not corruption: refreshOnce discards the
	// candidate and the previously committed snapshot is preserved
	// untouched; Run retries on the next poll.
	ErrMempoolRace = errors.New("mempool: core mempool or active chain changed during snapshot acquisition")
)
