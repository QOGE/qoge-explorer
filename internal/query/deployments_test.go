package query

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/QOGE/qoge-explorer/internal/deployments"
)

// TestDeploymentOverview_Uninitialized is spec item 42: a freshly migrated
// database (deployment_state.initialized = false, the migration's bootstrap
// row) must report status=uninitialized and an EMPTY (never nil, never a
// synthesized non-empty) deployment list, and DeploymentByName must refuse
// to claim any name doesn't exist.
func TestDeploymentOverview_Uninitialized(t *testing.T) {
	ctx := context.Background()
	q, _, _ := newTestQueryStore(t)

	overview, err := q.DeploymentOverview(ctx)
	if err != nil {
		t.Fatalf("DeploymentOverview: %v", err)
	}
	if overview.State.Initialized {
		t.Fatalf("State.Initialized = true, want false")
	}
	if overview.State.Status != "uninitialized" {
		t.Fatalf("State.Status = %q, want \"uninitialized\"", overview.State.Status)
	}
	if overview.State.Stale {
		t.Fatalf("State.Stale = true, want false for an uninitialized cache")
	}
	if len(overview.Deployments) != 0 {
		t.Fatalf("Deployments = %+v, want empty", overview.Deployments)
	}

	_, err = q.DeploymentByName(ctx, "p2qpk")
	if !errors.Is(err, ErrDeploymentCacheUninitialized) {
		t.Fatalf("DeploymentByName error = %v, want ErrDeploymentCacheUninitialized", err)
	}
}

// TestDeploymentOverview_InitializedEmpty is spec item 43: a real
// Store.ReplaceSnapshot with zero deployments is a DIFFERENT state from
// uninitialized — Initialized=true, DeploymentCount=0, an empty (not nil)
// deployment list.
func TestDeploymentOverview_InitializedEmpty(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("empty-tip"), observedAt)); err != nil {
		t.Fatalf("ReplaceSnapshot(empty): %v", err)
	}

	overview, err := q.DeploymentOverview(ctx)
	if err != nil {
		t.Fatalf("DeploymentOverview: %v", err)
	}
	if !overview.State.Initialized {
		t.Fatalf("State.Initialized = false, want true")
	}
	if overview.State.DeploymentCount != 0 {
		t.Fatalf("State.DeploymentCount = %d, want 0", overview.State.DeploymentCount)
	}
	if len(overview.Deployments) != 0 {
		t.Fatalf("Deployments = %+v, want empty", overview.Deployments)
	}

	_, err = q.DeploymentByName(ctx, "p2qpk")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeploymentByName error = %v, want ErrNotFound (initialized but absent)", err)
	}
}

// TestDeploymentByName_NotFoundInInitializedSnapshot confirms a name absent
// from an initialized, non-empty snapshot is ErrNotFound, never
// ErrDeploymentCacheUninitialized.
func TestDeploymentByName_NotFoundInInitializedSnapshot(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("nf-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	_, err := q.DeploymentByName(ctx, "taproot")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeploymentByName(taproot) error = %v, want ErrNotFound", err)
	}
}

// statusFixture pairs a BIP9 status/status_next/since triple with the raw
// JSON builder for that status, for the table-driven fixture test below.
type statusFixture struct {
	name       string
	status     string
	statusNext string
	since      int64
	raw        json.RawMessage
}

// TestDeploymentOverview_StatusFixtures is spec items 44-47: DEFINED,
// STARTED, LOCKED_IN, ACTIVE, and FAILED must all decode and expose their
// Core-reported fields correctly — in particular ACTIVE (spec item 46),
// which must render correctly with bit/statistics/signalling all
// legitimately absent, proving the real P2QPK mainnet transition to ACTIVE
// will not require a code change merely because those fields disappear.
func TestDeploymentOverview_StatusFixtures(t *testing.T) {
	fixtures := []statusFixture{
		{"defined", "defined", "defined", 0, p2qpkDefinedFixture()},
		{"started", "started", "started", 100_000, p2qpkStartedFixture()},
		{"locked_in", "locked_in", "locked_in", 102_016, p2qpkLockedInFixture()},
		{"active", "active", "active", 104_032, p2qpkActiveFixture()},
		{"failed", "failed", "failed", 102_016, p2qpkFailedFixture()},
	}

	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	var deps []deployments.CandidateDeployment
	for _, f := range fixtures {
		deps = append(deps, deploymentCandidateOf(f.name, f.status, f.since, f.raw))
	}
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("fixtures-tip"), observedAt, deps...)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	overview, err := q.DeploymentOverview(ctx)
	if err != nil {
		t.Fatalf("DeploymentOverview: %v", err)
	}
	if len(overview.Deployments) != len(fixtures) {
		t.Fatalf("Deployments count = %d, want %d", len(overview.Deployments), len(fixtures))
	}
	// Overview orders name ASC; all fixture names are distinct single words
	// here, so build a lookup instead of depending on sort order in
	// assertions below.
	byName := map[string]Deployment{}
	for _, d := range overview.Deployments {
		byName[d.Name] = d
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			d, ok := byName[f.name]
			if !ok {
				t.Fatalf("deployment %q missing from overview", f.name)
			}
			if d.Status != f.status || d.StatusNext != f.statusNext {
				t.Fatalf("status/status_next = %s/%s, want %s/%s", d.Status, d.StatusNext, f.status, f.statusNext)
			}
			if d.Since != f.since {
				t.Fatalf("since = %d, want %d", d.Since, f.since)
			}
			if d.Type != "bip9" {
				t.Fatalf("type = %q, want \"bip9\"", d.Type)
			}

			// Also fetch through DeploymentByName and require the same
			// values (both entry points decode identically).
			detail, err := q.DeploymentByName(ctx, f.name)
			if err != nil {
				t.Fatalf("DeploymentByName(%s): %v", f.name, err)
			}
			if detail.Deployment.Status != f.status {
				t.Fatalf("detail status = %q, want %q", detail.Deployment.Status, f.status)
			}
		})
	}

	active := byName["active"]
	if active.Bit != nil {
		t.Fatalf("active.Bit = %v, want nil (Core omits bit once ACTIVE)", active.Bit)
	}
	if active.Statistics != nil {
		t.Fatalf("active.Statistics = %+v, want nil (Core omits statistics once ACTIVE)", active.Statistics)
	}
	if active.Signalling != nil {
		t.Fatalf("active.Signalling = %v, want nil (Core omits signalling once ACTIVE)", active.Signalling)
	}
	if active.ActivationHeight == nil || *active.ActivationHeight != 104_032 {
		t.Fatalf("active.ActivationHeight = %v, want 104032", active.ActivationHeight)
	}
	if !active.Active {
		t.Fatalf("active.Active = false, want true")
	}

	started := byName["started"]
	if started.Statistics == nil {
		t.Fatalf("started.Statistics = nil, want present")
	}
	if started.Statistics.Threshold == nil || *started.Statistics.Threshold != 1815 {
		t.Fatalf("started.Statistics.Threshold = %v, want 1815", started.Statistics.Threshold)
	}

	lockedIn := byName["locked_in"]
	if lockedIn.Bit == nil {
		t.Fatalf("locked_in.Bit = nil, want present (has_signal is true for LOCKED_IN)")
	}
	if lockedIn.Statistics == nil {
		t.Fatalf("locked_in.Statistics = nil, want present")
	}
	if lockedIn.Statistics.Threshold != nil {
		t.Fatalf("locked_in.Statistics.Threshold = %v, want nil (legitimately absent once LOCKED_IN)", lockedIn.Statistics.Threshold)
	}
	if lockedIn.Statistics.Possible != nil {
		t.Fatalf("locked_in.Statistics.Possible = %v, want nil (legitimately absent once LOCKED_IN)", lockedIn.Statistics.Possible)
	}
	if lockedIn.Signalling == nil {
		t.Fatalf("locked_in.Signalling = nil, want present (has_signal is true for LOCKED_IN)")
	}

	defined := byName["defined"]
	if defined.Bit != nil || defined.Statistics != nil || defined.Signalling != nil {
		t.Fatalf("defined = %+v, want bit/statistics/signalling all nil (has_signal is false)", defined)
	}

	failed := byName["failed"]
	if failed.Bit != nil || failed.Statistics != nil || failed.Signalling != nil {
		t.Fatalf("failed = %+v, want bit/statistics/signalling all nil (has_signal is false)", failed)
	}
}

// TestDeploymentOverview_LockedInActivatingBoundary is spec items 6/7: the
// LOCKED_IN -> ACTIVE transition boundary (current_state LOCKED_IN,
// next_state ACTIVE) must be reported by the query layer exactly as Core
// sent it — Active true and ActivationHeight present while Status is
// still "locked_in" — proving the query never derives Active from Status.
// It must also still carry Bit/Statistics/Signalling, since has_signal is
// governed by current_state, which is still LOCKED_IN here.
func TestDeploymentOverview_LockedInActivatingBoundary(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "locked_in", 102_016, p2qpkLockedInActivatingFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(104_032, fakeHash("lockedin-activating-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	detail, err := q.DeploymentByName(ctx, "p2qpk")
	if err != nil {
		t.Fatalf("DeploymentByName: %v", err)
	}
	d := detail.Deployment
	if d.Status != "locked_in" {
		t.Fatalf("Status = %q, want locked_in", d.Status)
	}
	if d.StatusNext != "active" {
		t.Fatalf("StatusNext = %q, want active", d.StatusNext)
	}
	if !d.Active {
		t.Fatalf("Active = false, want true")
	}
	if d.ActivationHeight == nil || *d.ActivationHeight != 104_032 {
		t.Fatalf("ActivationHeight = %v, want 104032", d.ActivationHeight)
	}
	if d.Bit == nil {
		t.Fatalf("Bit = nil, want present (current_state is still LOCKED_IN)")
	}
	if d.Statistics == nil {
		t.Fatalf("Statistics = nil, want present (current_state is still LOCKED_IN)")
	}
	if d.Statistics.Threshold != nil || d.Statistics.Possible != nil {
		t.Fatalf("Statistics = %+v, want Threshold/Possible nil (LOCKED_IN semantics)", d.Statistics)
	}
	if d.Signalling == nil {
		t.Fatalf("Signalling = nil, want present (current_state is still LOCKED_IN)")
	}

	overview, err := q.DeploymentOverview(ctx)
	if err != nil {
		t.Fatalf("DeploymentOverview: %v", err)
	}
	if len(overview.Deployments) != 1 {
		t.Fatalf("Deployments = %+v, want exactly 1", overview.Deployments)
	}
	o := overview.Deployments[0]
	if o.Status != "locked_in" || o.StatusNext != "active" || !o.Active || o.ActivationHeight == nil {
		t.Fatalf("overview deployment = %+v, want status=locked_in status_next=active active=true activation_height present", o)
	}
}

// TestDeploymentOverview_RawJSONSemanticRoundTrip is spec item 48: the
// persisted raw_json, read back through PostgreSQL's JSONB storage (which
// does not preserve original byte-for-byte formatting), must remain
// SEMANTICALLY identical to what was written — verified by decoding both
// into a generic map and comparing, never by asserting exact byte equality.
func TestDeploymentOverview_RawJSONSemanticRoundTrip(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	original := p2qpkStartedFixture()
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, original)
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("rawrt-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	detail, err := q.DeploymentByName(ctx, "p2qpk")
	if err != nil {
		t.Fatalf("DeploymentByName: %v", err)
	}

	var wantAny, gotAny any
	if err := json.Unmarshal(original, &wantAny); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(detail.Deployment.Raw, &gotAny); err != nil {
		t.Fatalf("unmarshal returned raw: %v", err)
	}
	if !reflect.DeepEqual(wantAny, gotAny) {
		t.Fatalf("raw JSON round trip mismatch:\nwant %#v\ngot  %#v", wantAny, gotAny)
	}
}

// TestDeploymentState_Fresh is spec item 41.A: the deployment anchor
// matching the confirmed indexed tip exactly (same height AND hash) is
// fresh.
func TestDeploymentState_Fresh(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	g := block("depfresh-genesis", 0, "", coinbaseTx("depfresh-genesis", 100_00000000, "qDepFreshGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(0, g.Hash, observedAt)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	overview, err := q.DeploymentOverview(ctx)
	if err != nil {
		t.Fatalf("DeploymentOverview: %v", err)
	}
	if overview.State.Status != "fresh" || overview.State.Stale {
		t.Fatalf("State = %+v, want status=fresh stale=false", overview.State)
	}
}

// TestDeploymentState_SameHeightReorgIsStale is spec item 41.B: an anchor
// at the same height as the confirmed tip but a DIFFERENT hash is stale —
// this is a same-height canonical reorg, not "advancement", and must be
// reported identically to any other mismatch.
func TestDeploymentState_SameHeightReorgIsStale(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	g := block("depreorg-genesis", 0, "", coinbaseTx("depreorg-genesis", 100_00000000, "qDepReorgGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	// Anchor at height 0 but a hash that is NOT the confirmed tip's hash.
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(0, fakeHash("depreorg-other"), observedAt)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	overview, err := q.DeploymentOverview(ctx)
	if err != nil {
		t.Fatalf("DeploymentOverview: %v", err)
	}
	if overview.State.Status != "stale" || !overview.State.Stale {
		t.Fatalf("State = %+v, want status=stale stale=true", overview.State)
	}
}

// TestDeploymentState_DifferentHeightIsStale is spec item 41.C: an anchor
// at a different height than the confirmed tip is stale, with no direction
// (ahead/behind) implied by this package.
func TestDeploymentState_DifferentHeightIsStale(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	g := block("depheight-genesis", 0, "", coinbaseTx("depheight-genesis", 100_00000000, "qDepHeightGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(5, fakeHash("depheight-other"), observedAt)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	overview, err := q.DeploymentOverview(ctx)
	if err != nil {
		t.Fatalf("DeploymentOverview: %v", err)
	}
	if overview.State.Status != "stale" || !overview.State.Stale {
		t.Fatalf("State = %+v, want status=stale stale=true", overview.State)
	}
}

// ── Cache integrity drift tests (spec items 14/49/50/51/52) ──────────────
//
// Every test below seeds a VALID snapshot through the real
// deployments.Store, then uses direct test SQL ONLY to simulate database
// corruption a real writer could never produce. Production code must never
// silently pick one side of a disagreement.

func TestDeploymentIntegrity_StatusDrift(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("statusdrift-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE chain_deployments SET status = 'locked_in' WHERE name = 'p2qpk'`); err != nil {
		t.Fatalf("corrupt status: %v", err)
	}

	_, err := q.DeploymentByName(ctx, "p2qpk")
	if !errors.Is(err, ErrDeploymentCacheIntegrity) {
		t.Fatalf("DeploymentByName error = %v, want ErrDeploymentCacheIntegrity", err)
	}

	_, err = q.DeploymentOverview(ctx)
	if !errors.Is(err, ErrDeploymentCacheIntegrity) {
		t.Fatalf("DeploymentOverview error = %v, want ErrDeploymentCacheIntegrity", err)
	}
}

func TestDeploymentIntegrity_SinceDrift(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("sincedrift-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE chain_deployments SET since_height = since_height + 1 WHERE name = 'p2qpk'`); err != nil {
		t.Fatalf("corrupt since_height: %v", err)
	}

	_, err := q.DeploymentByName(ctx, "p2qpk")
	if !errors.Is(err, ErrDeploymentCacheIntegrity) {
		t.Fatalf("DeploymentByName error = %v, want ErrDeploymentCacheIntegrity", err)
	}
}

func TestDeploymentIntegrity_SinceNullDrift(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("sincenull-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE chain_deployments SET since_height = NULL WHERE name = 'p2qpk'`); err != nil {
		t.Fatalf("corrupt since_height to NULL: %v", err)
	}

	_, err := q.DeploymentByName(ctx, "p2qpk")
	if !errors.Is(err, ErrDeploymentCacheIntegrity) {
		t.Fatalf("DeploymentByName error = %v, want ErrDeploymentCacheIntegrity", err)
	}
}

func TestDeploymentIntegrity_TypeDrift(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("typedrift-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE chain_deployments SET raw_json = jsonb_set(raw_json, '{type}', '"buried"') WHERE name = 'p2qpk'`); err != nil {
		t.Fatalf("corrupt raw_json.type: %v", err)
	}

	_, err := q.DeploymentByName(ctx, "p2qpk")
	if !errors.Is(err, ErrDeploymentCacheIntegrity) {
		t.Fatalf("DeploymentByName error = %v, want ErrDeploymentCacheIntegrity", err)
	}
}

func TestDeploymentIntegrity_CheckedAtDrift(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("checkedatdrift-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE chain_deployments SET checked_at = checked_at + interval '1 second' WHERE name = 'p2qpk'`); err != nil {
		t.Fatalf("corrupt checked_at: %v", err)
	}

	_, err := q.DeploymentByName(ctx, "p2qpk")
	if !errors.Is(err, ErrDeploymentCacheIntegrity) {
		t.Fatalf("DeploymentByName error = %v, want ErrDeploymentCacheIntegrity", err)
	}
}

func TestDeploymentIntegrity_CountDrift(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("countdrift-tip"), observedAt, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE deployment_state SET deployment_count = 2 WHERE name = 'main'`); err != nil {
		t.Fatalf("corrupt deployment_count: %v", err)
	}

	_, err := q.DeploymentOverview(ctx)
	if !errors.Is(err, ErrDeploymentCacheIntegrity) {
		t.Fatalf("DeploymentOverview error = %v, want ErrDeploymentCacheIntegrity", err)
	}
}
