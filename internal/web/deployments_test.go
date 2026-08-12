package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"
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
	bodyContains(t, rec, "Core did not report signalling statistics for this deployment state.")
	bodyContains(t, rec, "Activation Height")
}

// TestDeploymentDetailPage_AllStatusesRenderSafely is spec items 44-47/13:
// DEFINED/STARTED/LOCKED_IN/ACTIVE/FAILED must all render without error —
// no phase logic may assume P2QPK is currently STARTED — and each status's
// signalling-statistics presentation must match what QOGE Core stable's
// getdeploymentinfo actually reports for that state (never inventing an
// "unknown" reading for a field Core simply did not include).
func TestDeploymentDetailPage_AllStatusesRenderSafely(t *testing.T) {
	fixtures := map[string]struct {
		status string
		since  int64
		raw    []byte
	}{
		"defined":                {"defined", 0, p2qpkDefinedFixture()},
		"started":                {"started", 100_000, p2qpkStartedFixture()},
		"started_possible_false": {"started", 100_000, p2qpkStartedPossibleFalseFixture()},
		"locked_in":              {"locked_in", 102_016, p2qpkLockedInFixture()},
		"active":                 {"active", 104_032, p2qpkActiveFixture()},
		"failed":                 {"failed", 102_016, p2qpkFailedFixture()},
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
			body := rec.Body.String()

			switch name {
			case "started":
				// STARTED with possible=true: statistics, threshold, and
				// an affirmative Possible row must all be visible.
				if !strings.Contains(body, "Signalling Statistics") {
					t.Fatalf("started: missing Signalling Statistics section")
				}
				if !strings.Contains(body, "Threshold") {
					t.Fatalf("started: missing Threshold row")
				}
				if !strings.Contains(body, "<th>Possible</th>") {
					t.Fatalf("started: missing Possible row")
				}
			case "started_possible_false":
				// possible=false must render "no", not be omitted the way
				// a nil Possible is for LOCKED_IN.
				if !strings.Contains(body, "<th>Possible</th><td>no</td>") {
					t.Fatalf("started_possible_false: want an explicit 'no' Possible row, got body=%s", body)
				}
			case "locked_in":
				// LOCKED_IN: statistics section exists, but threshold and
				// possible are structurally absent from Core's response for
				// this state — never presented as "unknown".
				if !strings.Contains(body, "Signalling Statistics") {
					t.Fatalf("locked_in: missing Signalling Statistics section")
				}
				if strings.Contains(body, "<th>Threshold</th>") {
					t.Fatalf("locked_in: Threshold row must be absent, got body=%s", body)
				}
				if strings.Contains(body, "<th>Possible</th>") {
					t.Fatalf("locked_in: Possible row must be absent, got body=%s", body)
				}
				if strings.Contains(body, "unknown") {
					t.Fatalf("locked_in: must never render 'unknown' for an omitted BIP9 field, got body=%s", body)
				}
			case "defined", "active":
				if !strings.Contains(body, "Core did not report signalling statistics for this deployment state.") {
					t.Fatalf("%s: missing neutral no-statistics wording, got body=%s", name, body)
				}
				if strings.Contains(body, "Signalling Statistics") {
					t.Fatalf("%s: unexpected Signalling Statistics section", name)
				}
			}
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

	// HTML safety alone is not enough — the generated link must also be
	// navigable back to the exact original name (spec item 5). The evil
	// name contains a literal '/', so a naive href would 404 or route
	// somewhere else entirely.
	wantPath := deploymentPath(evilName)
	rec = doRequest(t, s, "GET", wantPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail GET %s status = %d, want 200, body=%s", wantPath, rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "&lt;script&gt;")
	bodyNotContains(t, rec, "<script>alert(1)</script>")
}

// TestDeploymentsPage_URLRoundTrip is spec item 4: writer-valid deployment
// names containing URL/path-significant characters (/, ?, #, %) or literal
// dot-segments (., ..) must round-trip through the generated deployment
// link to the exact same name — never landing on a different route, being
// collapsed by net/http.ServeMux's canonical-path cleaning, or 404ing.
// Each case publishes the name through the REAL Phase 2G.1 Store, confirms
// the list page's generated href is present (HTML-attribute-escaped), then
// requests that exact URL and confirms the detail page shows the exact
// original name — proving writer name -> PostgreSQL -> query -> generated
// URL -> ServeMux PathValue -> exact DB lookup round-trips.
func TestDeploymentsPage_URLRoundTrip(t *testing.T) {
	names := []string{
		"ordinary-name",
		"slash/name",
		"question?name",
		"fragment#name",
		"percent%name",
		".",
		"..",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			ctx, s, _, dstore := newDeploymentTestServer(t)
			cand := deploymentCandidateOf(name, "started", 100_000, p2qpkStartedFixture())
			if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("web-roundtrip-tip"), cand)); err != nil {
				t.Fatalf("ReplaceSnapshot: %v", err)
			}

			wantPath := deploymentPath(name)

			rec := doRequest(t, s, "GET", "/deployments")
			if rec.Code != http.StatusOK {
				t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
			}
			bodyContains(t, rec, template.HTMLEscapeString(wantPath))

			rec = doRequest(t, s, "GET", wantPath)
			if rec.Code != http.StatusOK {
				t.Fatalf("detail GET %s status = %d, want 200, body=%s", wantPath, rec.Code, rec.Body.String())
			}
			bodyContains(t, rec, template.HTMLEscapeString(name))
		})
	}
}

// TestDeploymentsPage_SameHeightReorgPresentation is spec item 8: when the
// deployment snapshot's anchor and the confirmed indexed tip sit at the
// exact same height but disagree on hash, both hashes and both heights
// must be visible together so a human can diagnose the mismatch — and the
// wording must stay neutral (no "ahead"/"newer"/"advanced past": a
// same-height reorg implies no direction).
func TestDeploymentsPage_SameHeightReorgPresentation(t *testing.T) {
	ctx := context.Background()
	s, st, pool := newTestServerWithPool(t)
	dstore := newTestDeploymentsStore(pool)

	g := block("sh-reorg-genesis", 0, "", coinbaseTx("sh-reorg-genesis", 100_00000000, "qSHReorgGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	confirmedTip := block("sh-reorg-confirmed", 1, g.Hash, coinbaseTx("sh-reorg-confirmed", 40_00000000, "qSHReorgConfirmed"))
	if err := st.ApplyBlock(ctx, confirmedTip); err != nil {
		t.Fatalf("apply confirmed tip: %v", err)
	}

	anchorHash := fakeHash("sh-reorg-anchor-other")
	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(confirmedTip.Height, anchorHash, cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	for _, path := range []string{"/deployments", "/deployments/p2qpk"} {
		rec := doRequest(t, s, "GET", path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body=%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()

		if !strings.Contains(strings.ToLower(body), "stale") {
			t.Fatalf("%s: body does not mention stale status, body=%s", path, body)
		}
		if strings.Contains(body, "ahead") || strings.Contains(body, "newer") || strings.Contains(body, "advanced past") {
			t.Fatalf("%s body contains directional wording, want neutral only: %s", path, body)
		}

		bodyContains(t, rec, anchorHash)
		bodyContains(t, rec, confirmedTip.Hash)

		heightMarker := fmt.Sprintf(">%d<", confirmedTip.Height)
		if strings.Count(body, heightMarker) < 2 {
			t.Fatalf("%s: want height %d to appear at least twice (anchor + confirmed), body=%s", path, confirmedTip.Height, body)
		}
	}

	// The list page uses the neutral "no longer matches" wording; the
	// detail page uses the neutral "cached...stale" qualification.
	rec := doRequest(t, s, "GET", "/deployments")
	bodyContains(t, rec, "no longer matches")
	rec = doRequest(t, s, "GET", "/deployments/p2qpk")
	bodyContains(t, rec, "cached Core deployment observation")
}
