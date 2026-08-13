package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
)

// ─── PHASE 2H.1 FINAL BACKFILL NETWORK-SAFETY CORRECTION ──────────────────
//
// See cmd/qoge-explorer/backfill_test.go for the CLI-level ("wrong network
// is rejected before any write") integration tests. These tests exercise
// VerifyNetworkIdentity itself, directly against PostgreSQL, independent of
// the CLI wiring.

// TestVerifyNetworkIdentity_Match proves a database whose canonical genesis
// (height 0) hash equals the configured network's Core-asserted genesis
// hash passes verification.
func TestVerifyNetworkIdentity_Match(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	regtestGenesis, err := chain.ExpectedGenesisHash("regtest")
	if err != nil {
		t.Fatalf("ExpectedGenesisHash(regtest): %v", err)
	}
	s, err := NewForNetwork(pool, "regtest")
	if err != nil {
		t.Fatalf("NewForNetwork(regtest): %v", err)
	}
	g := testBlock(regtestGenesis, 0, "", coinbaseTx(hash64("matchtx"), out(0, 1, "qAlice")))
	mustApply(t, ctx, s, g)

	if err := VerifyNetworkIdentity(ctx, pool, "regtest"); err != nil {
		t.Fatalf("VerifyNetworkIdentity(regtest) = %v, want nil (genesis hash matches)", err)
	}
}

// TestVerifyNetworkIdentity_Mismatch reproduces the exact previously-
// demonstrated misconfiguration: a database whose actual canonical genesis
// is regtest's, checked against QOGE_NETWORK=main. Must fail with
// ErrGenesisMismatch, and the error text must carry the configured network,
// the expected genesis hash, and the observed genesis hash (no secrets
// involved, so all three are safe to log/print directly).
func TestVerifyNetworkIdentity_Mismatch(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	regtestGenesis, err := chain.ExpectedGenesisHash("regtest")
	if err != nil {
		t.Fatalf("ExpectedGenesisHash(regtest): %v", err)
	}
	mainGenesis, err := chain.ExpectedGenesisHash("main")
	if err != nil {
		t.Fatalf("ExpectedGenesisHash(main): %v", err)
	}
	s, err := NewForNetwork(pool, "regtest")
	if err != nil {
		t.Fatalf("NewForNetwork(regtest): %v", err)
	}
	g := testBlock(regtestGenesis, 0, "", coinbaseTx(hash64("mismatchtx"), out(0, 1, "qAlice")))
	mustApply(t, ctx, s, g)

	err = VerifyNetworkIdentity(ctx, pool, "main")
	if !errors.Is(err, ErrGenesisMismatch) {
		t.Fatalf("VerifyNetworkIdentity(main) error = %v, want ErrGenesisMismatch", err)
	}
	msg := err.Error()
	for _, want := range []string{"main", mainGenesis, regtestGenesis} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message %q does not mention %q", msg, want)
		}
	}
}

// TestVerifyNetworkIdentity_Missing proves an already-migrated but
// never-indexed database (no block at height 0 at all) fails closed rather
// than trusting the configured network alone.
func TestVerifyNetworkIdentity_Missing(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	err := VerifyNetworkIdentity(ctx, pool, "main")
	if !errors.Is(err, ErrGenesisMissing) {
		t.Fatalf("VerifyNetworkIdentity(main) on an empty database error = %v, want ErrGenesisMissing", err)
	}
}

// TestVerifyNetworkIdentity_UnsupportedOrUnknownNetworkPassesThrough proves
// VerifyNetworkIdentity queries no database state before delegating to
// chain.ExpectedGenesisHash's own network validation — an unrecognized
// network or signet is rejected with that package's own errors, not turned
// into a generic database-mismatch failure.
func TestVerifyNetworkIdentity_UnsupportedOrUnknownNetworkPassesThrough(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t) // deliberately empty: proves the DB is never even queried

	if err := VerifyNetworkIdentity(ctx, pool, "signet"); !errors.Is(err, chain.ErrGenesisUnsupportedForNetwork) {
		t.Fatalf("VerifyNetworkIdentity(signet) error = %v, want chain.ErrGenesisUnsupportedForNetwork", err)
	}
	if err := VerifyNetworkIdentity(ctx, pool, "mynet"); !errors.Is(err, chain.ErrUnknownNetwork) {
		t.Fatalf("VerifyNetworkIdentity(mynet) error = %v, want chain.ErrUnknownNetwork", err)
	}
}

// TestVerifyNetworkIdentity_MainnetMatchesOnlyMain seeds a mainnet-genesis
// database and requires "main" to match while "test" and "regtest" both
// report ErrGenesisMismatch against the SAME database.
func TestVerifyNetworkIdentity_MainnetMatchesOnlyMain(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	mainGenesis, err := chain.ExpectedGenesisHash("main")
	if err != nil {
		t.Fatalf("ExpectedGenesisHash(main): %v", err)
	}
	s, err := NewForNetwork(pool, "main")
	if err != nil {
		t.Fatalf("NewForNetwork(main): %v", err)
	}
	g := testBlock(mainGenesis, 0, "", coinbaseTx(hash64("mainonlytx"), out(0, 1, "qAlice")))
	mustApply(t, ctx, s, g)

	if err := VerifyNetworkIdentity(ctx, pool, "main"); err != nil {
		t.Fatalf("VerifyNetworkIdentity(main) = %v, want nil", err)
	}
	for _, other := range []string{"test", "regtest"} {
		if err := VerifyNetworkIdentity(ctx, pool, other); !errors.Is(err, ErrGenesisMismatch) {
			t.Fatalf("VerifyNetworkIdentity(%s) against mainnet-genesis DB = %v, want ErrGenesisMismatch", other, err)
		}
	}
}
