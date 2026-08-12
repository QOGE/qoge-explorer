package query

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSnapshotConsistency_DeploymentOverview_ConcurrentReplacement is spec
// item 39: an in-flight DeploymentOverview's snapshot is fixed on
// generation 1 (p2qpk STARTED) before a REAL, concurrent
// deployments.Store.ReplaceSnapshot commits generation 2 (p2qpk
// LOCKED_IN). The in-flight read must observe a COHERENT generation 1
// (state generation=1, p2qpk started) — never generation 1 paired with
// locked_in, or generation 2 paired with started. A fresh read afterward
// must observe a coherent generation 2.
func TestSnapshotConsistency_DeploymentOverview_ConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt1 := time.Now().UTC().Truncate(time.Microsecond)
	candA := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("depsnapov-tip-1"), observedAt1, candA)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen1): %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		overview DeploymentOverview
		err      error
	}
	done := make(chan result, 1)
	go func() {
		o, err := q.DeploymentOverview(ctx)
		done <- result{o, err}
	}()

	ss.waitForSnapshot(t)

	observedAt2 := observedAt1.Add(time.Minute)
	candB := deploymentCandidateOf("p2qpk", "locked_in", 102_016, p2qpkLockedInFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(200, fakeHash("depsnapov-tip-2"), observedAt2, candB)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen2): %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight DeploymentOverview: %v", r.err)
	}
	if r.overview.State.Generation != 1 {
		t.Fatalf("in-flight state generation = %d, want 1 (must reflect the snapshot fixed BEFORE the concurrent replacement)", r.overview.State.Generation)
	}
	if len(r.overview.Deployments) != 1 || r.overview.Deployments[0].Status != "started" {
		t.Fatalf("in-flight deployments = %+v, want exactly p2qpk started", r.overview.Deployments)
	}

	fresh, err := q.DeploymentOverview(ctx)
	if err != nil {
		t.Fatalf("fresh DeploymentOverview: %v", err)
	}
	if fresh.State.Generation != 2 {
		t.Fatalf("fresh state generation = %d, want 2", fresh.State.Generation)
	}
	if len(fresh.Deployments) != 1 || fresh.Deployments[0].Status != "locked_in" {
		t.Fatalf("fresh deployments = %+v, want exactly p2qpk locked_in", fresh.Deployments)
	}
}

// TestSnapshotConsistency_DeploymentDetail_ConcurrentReplacement is spec
// item 40: p2qpk exists as STARTED in generation 1; while an in-flight
// DeploymentByName("p2qpk") is assembling its response, a REAL concurrent
// Store.ReplaceSnapshot replaces the entire snapshot with generation 2
// where p2qpk is LOCKED_IN. Because the in-flight read's snapshot was
// fixed BEFORE that replacement committed, PostgreSQL's REPEATABLE READ
// isolation guarantees the remaining statement in that same read-only
// transaction still sees the complete pre-replacement row — the in-flight
// call must return the COMPLETE generation-1 STARTED deployment (never a
// not-found, and never any field mixed in from generation 2). A fresh call
// afterward must see generation 2 LOCKED_IN.
func TestSnapshotConsistency_DeploymentDetail_ConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt1 := time.Now().UTC().Truncate(time.Microsecond)
	candA := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("depsnapdetail-tip-1"), observedAt1, candA)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen1): %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		detail DeploymentDetail
		err    error
	}
	done := make(chan result, 1)
	go func() {
		d, err := q.DeploymentByName(ctx, "p2qpk")
		done <- result{d, err}
	}()

	ss.waitForSnapshot(t)

	observedAt2 := observedAt1.Add(time.Minute)
	candB := deploymentCandidateOf("p2qpk", "locked_in", 102_016, p2qpkLockedInFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(200, fakeHash("depsnapdetail-tip-2"), observedAt2, candB)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen2): %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight DeploymentByName(p2qpk): %v, want the complete generation-1 deployment (never not-found — REPEATABLE READ must still see the pre-replacement row)", r.err)
	}
	if r.detail.State.Generation != 1 {
		t.Fatalf("in-flight detail State.Generation = %d, want 1 (must not observe the concurrent generation-2 replacement)", r.detail.State.Generation)
	}
	if r.detail.Deployment.Status != "started" {
		t.Fatalf("in-flight detail status = %q, want \"started\"", r.detail.Deployment.Status)
	}
	if r.detail.Deployment.Statistics == nil || r.detail.Deployment.Statistics.Threshold == nil {
		t.Fatalf("in-flight detail statistics = %+v, want complete generation-1 STARTED statistics (with threshold), never mixed with generation-2 fields", r.detail.Deployment.Statistics)
	}

	fresh, err := q.DeploymentByName(ctx, "p2qpk")
	if err != nil {
		t.Fatalf("fresh DeploymentByName(p2qpk): %v", err)
	}
	if fresh.State.Generation != 2 {
		t.Fatalf("fresh detail State.Generation = %d, want 2", fresh.State.Generation)
	}
	if fresh.Deployment.Status != "locked_in" {
		t.Fatalf("fresh detail status = %q, want \"locked_in\"", fresh.Deployment.Status)
	}
}

// TestSnapshotConsistency_DeploymentByName_SurvivesFullReplacementAway
// proves the in-flight snapshot semantics hold even when the looked-up
// name is entirely REMOVED by the concurrent replacement (not just
// changed in status) — the in-flight read must still return the complete
// generation-1 body, and only a fresh call afterward sees it gone.
func TestSnapshotConsistency_DeploymentByName_SurvivesFullReplacementAway(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	dstore := newTestDeploymentsStore(pool)

	observedAt1 := time.Now().UTC().Truncate(time.Microsecond)
	candA := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(100, fakeHash("depsnapgone-tip-1"), observedAt1, candA)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen1): %v", err)
	}

	ss := newSnapshotSync(t)

	type result struct {
		detail DeploymentDetail
		err    error
	}
	done := make(chan result, 1)
	go func() {
		d, err := q.DeploymentByName(ctx, "p2qpk")
		done <- result{d, err}
	}()

	ss.waitForSnapshot(t)

	observedAt2 := observedAt1.Add(time.Minute)
	candOther := deploymentCandidateOf("taproot", "started", 50_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshot(200, fakeHash("depsnapgone-tip-2"), observedAt2, candOther)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen2, p2qpk removed): %v", err)
	}

	ss.release()
	r := <-done
	ss.disable()
	if r.err != nil {
		t.Fatalf("in-flight DeploymentByName(p2qpk) = %v, want the complete generation-1 body (removal happened AFTER the snapshot was fixed)", r.err)
	}
	if r.detail.Deployment.Name != "p2qpk" {
		t.Fatalf("in-flight detail name = %q, want p2qpk", r.detail.Deployment.Name)
	}

	_, err := q.DeploymentByName(ctx, "p2qpk")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("fresh DeploymentByName(p2qpk) error = %v, want ErrNotFound (p2qpk was replaced away)", err)
	}
}
