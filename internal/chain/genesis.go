package chain

import (
	"errors"
	"fmt"
)

// ErrUnknownNetwork means ExpectedGenesisHash was asked for a network string
// that doesn't match any QOGE Core network this package knows a genesis hash
// for. Mirrors accounting.ErrUnknownNetwork's policy: callers must never
// fall back to a default genesis hash on this error.
var ErrUnknownNetwork = errors.New("chain: unknown network")

// ErrGenesisUnsupportedForNetwork means network is a real QOGE Core network,
// but this package has no stable, independently-verifiable genesis hash for
// it to check against — currently true only for signet, which QOGE Core
// stable's src/chainparams.cpp documents as non-functional (no
// assert(consensus.hashGenesisBlock == ...) is ever compiled for
// CSigNetParams, unlike CMainParams/CTestNetParams/CRegTestParams, which all
// assert an exact genesis hash). Inventing a signet genesis hash here would
// be worse than refusing to check one: it would let a signet database pass
// or fail network-identity verification based on a value this codebase
// cannot actually source from Core. Callers must treat this the same as any
// other verification failure — refuse to proceed, never silently skip the
// check.
var ErrGenesisUnsupportedForNetwork = errors.New("chain: no stable asserted genesis hash is available for this network")

// mainGenesisHash, testGenesisHash, and regtestGenesisHash are QOGE Core
// stable's canonical genesis block hashes, verified directly against
// src/chainparams.cpp's assert(consensus.hashGenesisBlock ==
// uint256S("0x...")) for CMainParams/CTestNetParams/CRegTestParams
// respectively (lowercase hex, no "0x" prefix, matching how `blocks.hash` is
// stored and CHECK-constrained — migrations/0001_initial.up.sql). There is
// deliberately no signet constant: see ErrGenesisUnsupportedForNetwork.
const (
	mainGenesisHash    = "78cf9e38dad7e61400f3a3e4e987efa7c90c09f69d9be7ce95e504bfa447aadc"
	testGenesisHash    = "8e7f8c6096865a08773b52fd776a0e283d9037ff2b50b20a04f6a0feb01c68d9"
	regtestGenesisHash = "7a69b7bb0c8f5ced03c9e64b770c30b52582d072cbe506339b8d5331b014d727"
)

// ExpectedGenesisHash returns the canonical, Core-asserted genesis block
// hash for network — the exact Core getblockchaininfo "chain" string ("main",
// "test", "signet", "regtest"), never inferred. This is a pure lookup with no
// database or RPC dependency, so a caller can verify a database's actual
// indexed genesis (height 0) against it before trusting that database
// belongs to the network the operator configured — see
// docs/ARCHITECTURE.md §26 "Backfill network identity verification".
//
// An unrecognized network string returns ErrUnknownNetwork. signet returns
// ErrGenesisUnsupportedForNetwork: QOGE Core stable documents signet as
// non-functional and asserts no genesis hash for it, so this package refuses
// to invent one rather than let a signet database silently pass or fail a
// check against a fabricated value.
func ExpectedGenesisHash(network string) (string, error) {
	switch network {
	case "main":
		return mainGenesisHash, nil
	case "test":
		return testGenesisHash, nil
	case "regtest":
		return regtestGenesisHash, nil
	case "signet":
		return "", fmt.Errorf("%w: %q", ErrGenesisUnsupportedForNetwork, network)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownNetwork, network)
	}
}
