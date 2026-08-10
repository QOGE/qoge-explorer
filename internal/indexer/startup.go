package indexer

import (
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// ValidateStartup enforces the Core/network safety checks that must pass
// before historical indexing is allowed to mutate the database
// (docs/ARCHITECTURE.md §18). It is a pure function over an
// already-fetched getblockchaininfo response, so it's testable without an
// RPC round-trip.
//
//   - Network is required to exactly match info.Chain — never inferred
//     from an address HRP, since network/address representations can
//     overlap.
//   - A pruned node (info.Pruned) is rejected outright: this explorer's
//     historical sync needs every block from genesis.
//   - A node still in initial block download is rejected outright, not
//     retried: indexing an unstable, partial history is worse than making
//     the operator re-run the command once IBD completes. This is the
//     simplest auditable behavior for a fresh production start.
func ValidateStartup(info rpc.BlockchainInfo, wantNetwork string) error {
	if wantNetwork == "" {
		return fmt.Errorf("indexer: QOGE_NETWORK is not set")
	}
	if info.Chain != wantNetwork {
		return fmt.Errorf("%w: core reports chain=%q, QOGE_NETWORK=%q", ErrNetworkMismatch, info.Chain, wantNetwork)
	}
	if info.Pruned {
		return fmt.Errorf("%w", ErrPrunedNode)
	}
	if info.InitialBlockDownload {
		return fmt.Errorf("%w: verificationprogress=%.6f; wait for initial block download to complete and retry",
			ErrInitialBlockDownload, info.VerificationProgress)
	}
	return nil
}
