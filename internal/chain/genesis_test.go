package chain

import (
	"errors"
	"testing"
)

// TestExpectedGenesisHash_KnownNetworks proves the exact hashes verified
// directly against QOGE Core stable's src/chainparams.cpp
// (assert(consensus.hashGenesisBlock == uint256S("0x...")) for
// CMainParams/CTestNetParams/CRegTestParams) are returned, lowercase and
// without a "0x" prefix — matching how blocks.hash is stored and
// CHECK-constrained (migrations/0001_initial.up.sql: hash ~ '^[0-9a-f]{64}$').
func TestExpectedGenesisHash_KnownNetworks(t *testing.T) {
	cases := []struct {
		network string
		want    string
	}{
		{"main", "78cf9e38dad7e61400f3a3e4e987efa7c90c09f69d9be7ce95e504bfa447aadc"},
		{"test", "8e7f8c6096865a08773b52fd776a0e283d9037ff2b50b20a04f6a0feb01c68d9"},
		{"regtest", "7a69b7bb0c8f5ced03c9e64b770c30b52582d072cbe506339b8d5331b014d727"},
	}
	for _, tc := range cases {
		t.Run(tc.network, func(t *testing.T) {
			got, err := ExpectedGenesisHash(tc.network)
			if err != nil {
				t.Fatalf("ExpectedGenesisHash(%q): unexpected error: %v", tc.network, err)
			}
			if got != tc.want {
				t.Fatalf("ExpectedGenesisHash(%q) = %q, want %q", tc.network, got, tc.want)
			}
			if len(got) != 64 {
				t.Fatalf("ExpectedGenesisHash(%q) length = %d, want 64", tc.network, len(got))
			}
			for _, c := range got {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Fatalf("ExpectedGenesisHash(%q) = %q contains non-lowercase-hex character %q", tc.network, got, c)
				}
			}
		})
	}
}

// TestExpectedGenesisHash_Signet proves signet is explicitly rejected as
// unsupported rather than given an invented genesis hash: QOGE Core stable
// asserts no genesis hash for CSigNetParams (its own source comment reads
// "No assert on hashGenesisBlock: signet is not functional in this
// release"), unlike every other network.
func TestExpectedGenesisHash_Signet(t *testing.T) {
	_, err := ExpectedGenesisHash("signet")
	if !errors.Is(err, ErrGenesisUnsupportedForNetwork) {
		t.Fatalf("ExpectedGenesisHash(signet) error = %v, want ErrGenesisUnsupportedForNetwork", err)
	}
}

// TestExpectedGenesisHash_UnknownNetwork proves an unrecognized network
// string is rejected rather than silently matched against any network's
// hash.
func TestExpectedGenesisHash_UnknownNetwork(t *testing.T) {
	_, err := ExpectedGenesisHash("mynet")
	if !errors.Is(err, ErrUnknownNetwork) {
		t.Fatalf("ExpectedGenesisHash(mynet) error = %v, want ErrUnknownNetwork", err)
	}
}

// TestExpectedGenesisHash_NetworksAreDistinct guards against a copy/paste
// constant mistake that would make two networks silently indistinguishable
// by genesis hash (defeating the entire point of this package).
func TestExpectedGenesisHash_NetworksAreDistinct(t *testing.T) {
	networks := []string{"main", "test", "regtest"}
	seen := make(map[string]string, len(networks))
	for _, n := range networks {
		hash, err := ExpectedGenesisHash(n)
		if err != nil {
			t.Fatalf("ExpectedGenesisHash(%q): %v", n, err)
		}
		if other, ok := seen[hash]; ok {
			t.Fatalf("networks %q and %q share the same genesis hash %q", n, other, hash)
		}
		seen[hash] = n
	}
}
