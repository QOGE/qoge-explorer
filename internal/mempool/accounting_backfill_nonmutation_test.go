package mempool

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/store"
)

// mempoolTablesForNonMutation is every mempool-cache table a confirmed-side
// operation must never touch — the same set confirmed_nonmutation_test.go
// proves is untouched BY mempool writes, checked here in the opposite
// direction: proves internal/store's Phase 2H.1 accounting backfill never
// touches it either.
var mempoolTablesForNonMutation = []string{
	"mempool_state",
	"mempool_transactions",
	"mempool_inputs",
	"mempool_input_witness",
	"mempool_outputs",
	"mempool_output_addresses",
	"mempool_output_participants",
	"mempool_dependencies",
}

// TestBackfillAccounting_MempoolTablesUnaffected is Phase 2H.1 spec section
// 58: seed a real confirmed chain (via store.ApplyBlock) and a real mempool
// snapshot (via mempool.Store.ReplaceSnapshot), delete the block_accounting
// rows ApplyBlock already wrote to give BackfillAccounting real work, then
// require every mempool_* table to be byte-identical before and after.
func TestBackfillAccounting_MempoolTablesUnaffected(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	confirmed := store.New(pool)
	genesis := confirmedBlockFixture(t, ctx, "backfillnonmut-genesis", 0, "", "qBackfillNonMutGenesis", 100_00000000)
	if err := confirmed.ApplyBlock(ctx, genesis); err != nil {
		t.Fatalf("ApplyBlock(genesis): %v", err)
	}
	block1 := confirmedBlockFixture(t, ctx, "backfillnonmut-block1", 1, genesis.Hash, "qBackfillNonMutBlock1", 50_00000000)
	if err := confirmed.ApplyBlock(ctx, block1); err != nil {
		t.Fatalf("ApplyBlock(block1): %v", err)
	}

	mstore := NewStore(pool)
	txA := simpleCandidateTx("BackfillNonMut-A", fakeHash("prevBackfillNonMutA"), 10_00000000, 1000, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, candidateOf(1, block1.Hash, txA)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// Give BackfillAccounting real work to do, rather than trivially
	// verifying rows ApplyBlock already wrote.
	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting`); err != nil {
		t.Fatalf("simulate legacy state: %v", err)
	}

	before := make(map[string]string, len(mempoolTablesForNonMutation))
	for _, table := range mempoolTablesForNonMutation {
		before[table] = dumpTable(t, ctx, pool, table)
	}

	result, err := confirmed.BackfillAccounting(ctx)
	if err != nil {
		t.Fatalf("BackfillAccounting: %v", err)
	}
	if result.TotalBlocks != 2 || result.Inserted != 2 {
		t.Fatalf("fixture bug: BackfillAccounting result = %+v, want TotalBlocks=2 Inserted=2", result)
	}

	for _, table := range mempoolTablesForNonMutation {
		after := dumpTable(t, ctx, pool, table)
		if before[table] != after {
			t.Fatalf("mempool table %s changed after BackfillAccounting:\nbefore:\n%s\nafter:\n%s", table, before[table], after)
		}
	}
}
