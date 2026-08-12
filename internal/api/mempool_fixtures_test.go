package api

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/mempool"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/jackc/pgx/v5/pgxpool"
)

func boolPtr(b bool) *bool { return &b }

// newTestMempoolStore wraps pool (already migrated by newTestServerWithPool,
// which applies every migration under migrations/ including
// 0002_mempool_cache) in a real internal/mempool.Store — mirrors
// internal/query/mempool_fixtures_test.go's helper of the same name,
// duplicated per package (test-only files aren't importable across
// packages).
func newTestMempoolStore(pool *pgxpool.Pool) *mempool.Store {
	return mempool.NewStore(pool)
}

// mempoolCandidateTx decodes raw through the REAL decode.DecodeTransaction
// pipeline and wraps it as a mempool.CandidateTransaction — mirrors
// internal/query/mempool_fixtures_test.go's helper of the same name.
func mempoolCandidateTx(t *testing.T, ctx context.Context, raw rpc.RawTransaction, feeSats, entryTime int64, entryHeight *int64, replaceable *bool, depends []string) mempool.CandidateTransaction {
	t.Helper()
	txn, err := decode.DecodeTransaction(ctx, raw, fakeAddressResolver{})
	if err != nil {
		t.Fatalf("DecodeTransaction: %v", err)
	}
	return mempool.CandidateTransaction{
		Transaction: txn,
		FeeSatoshis: feeSats,
		EntryTime:   entryTime,
		EntryHeight: entryHeight,
		Replaceable: replaceable,
		Depends:     depends,
	}
}

func mempoolCandidate(coreTipHeight int64, coreTipHash string, txs ...mempool.CandidateTransaction) mempool.Candidate {
	return mempool.Candidate{
		CoreTipHeight: coreTipHeight,
		CoreTipHash:   coreTipHash,
		Transactions:  txs,
	}
}

// newMempoolTestServer bundles newTestServerWithPool with a ready
// context.Background() and a real internal/mempool.Store sharing the same
// pool, for tests that only need to seed mempool_* fixtures (never
// confirmed-chain ones).
func newMempoolTestServer(t *testing.T) (context.Context, *Server, *pgxpool.Pool, *mempool.Store) {
	t.Helper()
	s, _, pool := newTestServerWithPool(t)
	return context.Background(), s, pool, newTestMempoolStore(pool)
}

// simpleMempoolRawTx builds a minimal single-input, single-output
// non-coinbase rpc.RawTransaction (no witness), mirroring
// internal/query/mempool_test.go's helper of the same name.
func simpleMempoolRawTx(label string) rpc.RawTransaction {
	addr := "q" + label
	return rawSpendTx(label, 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(fakeHash(label+"-prev"), 0, "473044")},
		[]rpc.RawVout{rawVout(0, 1_00000000, p2pkhScript(label), "pubkeyhash", &addr)},
	)
}
