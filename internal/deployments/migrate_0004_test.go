package deployments

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/mempool"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrationRoundTrip_0004PreservesExistingData is Phase 2H.1 spec
// section 39: schema v4 -> v3 -> v4 (0004/block_accounting's own round
// trip — the newest migration, so unlike 0003's round trip
// (migrate_test.go) it has nothing layered on top of it and Down(1)/Up
// target it directly), with REAL pre-existing confirmed-chain (including
// an ORPHANED block), mempool, and deployment-cache state seeded first.
//
// Every migration is applied up front, not just through v3, because
// internal/store.ApplyBlock (used below to seed real confirmed-chain
// fixtures) unconditionally writes a block_accounting row for every block
// it applies — 0004 must already exist for that to succeed at all. This
// is the honest reflection of Phase 2H.1's actual dependency: a database
// that has ever had ApplyBlock run against it after 0004 exists cannot
// meaningfully be "at v3" while containing confirmed-chain data.
func TestMigrationRoundTrip_0004PreservesExistingData(t *testing.T) {
	ctx := context.Background()
	pool := newTestSchema(t)
	migrations := migrationsFS(t)

	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("Up (all migrations): %v", err)
	}

	cstore := store.New(pool)
	mstore := mempool.NewStore(pool)
	dstore := NewStore(pool)

	g := block("mig0004-genesis", 0, "", coinbaseTx("mig0004-genesis", 100_00000000, "qMig0004Genesis"))
	if err := cstore.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock genesis: %v", err)
	}
	b1 := block("mig0004-block1", 1, g.Hash, coinbaseTx("mig0004-block1", 50_00000000, "qMig0004Block1"))
	if err := cstore.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("ApplyBlock block1: %v", err)
	}

	// An orphaned block: apply a second child of b1, roll back to b1, then
	// apply a replacement — the orphan's block_accounting row must remain
	// as audit history throughout the whole 0004 round trip below (spec
	// section 24/51).
	orphan := block("mig0004-orphan", 2, b1.Hash, coinbaseTx("mig0004-orphan", 50_00000000, "qMig0004Orphan"))
	if err := cstore.ApplyBlock(ctx, orphan); err != nil {
		t.Fatalf("ApplyBlock orphan: %v", err)
	}
	if err := cstore.RollbackTo(ctx, b1.Hash); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	replacement := block("mig0004-replacement", 2, b1.Hash, coinbaseTx("mig0004-replacement", 50_00000000, "qMig0004Replacement"))
	if err := cstore.ApplyBlock(ctx, replacement); err != nil {
		t.Fatalf("ApplyBlock replacement: %v", err)
	}

	mtx := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("mig0004-mempool"), 500, 1_700_000_000)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(2, replacement.Hash, mtx)); err != nil {
		t.Fatalf("mempool ReplaceSnapshot: %v", err)
	}

	if _, err := dstore.ReplaceSnapshot(ctx, candidateFor(2, replacement.Hash, fixedTime(),
		p2qpkDeployment("started", 0, p2qpkStartedFixture()))); err != nil {
		t.Fatalf("deployment ReplaceSnapshot: %v", err)
	}

	// Every pre-existing block, including the orphan, must already have a
	// block_accounting row from ApplyBlock itself. Fingerprinted on the
	// monetary columns only (not indexed_at, which legitimately gets a
	// fresh timestamp when backfill-accounting re-inserts a row later —
	// see the digest comparison at the end of this test).
	accountingBefore := blockAccountingMonetaryDigest(t, ctx, pool)
	confirmedBefore := fingerprintTables(t, ctx, pool, confirmedTables)
	mempoolBefore := fingerprintTables(t, ctx, pool, mempoolTables)
	deploymentBefore := fingerprintTables(t, ctx, pool, deploymentTablesForNonMutation)

	// v4 -> v3: roll back ONLY 0004. Unlike 0003's round trip, nothing is
	// layered on top of 0004, so Down(1) targets it directly.
	rolledBack, err := store.Down(ctx, pool, migrations, 1)
	if err != nil {
		t.Fatalf("Down(1): %v", err)
	}
	if len(rolledBack) != 1 || rolledBack[0] != 4 {
		t.Fatalf("Down(1) rolled back %v, want [4]", rolledBack)
	}
	if tableExistsIn(t, ctx, pool, "block_accounting") {
		t.Fatal("block_accounting table still present after rolling back 0004")
	}
	if !tableExistsIn(t, ctx, pool, "deployment_state") {
		t.Fatal("deployment_state table (owned by 0003) must survive rolling back 0004")
	}
	requireTablesUnchanged(t, ctx, pool, confirmedTables, confirmedBefore)
	requireTablesUnchanged(t, ctx, pool, mempoolTables, mempoolBefore)
	requireTablesUnchanged(t, ctx, pool, deploymentTablesForNonMutation, deploymentBefore)

	// v3 -> v4 again: reapply 0004. The table comes back EMPTY — DROP
	// TABLE really did remove the old rows, and recreating the table via
	// 0004's up.sql does not (and structurally cannot) restore them. This
	// is exactly why backfill-accounting exists (spec section 63) —
	// demonstrated concretely below.
	applied, err := store.Up(ctx, pool, migrations)
	if err != nil {
		t.Fatalf("Up (reapply 0004): %v", err)
	}
	if len(applied) != 1 || applied[0] != 4 {
		t.Fatalf("Up (reapply) applied %v, want [4]", applied)
	}
	if !tableExistsIn(t, ctx, pool, "block_accounting") {
		t.Fatal("block_accounting table missing after reapplying 0004")
	}
	requireTablesUnchanged(t, ctx, pool, confirmedTables, confirmedBefore)
	requireTablesUnchanged(t, ctx, pool, mempoolTables, mempoolBefore)
	requireTablesUnchanged(t, ctx, pool, deploymentTablesForNonMutation, deploymentBefore)

	var emptyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM block_accounting`).Scan(&emptyCount); err != nil {
		t.Fatalf("count block_accounting: %v", err)
	}
	if emptyCount != 0 {
		t.Fatalf("block_accounting count = %d after reapplying 0004, want 0 (rows are not restored by the migration itself)", emptyCount)
	}

	// backfill-accounting closes the loop: reconstructs the exact same
	// facts (including for the orphaned block) from the confirmed-chain
	// data that survived the whole round trip untouched.
	result, err := cstore.BackfillAccounting(ctx)
	if err != nil {
		t.Fatalf("BackfillAccounting: %v", err)
	}
	if result.TotalBlocks != 4 || result.Inserted != 4 {
		t.Fatalf("BackfillAccounting result = %+v, want TotalBlocks=4 Inserted=4 (genesis, block1, orphan, replacement)", result)
	}

	accountingAfter := blockAccountingMonetaryDigest(t, ctx, pool)
	if accountingAfter != accountingBefore {
		t.Errorf("block_accounting monetary content after backfill differs from before the round trip: before=%s after=%s",
			accountingBefore, accountingAfter)
	}
}

// blockAccountingMonetaryDigest fingerprints block_accounting's monetary
// columns only (block_hash, subsidy/fee/coinbase/unclaimed satoshis) —
// excluding indexed_at, which legitimately differs between the row
// ApplyBlock originally wrote and the row backfill-accounting later
// re-derives and re-inserts for the SAME block after 0004's table was
// dropped and recreated (each INSERT gets its own now() default; the
// monetary facts themselves are what must be identical, not the wall-clock
// moment they were (re)computed).
func blockAccountingMonetaryDigest(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var digest string
	err := pool.QueryRow(ctx, `
		SELECT coalesce(md5(string_agg(t::text, '|' ORDER BY t::text)), '')
		FROM (
			SELECT block_hash, subsidy_satoshis, fee_satoshis, coinbase_output_satoshis, unclaimed_reward_satoshis
			FROM block_accounting
		) t
	`).Scan(&digest)
	if err != nil {
		t.Fatalf("fingerprint block_accounting monetary columns: %v", err)
	}
	return digest
}
