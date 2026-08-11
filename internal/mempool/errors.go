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

	// ErrRPCIdentityMismatch means Core's getrawtransaction response for a
	// requested txid decoded to a DIFFERENT TxID than what was asked for.
	// This is deliberately NOT ErrMempoolRace: a disappeared/now-confirmed
	// transaction is an expected mempool race, but Core answering a
	// request for txid A with transaction B's data is a wire-level
	// integrity problem — fetchAndDecode rejects the whole candidate
	// rather than ever attaching one mempool entry's fee/time/dependency
	// metadata to a different decoded transaction.
	ErrRPCIdentityMismatch = errors.New("mempool: getrawtransaction response txid does not match the requested txid")
)
