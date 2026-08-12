package deployments

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func candidateFor(coreTipHeight int64, coreTipHash string, observedAt time.Time, deployments ...CandidateDeployment) Candidate {
	return Candidate{
		CoreTipHeight: coreTipHeight,
		CoreTipHash:   coreTipHash,
		ObservedAt:    observedAt,
		Deployments:   deployments,
	}
}

func p2qpkDeployment(status string, since int64, raw json.RawMessage) CandidateDeployment {
	return CandidateDeployment{Name: "p2qpk", Status: status, SinceHeight: since, RawJSON: raw}
}

// TestReplaceSnapshot_InitialPublication is spec item 28.A: an
// uninitialized deployment_state moves to generation 1 with the
// candidate's rows present and the correct anchor.
func TestReplaceSnapshot_InitialPublication(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, _ := newTestStores(t)

	before, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State (before): %v", err)
	}
	if before.Initialized {
		t.Fatalf("Initialized = true before any publication, want false")
	}

	anchorHash := fakeHash("initial")
	observedAt := fixedTime()
	gen, err := dstore.ReplaceSnapshot(ctx, candidateFor(10, anchorHash, observedAt,
		p2qpkDeployment("started", 0, p2qpkStartedFixture())))
	if err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	if gen != 1 {
		t.Errorf("generation = %d, want 1", gen)
	}

	after, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State (after): %v", err)
	}
	if !after.Initialized {
		t.Fatal("Initialized = false after successful publication, want true")
	}
	if after.Generation != 1 {
		t.Errorf("Generation = %d, want 1", after.Generation)
	}
	if after.CoreTipHeight == nil || *after.CoreTipHeight != 10 {
		t.Errorf("CoreTipHeight = %v, want 10", after.CoreTipHeight)
	}
	if after.CoreTipHash == nil || *after.CoreTipHash != anchorHash {
		t.Errorf("CoreTipHash = %v, want %s", after.CoreTipHash, anchorHash)
	}
	if after.DeploymentCount != 1 {
		t.Errorf("DeploymentCount = %d, want 1", after.DeploymentCount)
	}
	if after.ObservedAt == nil {
		t.Fatal("ObservedAt is nil after successful publication")
	}

	var status string
	var sinceHeight int64
	if err := dstore.pool.QueryRow(ctx, `SELECT status, since_height FROM chain_deployments WHERE name = 'p2qpk'`).Scan(&status, &sinceHeight); err != nil {
		t.Fatalf("query chain_deployments: %v", err)
	}
	if status != "started" {
		t.Errorf("chain_deployments.status = %q, want started", status)
	}
}

// TestReplaceSnapshot_ReplacesOldWithNew is spec item 28.B: snapshot A
// (p2qpk started) -> snapshot B (p2qpk locked_in); only B is visible
// after commit, and generation increments.
func TestReplaceSnapshot_ReplacesOldWithNew(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, pool := newTestStores(t)

	genA, err := dstore.ReplaceSnapshot(ctx, candidateFor(10, fakeHash("A"), fixedTime(),
		p2qpkDeployment("started", 0, p2qpkStartedFixture())))
	if err != nil {
		t.Fatalf("ReplaceSnapshot A: %v", err)
	}

	genB, err := dstore.ReplaceSnapshot(ctx, candidateFor(20, fakeHash("B"), fixedTime().Add(time.Hour),
		p2qpkDeployment("locked_in", 102_016, p2qpkLockedInFixture())))
	if err != nil {
		t.Fatalf("ReplaceSnapshot B: %v", err)
	}
	if genB != genA+1 {
		t.Errorf("generation B = %d, want %d", genB, genA+1)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_deployments`).Scan(&count); err != nil {
		t.Fatalf("count chain_deployments: %v", err)
	}
	if count != 1 {
		t.Fatalf("chain_deployments row count = %d, want 1 (only B visible)", count)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM chain_deployments WHERE name = 'p2qpk'`).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "locked_in" {
		t.Errorf("status = %q, want locked_in (only B's value should be visible)", status)
	}
}

// TestReplaceSnapshot_NonEmptyToEmpty is spec item 28.C: a successful
// zero-deployment observation is real synchronized state, not "nothing
// happened" — rows go empty, initialized stays true, generation
// increments.
func TestReplaceSnapshot_NonEmptyToEmpty(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, pool := newTestStores(t)

	genA, err := dstore.ReplaceSnapshot(ctx, candidateFor(10, fakeHash("A"), fixedTime(),
		p2qpkDeployment("started", 0, p2qpkStartedFixture())))
	if err != nil {
		t.Fatalf("ReplaceSnapshot A: %v", err)
	}

	genB, err := dstore.ReplaceSnapshot(ctx, candidateFor(20, fakeHash("B"), fixedTime().Add(time.Hour)))
	if err != nil {
		t.Fatalf("ReplaceSnapshot B (empty): %v", err)
	}
	if genB != genA+1 {
		t.Errorf("generation = %d, want %d", genB, genA+1)
	}

	after, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !after.Initialized {
		t.Error("Initialized = false after a successful empty snapshot, want true")
	}
	if after.DeploymentCount != 0 {
		t.Errorf("DeploymentCount = %d, want 0", after.DeploymentCount)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_deployments`).Scan(&count); err != nil {
		t.Fatalf("count chain_deployments: %v", err)
	}
	if count != 0 {
		t.Errorf("chain_deployments row count = %d, want 0", count)
	}
}

// TestReplaceSnapshot_EmptyToNonEmpty is spec item 28.D.
func TestReplaceSnapshot_EmptyToNonEmpty(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, pool := newTestStores(t)

	if _, err := dstore.ReplaceSnapshot(ctx, candidateFor(10, fakeHash("A"), fixedTime())); err != nil {
		t.Fatalf("ReplaceSnapshot A (empty): %v", err)
	}

	if _, err := dstore.ReplaceSnapshot(ctx, candidateFor(20, fakeHash("B"), fixedTime().Add(time.Hour),
		p2qpkDeployment("started", 0, p2qpkStartedFixture()))); err != nil {
		t.Fatalf("ReplaceSnapshot B (non-empty): %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_deployments`).Scan(&count); err != nil {
		t.Fatalf("count chain_deployments: %v", err)
	}
	if count != 1 {
		t.Errorf("chain_deployments row count = %d, want 1", count)
	}
}

// TestReplaceSnapshot_FailedReplacementPreservesPrevious is spec item
// 28.E: a failure that occurs AFTER deletion/insertion has begun must
// roll back completely — the old snapshot rows and old deployment_state
// remain exactly as they were, and generation is unchanged. Triggered by
// a candidate whose second deployment carries syntactically invalid JSON
// bytes as RawJSON (passes Go-level validate(), which only checks
// len>0, but PostgreSQL's JSONB column type rejects it at INSERT time) —
// this proves the failure happens mid-transaction, after the first
// (valid) row's INSERT and the DELETE of the previous snapshot have
// already run inside the same still-uncommitted transaction.
func TestReplaceSnapshot_FailedReplacementPreservesPrevious(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, pool := newTestStores(t)

	goodAnchor := fakeHash("good")
	goodObservedAt := fixedTime()
	genGood, err := dstore.ReplaceSnapshot(ctx, candidateFor(10, goodAnchor, goodObservedAt,
		p2qpkDeployment("started", 0, p2qpkStartedFixture())))
	if err != nil {
		t.Fatalf("ReplaceSnapshot (good): %v", err)
	}

	badCandidate := candidateFor(20, fakeHash("bad"), fixedTime().Add(time.Hour),
		CandidateDeployment{Name: "aaa_first", Status: "started", SinceHeight: 0, RawJSON: p2qpkStartedFixture()},
		CandidateDeployment{Name: "zzz_second", Status: "started", SinceHeight: 0, RawJSON: json.RawMessage("not-json")},
	)
	if _, err := dstore.ReplaceSnapshot(ctx, badCandidate); err == nil {
		t.Fatal("ReplaceSnapshot (bad): expected error, got nil")
	}

	after, err := dstore.State(ctx)
	if err != nil {
		t.Fatalf("State (after failed replacement): %v", err)
	}
	if after.Generation != genGood {
		t.Errorf("Generation = %d after failed replacement, want unchanged %d", after.Generation, genGood)
	}
	if after.CoreTipHash == nil || *after.CoreTipHash != goodAnchor {
		t.Errorf("CoreTipHash = %v after failed replacement, want unchanged %s", after.CoreTipHash, goodAnchor)
	}
	if after.DeploymentCount != 1 {
		t.Errorf("DeploymentCount = %d after failed replacement, want unchanged 1", after.DeploymentCount)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_deployments`).Scan(&count); err != nil {
		t.Fatalf("count chain_deployments: %v", err)
	}
	if count != 1 {
		t.Fatalf("chain_deployments row count = %d after failed replacement, want unchanged 1", count)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM chain_deployments WHERE name = 'p2qpk'`).Scan(&status); err != nil {
		t.Fatalf("query surviving row: %v", err)
	}
	if status != "started" {
		t.Errorf("surviving status = %q, want started (the pre-failure snapshot)", status)
	}

	// Neither of the bad candidate's rows leaked in, including the one
	// whose own INSERT would have (illegally) succeeded before the
	// second row's failure aborted the transaction.
	var leaked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chain_deployments WHERE name IN ('aaa_first', 'zzz_second')`).Scan(&leaked); err != nil {
		t.Fatalf("count leaked rows: %v", err)
	}
	if leaked != 0 {
		t.Errorf("leaked = %d rows from the failed candidate, want 0", leaked)
	}
}

// TestReplaceSnapshot_RawJSONDBRoundTrip is spec item 29 at the storage
// layer: persisting a realistic BIP9 object through ReplaceSnapshot and
// reading raw_json back from PostgreSQL (JSONB canonicalizes key
// ordering, which is acceptable — spec item 11) must preserve semantic
// equality with the original, with no field loss.
func TestReplaceSnapshot_RawJSONDBRoundTrip(t *testing.T) {
	ctx := context.Background()
	dstore, _, _, pool := newTestStores(t)

	original := p2qpkStartedFixture()
	if _, err := dstore.ReplaceSnapshot(ctx, candidateFor(10, fakeHash("tip"), fixedTime(),
		p2qpkDeployment("started", 100_000, original))); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	var rawFromDB []byte
	if err := pool.QueryRow(ctx, `SELECT raw_json FROM chain_deployments WHERE name = 'p2qpk'`).Scan(&rawFromDB); err != nil {
		t.Fatalf("query raw_json: %v", err)
	}

	var want, got map[string]interface{}
	if err := json.Unmarshal(original, &want); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(rawFromDB, &got); err != nil {
		t.Fatalf("unmarshal db raw_json: %v", err)
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("raw_json round trip not semantically equal:\n want %s\n got  %s", wantJSON, gotJSON)
	}
}

// tableFingerprint captures both the row count and an order-independent
// content digest of table, so a "before" and "after" fingerprint can
// prove a table's contents are byte-for-byte unchanged (spec items
// 34/35) without needing to hand-write a full row-shape comparison.
func tableFingerprint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) (count int, digest string) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT count(*), coalesce(md5(string_agg(t::text, '|' ORDER BY t::text)), '') FROM `+table+` t`).Scan(&count, &digest)
	if err != nil {
		t.Fatalf("fingerprint table %s: %v", table, err)
	}
	return count, digest
}

var confirmedTables = []string{
	"sync_state", "blocks", "transactions", "transaction_variants",
	"block_transactions", "transaction_inputs", "transaction_input_witness",
	"transaction_outputs", "output_addresses", "output_participants",
	"utxo_state", "addresses",
}

var mempoolTables = []string{
	"mempool_state", "mempool_transactions", "mempool_inputs",
	"mempool_input_witness", "mempool_outputs", "mempool_output_addresses",
	"mempool_output_participants", "mempool_dependencies",
}

// TestReplaceSnapshot_ConfirmedTablesUnaffected is spec item 34: seeding
// real confirmed chain state via internal/store.Store.ApplyBlock, then
// running a deployment snapshot replacement, must leave every confirmed
// table byte-for-byte unchanged.
func TestReplaceSnapshot_ConfirmedTablesUnaffected(t *testing.T) {
	ctx := context.Background()
	dstore, cstore, _, pool := newTestStores(t)

	g := block("confirmed-nonmutation-genesis", 0, "", coinbaseTx("confirmed-nonmutation-genesis", 100_00000000, "qDeployConfirmedNonMutation"))
	if err := cstore.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}

	before := make(map[string]struct {
		count  int
		digest string
	}, len(confirmedTables))
	for _, table := range confirmedTables {
		c, d := tableFingerprint(t, ctx, pool, table)
		before[table] = struct {
			count  int
			digest string
		}{c, d}
	}

	if _, err := dstore.ReplaceSnapshot(ctx, candidateFor(0, g.Hash, fixedTime(),
		p2qpkDeployment("started", 0, p2qpkStartedFixture()))); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	for _, table := range confirmedTables {
		c, d := tableFingerprint(t, ctx, pool, table)
		if c != before[table].count || d != before[table].digest {
			t.Errorf("table %s changed after deployment ReplaceSnapshot: count %d->%d, digest %s->%s",
				table, before[table].count, c, before[table].digest, d)
		}
	}
}

// TestReplaceSnapshot_MempoolTablesUnaffected is spec item 35: seeding a
// real Phase 2F mempool snapshot via internal/mempool.Store.ReplaceSnapshot,
// then running a deployment snapshot replacement, must leave every
// mempool_* table byte-for-byte unchanged.
func TestReplaceSnapshot_MempoolTablesUnaffected(t *testing.T) {
	ctx := context.Background()
	dstore, _, mstore, pool := newTestStores(t)

	mtx := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("deploy-mempool-nonmutation"), 1000, 1_700_000_000)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(0, fakeHash("mempool-nonmutation-tip"), mtx)); err != nil {
		t.Fatalf("mempool ReplaceSnapshot: %v", err)
	}

	before := make(map[string]struct {
		count  int
		digest string
	}, len(mempoolTables))
	for _, table := range mempoolTables {
		c, d := tableFingerprint(t, ctx, pool, table)
		before[table] = struct {
			count  int
			digest string
		}{c, d}
	}

	if _, err := dstore.ReplaceSnapshot(ctx, candidateFor(0, fakeHash("deploy-tip"), fixedTime(),
		p2qpkDeployment("started", 0, p2qpkStartedFixture()))); err != nil {
		t.Fatalf("deployments ReplaceSnapshot: %v", err)
	}

	for _, table := range mempoolTables {
		c, d := tableFingerprint(t, ctx, pool, table)
		if c != before[table].count || d != before[table].digest {
			t.Errorf("table %s changed after deployment ReplaceSnapshot: count %d->%d, digest %s->%s",
				table, before[table].count, c, before[table].digest, d)
		}
	}
}
