package query

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dbSnapshot captures row counts + the durable checkpoint across every
// table a mutating write could touch, so a broad suite of read calls can be
// proven not to have moved any of them.
type dbSnapshot struct {
	syncState, blocks, txs, variants, blockTxs, inputs, witness, outputs, addrs, addrOut, participants, utxo int64
	indexedHeight                                                                                            int64
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
	if err := pool.QueryRow(ctx, "SELECT indexed_height FROM sync_state WHERE name = 'main'").Scan(&s.indexedHeight); err != nil {
		t.Fatalf("read sync_state: %v", err)
	}
	return s
}

// 29: normal API/query calls never mutate sync_state, canonical flags,
// utxo_state, or address caches — snapshot every mutable table before and
// after a broad suite of reads and require exact equality.
func TestReadOnlyEnforcement(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	f := buildTxFixture(t, ctx, st)

	before := snapshotDB(t, ctx, pool)

	// A broad read sweep: every exported query, including lookups for
	// data that doesn't exist (error paths must not write either).
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
	if _, err := q.TransactionByTxID(ctx, f.spendTxid, false); err != nil {
		t.Fatalf("TransactionByTxID: %v", err)
	}
	if _, err := q.TransactionByTxID(ctx, f.spendTxid, true); err != nil {
		t.Fatalf("TransactionByTxID(raw witness): %v", err)
	}
	if _, err := q.TransactionByWTxID(ctx, f.spendWtxid, false); err != nil {
		t.Fatalf("TransactionByWTxID: %v", err)
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

	after := snapshotDB(t, ctx, pool)
	if before != after {
		t.Fatalf("query layer mutated database state:\nbefore = %+v\nafter  = %+v", before, after)
	}
}
