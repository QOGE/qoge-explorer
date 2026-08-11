package mempool

import (
	"context"
	"fmt"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dumpTable renders every row of table, ordered by every column (for a
// stable, repeatable serialization), as one string — an exact
// content-level snapshot, not just a row count. Comparing two dumps of
// the same table catches any change: an added/removed/modified row, or a
// column value silently drifting.
func dumpTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) string {
	t.Helper()

	var colCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1
	`, table).Scan(&colCount); err != nil {
		t.Fatalf("count columns of %s: %v", table, err)
	}

	orderBy := ""
	for i := 1; i <= colCount; i++ {
		if i > 1 {
			orderBy += ", "
		}
		orderBy += fmt.Sprintf("%d", i)
	}

	rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT * FROM %s ORDER BY %s`, table, orderBy))
	if err != nil {
		t.Fatalf("dump table %s: %v", table, err)
	}
	defer rows.Close()

	out := ""
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("dump table %s: read row: %v", table, err)
		}
		out += fmt.Sprintf("%v\n", vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump table %s: iterate: %v", table, err)
	}
	return out
}

// confirmedTables is every confirmed-chain table a mempool write must
// NEVER touch (spec item 38's explicit list, minus utxo_state/chain
// deployments — utxo_state is included below; chain_deployments is
// unrelated to block/tx indexing and out of scope for this fixture).
var confirmedTables = []string{
	"sync_state",
	"blocks",
	"transactions",
	"transaction_variants",
	"block_transactions",
	"transaction_inputs",
	"transaction_input_witness",
	"transaction_outputs",
	"output_addresses",
	"output_participants",
	"utxo_state",
	"addresses",
}

func dumpConfirmedTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	dumps := make(map[string]string, len(confirmedTables))
	for _, table := range confirmedTables {
		dumps[table] = dumpTable(t, ctx, pool, table)
	}
	return dumps
}

// TestConfirmedState_UnaffectedByMempoolReplacement is spec item 38: seed
// a real confirmed chain through store.ApplyBlock, capture EXACT
// (content-level, not just row-count) confirmed state, run a successful
// mempool ReplaceSnapshot, and require every confirmed table listed above
// to be byte-identical afterward.
func TestConfirmedState_UnaffectedByMempoolReplacement(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	confirmed := store.New(pool)
	genesis := confirmedBlockFixture(t, ctx, "nonmut-genesis", 0, "", "qNonMutGenesis", 100_00000000)
	if err := confirmed.ApplyBlock(ctx, genesis); err != nil {
		t.Fatalf("ApplyBlock(genesis): %v", err)
	}
	block1 := confirmedBlockFixture(t, ctx, "nonmut-block1", 1, genesis.Hash, "qNonMutBlock1", 50_00000000)
	if err := confirmed.ApplyBlock(ctx, block1); err != nil {
		t.Fatalf("ApplyBlock(block1): %v", err)
	}

	before := dumpConfirmedTables(t, ctx, pool)

	mstore := NewStore(pool)
	txA := simpleCandidateTx("NonMut-A", fakeHash("prevNonMutA"), 10_00000000, 1000, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, candidateOf(1, block1.Hash, txA)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	// A second replacement (including an empty one) exercises the DELETE
	// path too, not just INSERT — both must leave confirmed state alone.
	if _, err := mstore.ReplaceSnapshot(ctx, candidateOf(1, block1.Hash)); err != nil {
		t.Fatalf("ReplaceSnapshot (empty): %v", err)
	}

	after := dumpConfirmedTables(t, ctx, pool)

	for _, table := range confirmedTables {
		if before[table] != after[table] {
			t.Fatalf("confirmed table %s changed after mempool ReplaceSnapshot calls:\nbefore:\n%s\nafter:\n%s", table, before[table], after[table])
		}
	}

	tip, err := confirmed.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Height != 1 || tip.Hash != block1.Hash {
		t.Fatalf("confirmed tip = %+v, want height=1 hash=%s", tip, block1.Hash)
	}
}
