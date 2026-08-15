package query

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// supplyRowCounts is a row-count fingerprint across every table
// SupplyOverview could conceivably touch or be affected by — mirrors
// readonly_test.go's dbSnapshot, extended with block_accounting and the
// deployment-cache tables (spec item 59). A coarse but cheap way to prove
// SupplyOverview genuinely never writes.
type supplyRowCounts struct {
	syncState, blocks, txs, variants, blockTxs, inputs, witness, outputs, addrs, addrOut, participants, utxo, accounting, rollup int64

	mempoolState, mempoolTxs, mempoolInputs, mempoolWitness, mempoolOutputs, mempoolAddrs, mempoolParticipants, mempoolDeps int64

	deploymentState, chainDeployments int64
}

func snapshotSupplyRowCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) supplyRowCounts {
	t.Helper()
	count := func(table string) int64 {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	return supplyRowCounts{
		syncState:    count("sync_state"),
		blocks:       count("blocks"),
		txs:          count("transactions"),
		variants:     count("transaction_variants"),
		blockTxs:     count("block_transactions"),
		inputs:       count("transaction_inputs"),
		witness:      count("transaction_input_witness"),
		outputs:      count("transaction_outputs"),
		addrs:        count("addresses"),
		addrOut:      count("output_addresses"),
		participants: count("output_participants"),
		utxo:         count("utxo_state"),
		accounting:   count("block_accounting"),
		rollup:       count("block_supply_rollup"),

		mempoolState:        count("mempool_state"),
		mempoolTxs:          count("mempool_transactions"),
		mempoolInputs:       count("mempool_inputs"),
		mempoolWitness:      count("mempool_input_witness"),
		mempoolOutputs:      count("mempool_outputs"),
		mempoolAddrs:        count("mempool_output_addresses"),
		mempoolParticipants: count("mempool_output_participants"),
		mempoolDeps:         count("mempool_dependencies"),

		deploymentState:  count("deployment_state"),
		chainDeployments: count("chain_deployments"),
	}
}

// TestSupplyOverview_NonMutation is spec item 44: repeated SupplyOverview
// calls — across an empty database, a populated one, and a rollup-
// unavailable error path — must never insert, delete, or update a single
// row anywhere in the schema. query.Store's read-only enforcement is
// already structural (every statement is a SELECT —
// docs/ARCHITECTURE.md §19), but this is a genuine end-to-end proof, not
// just a code-review claim.
func TestSupplyOverview_NonMutation(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	before := snapshotSupplyRowCounts(t, ctx, pool)
	if _, err := q.SupplyOverview(ctx); err != nil {
		t.Fatalf("SupplyOverview on empty database: %v", err)
	}
	if got := snapshotSupplyRowCounts(t, ctx, pool); got != before {
		t.Fatalf("row counts changed after SupplyOverview on empty database: before=%+v after=%+v", before, got)
	}

	g := block("supply-nonmut-g", 0, "", coinbaseTx("supply-nonmut-g", 100*qoge, "qSupplyNonMutG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("supply-nonmut-h1", 1, g.Hash, coinbaseTx("supply-nonmut-h1", 100*qoge, "qSupplyNonMutH1"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}

	before = snapshotSupplyRowCounts(t, ctx, pool)
	for i := 0; i < 3; i++ {
		if _, err := q.SupplyOverview(ctx); err != nil {
			t.Fatalf("SupplyOverview call %d: %v", i, err)
		}
	}
	if got := snapshotSupplyRowCounts(t, ctx, pool); got != before {
		t.Fatalf("row counts changed after repeated SupplyOverview calls: before=%+v after=%+v", before, got)
	}

	// The rollup-unavailable error path also must not mutate anything.
	if _, err := pool.Exec(ctx, "DELETE FROM block_supply_rollup WHERE block_hash = $1", h1.Hash); err != nil {
		t.Fatalf("delete block_supply_rollup for h1: %v", err)
	}
	before = snapshotSupplyRowCounts(t, ctx, pool)
	if _, err := q.SupplyOverview(ctx); err == nil {
		t.Fatal("SupplyOverview succeeded despite missing tip rollup")
	}
	if got := snapshotSupplyRowCounts(t, ctx, pool); got != before {
		t.Fatalf("row counts changed after the rollup-unavailable error path: before=%+v after=%+v", before, got)
	}
}
