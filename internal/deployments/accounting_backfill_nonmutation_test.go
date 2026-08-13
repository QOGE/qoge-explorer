package deployments

import (
	"context"
	"testing"
)

// deploymentTablesForNonMutation is every deployment-cache table a
// confirmed-side operation must never touch.
var deploymentTablesForNonMutation = []string{"deployment_state", "chain_deployments"}

// TestBackfillAccounting_DeploymentTablesUnaffected is Phase 2H.1 spec
// section 59: seed a real confirmed chain (via store.ApplyBlock) and a
// real deployment cache (via deployments.Store.ReplaceSnapshot), delete
// the block_accounting rows ApplyBlock already wrote to give
// BackfillAccounting real work, then require deployment_state and
// chain_deployments to be byte-identical before and after.
func TestBackfillAccounting_DeploymentTablesUnaffected(t *testing.T) {
	ctx := context.Background()
	dstore, cstore, _, pool := newTestStores(t)

	g := block("backfillnonmut-genesis", 0, "", coinbaseTx("backfillnonmut-genesis", 100_00000000, "qBackfillDeployNonMut"))
	if err := cstore.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}

	if _, err := dstore.ReplaceSnapshot(ctx, candidateFor(0, g.Hash, fixedTime(),
		p2qpkDeployment("started", 0, p2qpkStartedFixture()))); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM block_accounting`); err != nil {
		t.Fatalf("simulate legacy state: %v", err)
	}

	before := make(map[string]tableFP, len(deploymentTablesForNonMutation))
	for _, table := range deploymentTablesForNonMutation {
		count, digest := tableFingerprint(t, ctx, pool, table)
		before[table] = tableFP{count, digest}
	}

	result, err := cstore.BackfillAccounting(ctx)
	if err != nil {
		t.Fatalf("BackfillAccounting: %v", err)
	}
	if result.TotalBlocks != 1 || result.Inserted != 1 {
		t.Fatalf("fixture bug: BackfillAccounting result = %+v, want TotalBlocks=1 Inserted=1", result)
	}

	for _, table := range deploymentTablesForNonMutation {
		count, digest := tableFingerprint(t, ctx, pool, table)
		if before[table] != (tableFP{count, digest}) {
			t.Fatalf("deployment table %s changed after BackfillAccounting: before=%+v after=%+v", table, before[table], tableFP{count, digest})
		}
	}
}
