package query

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/decode"
	"github.com/QOGE/qoge-explorer/internal/mempool"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestMempoolStore wraps pool (the SAME pool newTestQueryStore already
// migrated, which applies every migration under migrations/ including
// 0002_mempool_cache) in a real internal/mempool.Store, so this package's
// own tests can build mempool_* fixtures through the real, already-reviewed
// writer (Store.ReplaceSnapshot) — never ad-hoc SQL — exactly mirroring how
// newTestQueryStore already hands back a real confirmed internal/store.Store
// for building confirmed fixtures via ApplyBlock.
func newTestMempoolStore(pool *pgxpool.Pool) *mempool.Store {
	return mempool.NewStore(pool)
}

// mempoolCandidateTx decodes raw through the REAL decode.DecodeTransaction
// pipeline (never ad-hoc field assignment) and wraps it as a
// mempool.CandidateTransaction — the same construction
// internal/mempool/p2qpk_test.go's TestP2QPKAndP2TR_EndToEnd uses,
// duplicated here (test-only files aren't importable across packages) so
// this package's tests can populate real mempool_* rows through the real
// decode -> candidate -> Store.ReplaceSnapshot pipeline.
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

// mempoolCandidate assembles a mempool.Candidate anchored to
// (coreTipHeight, coreTipHash) from txs.
func mempoolCandidate(coreTipHeight int64, coreTipHash string, txs ...mempool.CandidateTransaction) mempool.Candidate {
	return mempool.Candidate{
		CoreTipHeight: coreTipHeight,
		CoreTipHash:   coreTipHash,
		Transactions:  txs,
	}
}

func boolPtr(b bool) *bool { return &b }

// compressedPubKey builds a structurally valid 33-byte compressed pubkey
// push value — duplicated from internal/mempool/fixtures_test.go (test-only
// files aren't importable across packages).
func compressedPubKey(label string) []byte {
	sum := sha256.Sum256([]byte("pubkey:" + label))
	pk := make([]byte, 33)
	pk[0] = 0x02
	copy(pk[1:], sum[:])
	return pk
}

// multisigScript builds a bare 1-of-N CHECKMULTISIG scriptPubKey:
// OP_1 <pubkey1>...<pubkeyN> OP_N OP_CHECKMULTISIG — duplicated from
// internal/mempool/fixtures_test.go.
func multisigScript(pubKeys [][]byte) []byte {
	s := []byte{0x51} // OP_1 (m=1)
	for _, pk := range pubKeys {
		s = append(s, byte(len(pk)))
		s = append(s, pk...)
	}
	s = append(s, byte(0x50+len(pubKeys))) // OP_N (n=len(pubKeys))
	s = append(s, 0xae)                    // OP_CHECKMULTISIG
	return s
}
