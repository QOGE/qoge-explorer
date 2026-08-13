package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// accountingNonMutationTables is every confirmed-chain table (spec section
// 57's explicit list) that BackfillAccounting must never modify — it only
// ever reads them, and writes exclusively to block_accounting itself.
var accountingNonMutationTables = []string{
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

// dumpTableForNonMutation renders table's entire content as one string —
// an exact content-level snapshot, not just a row count — so a changed
// column value, not only an added/removed row, is caught.
func dumpTableForNonMutation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) string {
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

// TestBackfillAccounting_ConfirmedTablesUnaffected is Phase 2H.1 spec
// section 57: seed real confirmed-chain state via ApplyBlock, delete the
// block_accounting rows ApplyBlock already wrote (to give BackfillAccounting
// real work rather than a trivial re-verify pass), and require every OTHER
// confirmed table to be byte-identical before and after.
func TestBackfillAccounting_ConfirmedTablesUnaffected(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("acctNM0"), 100, "", coinbaseTx(hash64("acctNM0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)
	b := testBlock(hash64("acctNM1"), 101, hash64("acctNM0"), coinbaseTx(hash64("acctNM1tx"), out(0, era0Subsidy, "qBob")))
	mustApply(t, ctx, s, b)

	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting`); err != nil {
		t.Fatalf("simulate legacy state: %v", err)
	}

	before := make(map[string]string, len(accountingNonMutationTables))
	for _, table := range accountingNonMutationTables {
		before[table] = dumpTableForNonMutation(t, ctx, pool, table)
	}

	result, err := s.BackfillAccounting(ctx)
	if err != nil {
		t.Fatalf("BackfillAccounting: %v", err)
	}
	if result.TotalBlocks != 2 || result.Inserted != 2 {
		t.Fatalf("fixture bug: BackfillAccounting result = %+v, want TotalBlocks=2 Inserted=2", result)
	}

	for _, table := range accountingNonMutationTables {
		after := dumpTableForNonMutation(t, ctx, pool, table)
		if before[table] != after {
			t.Fatalf("confirmed table %s changed after BackfillAccounting:\nbefore:\n%s\nafter:\n%s", table, before[table], after)
		}
	}
}
