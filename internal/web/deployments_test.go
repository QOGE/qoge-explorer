package web

import (
	"net/http"
	"testing"
)

// TestDeploymentsPage_Uninitialized is spec item 32: an uninitialized
// deployment cache must render an explicit "not synchronized yet" message
// — never "0 deployments" as if a successful Core observation had
// returned none.
func TestDeploymentsPage_Uninitialized(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "not synchronized yet")
	bodyNotContains(t, rec, "0 deployments")
}

// TestDeploymentDetailPage_UninitializedIsServiceUnavailable is spec item
// 21/54: an uninitialized cache cannot truthfully claim a deployment does
// not exist, so the detail page renders a service-unavailable state, not a
// 404.
func TestDeploymentDetailPage_UninitializedIsServiceUnavailable(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/deployments/p2qpk")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "not synchronized yet")
}

// TestDeploymentsPage_ListAndDetail exercises the golden path: the list
// page shows the cached deployment and links to its detail page, which
// shows Core's raw fields and the collapsed raw JSON block.
func TestDeploymentsPage_ListAndDetail(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("web-deps-tip"), cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// List page.
	rec := doRequest(t, s, "GET", "/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "/deployments/p2qpk")
	bodyContains(t, rec, "started")
	bodyNotContains(t, rec, "not synchronized yet")

	// Detail page.
	rec = doRequest(t, s, "GET", "/deployments/p2qpk")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "Deployment: p2qpk")
	bodyContains(t, rec, "started")
	bodyContains(t, rec, "Signalling Statistics")
	bodyContains(t, rec, "Raw Core Response")
	// The raw JSON is rendered with ordinary html/template escaping
	// (never template.HTML) — assert the escaped quote form actually
	// present, not a literal quote.
	bodyContains(t, rec, `&#34;status&#34;: &#34;started&#34;`)

	// Unknown name in an initialized snapshot: 404.
	rec = doRequest(t, s, "GET", "/deployments/taproot")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestDeploymentsPage_InitializedEmpty is spec item 43's HTML requirement:
// a legitimately empty synchronized snapshot must say so explicitly,
// distinct from "not synchronized yet".
func TestDeploymentsPage_InitializedEmpty(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("web-deps-empty-tip"))); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyNotContains(t, rec, "not synchronized yet")
	bodyContains(t, rec, "zero BIP9 deployments")
}

// TestDeploymentDetailPage_ActiveFixture is spec item 46: ACTIVE must
// render safely with bit/statistics/signalling all absent, proving no code
// change is needed for the real P2QPK mainnet transition.
func TestDeploymentDetailPage_ActiveFixture(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	cand := deploymentCandidateOf("p2qpk", "active", 104_032, p2qpkActiveFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("web-active-tip"), cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/deployments/p2qpk")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "active")
	bodyContains(t, rec, "No signalling statistics reported")
	bodyContains(t, rec, "Activation Height")
}

// TestDeploymentDetailPage_AllStatusesRenderSafely is spec items 44-47:
// DEFINED/STARTED/LOCKED_IN/ACTIVE/FAILED must all render without error —
// no phase logic may assume P2QPK is currently STARTED.
func TestDeploymentDetailPage_AllStatusesRenderSafely(t *testing.T) {
	fixtures := map[string]struct {
		status string
		since  int64
		raw    []byte
	}{
		"defined":   {"defined", 0, p2qpkDefinedFixture()},
		"started":   {"started", 100_000, p2qpkStartedFixture()},
		"locked_in": {"locked_in", 102_016, p2qpkLockedInFixture()},
		"active":    {"active", 104_032, p2qpkActiveFixture()},
		"failed":    {"failed", 102_016, p2qpkFailedFixture()},
	}

	for name, f := range fixtures {
		t.Run(name, func(t *testing.T) {
			ctx, s, _, dstore := newDeploymentTestServer(t)
			cand := deploymentCandidateOf("p2qpk", f.status, f.since, f.raw)
			if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("web-allstatus-"+name), cand)); err != nil {
				t.Fatalf("ReplaceSnapshot: %v", err)
			}

			rec := doRequest(t, s, "GET", "/deployments/p2qpk")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			bodyContains(t, rec, f.status)
		})
	}
}

// TestDeploymentsPage_MethodNotAllowed mirrors the mempool/blocks pattern:
// a wrong method on a known deployment route renders an HTML 405.
func TestDeploymentsPage_MethodNotAllowed(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)
	rec := doRequest(t, s, "POST", "/deployments")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

// TestDeploymentsPage_Navigation confirms the shared nav link is present.
func TestDeploymentsPage_Navigation(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)
	rec := doRequest(t, s, "GET", "/")
	bodyContains(t, rec, `href="/deployments"`)
}

// TestDeploymentsPage_StaleWording is spec item 33: a stale deployment
// snapshot must use neutral "no longer matches" wording on the list page
// and an explicit cached/stale qualification on the detail page — never
// "currently started"/"currently active" without that qualification.
func TestDeploymentsPage_StaleWording(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(99, fakeHash("web-deps-stale-tip"), cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "no longer matches")

	rec = doRequest(t, s, "GET", "/deployments/p2qpk")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "cached Core deployment observation")
	bodyContains(t, rec, "stale")
}

// TestDeploymentsPage_Escaping mirrors TestMempoolPage_Escaping: a
// deployment name containing HTML-special characters must never render as
// raw, unescaped markup. The evil name is written through the REAL
// deployments.Store write path (candidate name shape allows any non-empty
// string up to the length bound — see internal/deployments/model.go's
// validateDeploymentName), then read back through the real query.Store ->
// handler -> html/template path.
func TestDeploymentsPage_Escaping(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	evilName := `<script>alert(1)</script>&"'`
	cand := deploymentCandidateOf(evilName, "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("web-deps-escaping-tip"), cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyNotContains(t, rec, "<script>alert(1)</script>")
	bodyContains(t, rec, "&lt;script&gt;")
}
