package query

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbSnapshot captures row COUNTS across every table a mutating write could
// touch — a coarse, cheap structural check that nothing was inserted or
// deleted. It intentionally does NOT prove content is unchanged: an
// UPDATE-only mutation (e.g. blocks.canonical true -> false, or
// utxo_state.spent false -> true) leaves every count identical — see
// mutableContentSnapshot below for the check that actually catches that
// class of bug (review round: task item 13).
type dbSnapshot struct {
	syncState, blocks, txs, variants, blockTxs, inputs, witness, outputs, addrs, addrOut, participants, utxo                int64
	indexedHeight                                                                                                           int64
	mempoolState, mempoolTxs, mempoolInputs, mempoolWitness, mempoolOutputs, mempoolAddrs, mempoolParticipants, mempoolDeps int64
}

func snapshotDB(t *testing.T, ctx context.Context, pool *pgxpool.Pool) dbSnapshot {
	t.Helper()
	var s dbSnapshot
	count := func(table string) int64 {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	s.blocks = count("blocks")
	s.txs = count("transactions")
	s.variants = count("transaction_variants")
	s.blockTxs = count("block_transactions")
	s.inputs = count("transaction_inputs")
	s.witness = count("transaction_input_witness")
	s.outputs = count("transaction_outputs")
	s.addrs = count("addresses")
	s.addrOut = count("output_addresses")
	s.participants = count("output_participants")
	s.utxo = count("utxo_state")
	s.mempoolState = count("mempool_state")
	s.mempoolTxs = count("mempool_transactions")
	s.mempoolInputs = count("mempool_inputs")
	s.mempoolWitness = count("mempool_input_witness")
	s.mempoolOutputs = count("mempool_outputs")
	s.mempoolAddrs = count("mempool_output_addresses")
	s.mempoolParticipants = count("mempool_output_participants")
	s.mempoolDeps = count("mempool_dependencies")
	if err := pool.QueryRow(ctx, "SELECT indexed_height FROM sync_state WHERE name = 'main'").Scan(&s.indexedHeight); err != nil {
		t.Fatalf("read sync_state: %v", err)
	}
	return s
}

// mutableContentSnapshot captures the actual CONTENT of every mutable
// column this phase's query layer could conceivably observe or disturb —
// not just row counts. Comparing two of these with reflect.DeepEqual
// catches an UPDATE-only mutation a row-count comparison would miss
// entirely (task item 13's examples: blocks.canonical flipping,
// utxo_state.spent flipping, addresses.balance_satoshis changing).
// Serialized as sorted, deterministic string lines (one per row, ordered
// by primary key) rather than raw structs so a test failure's diff is
// directly readable.
type mutableContentSnapshot struct {
	syncState string
	blocks    []string // ordered by hash: "hash|canonical|orphaned_at_is_null"
	utxo      []string // ordered by (txid,vout_index): full spend-state row
	addresses []string // ordered by address: full balance-cache row
}

func snapshotMutableContent(t *testing.T, ctx context.Context, pool *pgxpool.Pool) mutableContentSnapshot {
	t.Helper()

	var height int64
	var hash *string
	if err := pool.QueryRow(ctx,
		`SELECT indexed_height, indexed_block_hash FROM sync_state WHERE name = 'main'`,
	).Scan(&height, &hash); err != nil {
		t.Fatalf("read sync_state: %v", err)
	}
	hashStr := "<nil>"
	if hash != nil {
		hashStr = *hash
	}

	return mutableContentSnapshot{
		syncState: fmt.Sprintf("%d|%s", height, hashStr),
		blocks: scanLines(t, ctx, pool,
			`SELECT hash, canonical, orphaned_at IS NULL FROM blocks ORDER BY hash`,
			func(rows pgx.Rows) string {
				var h string
				var canonical, orphanedAtIsNull bool
				mustScan(t, rows, &h, &canonical, &orphanedAtIsNull)
				return fmt.Sprintf("%s|%t|%t", h, canonical, orphanedAtIsNull)
			}),
		utxo: scanLines(t, ctx, pool,
			`SELECT txid, vout_index, creation_block_hash, creation_block_height, spent,
			        spending_txid, spending_vin_index, spending_block_hash, spending_block_height
			 FROM utxo_state ORDER BY txid, vout_index`,
			func(rows pgx.Rows) string {
				var txid, creationHash string
				var vout int
				var creationHeight int64
				var spent bool
				var spendingTxid, spendingBlockHash *string
				var spendingVin, spendingHeight *int64
				mustScan(t, rows, &txid, &vout, &creationHash, &creationHeight, &spent,
					&spendingTxid, &spendingVin, &spendingBlockHash, &spendingHeight)
				return fmt.Sprintf("%s:%d|%s|%d|%t|%s|%s|%s|%s",
					txid, vout, creationHash, creationHeight, spent,
					strOrNil(spendingTxid), int64OrNil(spendingVin), strOrNil(spendingBlockHash), int64OrNil(spendingHeight))
			}),
		addresses: scanLines(t, ctx, pool,
			`SELECT address, total_received_satoshis, total_sent_satoshis, balance_satoshis,
			        tx_count, first_seen_height, last_seen_height
			 FROM addresses ORDER BY address`,
			func(rows pgx.Rows) string {
				var addr string
				var received, sent, balance int64
				var txCount int
				var firstSeen, lastSeen *int64
				mustScan(t, rows, &addr, &received, &sent, &balance, &txCount, &firstSeen, &lastSeen)
				return fmt.Sprintf("%s|%d|%d|%d|%d|%s|%s",
					addr, received, sent, balance, txCount, int64OrNil(firstSeen), int64OrNil(lastSeen))
			}),
	}
}

func scanLines(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, line func(pgx.Rows) string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, sql)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		lines = append(lines, line(rows))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %q: %v", sql, err)
	}
	return lines
}

func mustScan(t *testing.T, rows pgx.Rows, dest ...any) {
	t.Helper()
	if err := rows.Scan(dest...); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

func strOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func int64OrNil(p *int64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *p)
}

// 29: normal API/query calls never mutate sync_state, canonical flags,
// utxo_state, or address caches — snapshot every mutable table's actual
// CONTENT (not just row counts — see mutableContentSnapshot) before and
// after a broad suite of reads and require exact equality.
func TestReadOnlyEnforcement(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	f := buildTxFixture(t, ctx, st)

	mstore := newTestMempoolStore(pool)
	roTx := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("readonly-mempool", 0), 1000, 1_700_000_000, i64Ptr(1), boolPtr(true), nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("readonly-mempool-tip"), roTx)); err != nil {
		t.Fatalf("seed mempool fixture: %v", err)
	}

	beforeCounts := snapshotDB(t, ctx, pool)
	beforeContent := snapshotMutableContent(t, ctx, pool)

	// A broad read sweep: every exported query, including lookups for
	// data that doesn't exist (error paths must not write either), and
	// both the default and raw-witness-opt-in paths.
	if _, err := q.Status(ctx); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := q.RecentBlocks(ctx, nil, 50); err != nil {
		t.Fatalf("RecentBlocks: %v", err)
	}
	if _, err := q.BlockByHeight(ctx, 0); err != nil {
		t.Fatalf("BlockByHeight: %v", err)
	}
	if _, err := q.BlockByHeight(ctx, 999); err != ErrNotFound {
		t.Fatalf("BlockByHeight(missing): %v", err)
	}
	if _, err := q.BlockByHash(ctx, f.block2.Hash); err != nil {
		t.Fatalf("BlockByHash: %v", err)
	}
	if _, err := q.BlockByHash(ctx, fakeHash("does-not-exist")); err != ErrNotFound {
		t.Fatalf("BlockByHash(missing): %v", err)
	}
	if _, err := q.TransactionByTxID(ctx, f.spendTxid, false); err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}
	if _, err := q.TransactionByTxID(ctx, f.spendTxid, true); err != nil {
		t.Fatalf("TransactionByTxID(raw witness): %v", err)
	}
	if _, err := q.TransactionByWTxID(ctx, f.spendWtxid, false); err != nil {
		t.Fatalf("TransactionByWTxID: %v", err)
	}
	if _, err := q.TransactionByWTxID(ctx, f.spendWtxid, true); err != nil {
		t.Fatalf("TransactionByWTxID(raw witness): %v", err)
	}
	if _, err := q.AddressSummary(ctx, "qTxY1"); err != nil {
		t.Fatalf("AddressSummary: %v", err)
	}
	if _, err := q.AddressSummary(ctx, "qNeverSeenAnywhere"); err != nil {
		t.Fatalf("AddressSummary(unused): %v", err)
	}
	if _, err := q.AddressHistory(ctx, "qTxY1", nil, nil, 10); err != nil {
		t.Fatalf("AddressHistory: %v", err)
	}
	if _, err := q.MempoolState(ctx); err != nil {
		t.Fatalf("MempoolState: %v", err)
	}
	if _, err := q.MempoolOverview(ctx, nil, 50); err != nil {
		t.Fatalf("MempoolOverview: %v", err)
	}
	if _, err := q.MempoolTransactionByTxID(ctx, roTx.TxID, false); err != nil {
		t.Fatalf("MempoolTransactionByTxID: %v", err)
	}
	if _, err := q.MempoolTransactionByTxID(ctx, roTx.TxID, true); err != nil {
		t.Fatalf("MempoolTransactionByTxID(raw witness): %v", err)
	}
	if _, err := q.MempoolTransactionByWTxID(ctx, roTx.WTxID, false); err != nil {
		t.Fatalf("MempoolTransactionByWTxID: %v", err)
	}
	if _, err := q.MempoolTransactionByTxID(ctx, fakeHash("readonly-mempool-missing"), false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MempoolTransactionByTxID(missing): %v", err)
	}

	afterCounts := snapshotDB(t, ctx, pool)
	if beforeCounts != afterCounts {
		t.Fatalf("query layer changed row counts:\nbefore = %+v\nafter  = %+v", beforeCounts, afterCounts)
	}

	afterContent := snapshotMutableContent(t, ctx, pool)
	if !reflect.DeepEqual(beforeContent, afterContent) {
		t.Fatalf("query layer mutated table CONTENT despite unchanged row counts:\nbefore = %+v\nafter  = %+v", beforeContent, afterContent)
	}
}
