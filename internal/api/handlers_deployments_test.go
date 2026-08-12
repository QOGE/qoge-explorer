package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestDeploymentsEndpoint_Uninitialized is spec item 20: an uninitialized
// deployment cache is a valid HTTP 200 response with initialized=false and
// an empty deployments array — never a fake synchronized-and-empty
// response.
func TestDeploymentsEndpoint_Uninitialized(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/api/v1/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		State struct {
			Initialized bool   `json:"initialized"`
			Status      string `json:"status"`
		} `json:"state"`
		Deployments []any `json:"deployments"`
	}
	decodeBody(t, rec, &body)
	if body.State.Initialized {
		t.Fatalf("state.initialized = true, want false")
	}
	if body.State.Status != "uninitialized" {
		t.Fatalf("state.status = %q, want uninitialized", body.State.Status)
	}
	if body.Deployments == nil || len(body.Deployments) != 0 {
		t.Fatalf("deployments = %+v, want empty array (not null)", body.Deployments)
	}
}

// TestDeploymentDetailEndpoint_UninitializedIs503 is spec item 21: an
// uninitialized cache cannot truthfully claim a deployment does not exist.
func TestDeploymentDetailEndpoint_UninitializedIs503(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/api/v1/deployments/p2qpk")
	assertJSONError(t, rec, http.StatusServiceUnavailable, "deployment_cache_uninitialized")
}

// TestDeploymentsEndpoint_ListAndDetail exercises the golden path: list
// shows the cached deployment, detail is reachable by exact name, and the
// raw Core JSON is embedded as a real object (not a quoted string).
func TestDeploymentsEndpoint_ListAndDetail(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	cand := deploymentCandidateOf("p2qpk", "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("api-deps-tip"), cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// List.
	rec := doRequest(t, s, "GET", "/api/v1/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		State struct {
			Initialized     bool  `json:"initialized"`
			Generation      int64 `json:"generation"`
			DeploymentCount int   `json:"deployment_count"`
		} `json:"state"`
		Deployments []struct {
			Name   string          `json:"name"`
			Status string          `json:"status"`
			Raw    json.RawMessage `json:"raw"`
		} `json:"deployments"`
	}
	decodeBody(t, rec, &list)
	if !list.State.Initialized || list.State.Generation != 1 || list.State.DeploymentCount != 1 {
		t.Fatalf("list.State = %+v, want initialized=true generation=1 deployment_count=1", list.State)
	}
	if len(list.Deployments) != 1 || list.Deployments[0].Name != "p2qpk" || list.Deployments[0].Status != "started" {
		t.Fatalf("list deployments = %+v, want exactly p2qpk/started", list.Deployments)
	}
	if len(list.Deployments[0].Raw) == 0 {
		t.Fatalf("list deployments[0].Raw is empty, want the embedded Core JSON object")
	}

	// Detail.
	rec = doRequest(t, s, "GET", "/api/v1/deployments/p2qpk")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Deployment struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			StatusNext string `json:"status_next"`
			Bit        *int   `json:"bit"`
			Statistics *struct {
				Threshold *int64 `json:"threshold"`
			} `json:"statistics"`
		} `json:"deployment"`
	}
	decodeBody(t, rec, &detail)
	if detail.Deployment.Name != "p2qpk" || detail.Deployment.Status != "started" {
		t.Fatalf("detail = %+v, want name=p2qpk status=started", detail.Deployment)
	}
	if detail.Deployment.Bit == nil || *detail.Deployment.Bit != 21 {
		t.Fatalf("detail.Bit = %v, want 21", detail.Deployment.Bit)
	}
	if detail.Deployment.Statistics == nil || detail.Deployment.Statistics.Threshold == nil || *detail.Deployment.Statistics.Threshold != 1815 {
		t.Fatalf("detail.Statistics = %+v, want threshold=1815", detail.Deployment.Statistics)
	}

	// Unknown name in an initialized snapshot: 404 with the specific code.
	rec = doRequest(t, s, "GET", "/api/v1/deployments/taproot")
	assertJSONError(t, rec, http.StatusNotFound, "deployment_not_found_in_snapshot")
}

// TestDeploymentsEndpoint_ActiveFixture is spec item 46: an ACTIVE
// deployment with bit/statistics/signalling all legitimately absent must
// render correctly through the API — proving the real mainnet P2QPK
// transition to ACTIVE requires no code change.
func TestDeploymentsEndpoint_ActiveFixture(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	cand := deploymentCandidateOf("p2qpk", "active", 104_032, p2qpkActiveFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("api-active-tip"), cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/deployments/p2qpk")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Deployment struct {
			Active           bool      `json:"active"`
			Status           string    `json:"status"`
			Bit              *int      `json:"bit"`
			Statistics       *struct{} `json:"statistics"`
			Signalling       *string   `json:"signalling"`
			ActivationHeight *int64    `json:"activation_height"`
		} `json:"deployment"`
	}
	decodeBody(t, rec, &detail)
	if !detail.Deployment.Active || detail.Deployment.Status != "active" {
		t.Fatalf("detail = %+v, want active=true status=active", detail.Deployment)
	}
	if detail.Deployment.Bit != nil || detail.Deployment.Statistics != nil || detail.Deployment.Signalling != nil {
		t.Fatalf("detail = %+v, want bit/statistics/signalling all absent once ACTIVE", detail.Deployment)
	}
	if detail.Deployment.ActivationHeight == nil || *detail.Deployment.ActivationHeight != 104_032 {
		t.Fatalf("detail.ActivationHeight = %v, want 104032", detail.Deployment.ActivationHeight)
	}
}

// TestDeploymentsEndpoint_Stale mirrors TestMempoolEndpoint_Stale: an
// initialized-but-stale snapshot must still return the cached data, with
// stale=true and both anchor/confirmed-tip visible.
func TestDeploymentsEndpoint_Stale(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(99, fakeHash("deps-stale-anchor"))); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/deployments")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		State struct {
			Initialized            bool   `json:"initialized"`
			Stale                  bool   `json:"stale"`
			Status                 string `json:"status"`
			CoreTipHeight          *int64 `json:"core_tip_height"`
			ConfirmedIndexedHeight int64  `json:"confirmed_indexed_height"`
		} `json:"state"`
	}
	decodeBody(t, rec, &body)
	if !body.State.Initialized {
		t.Fatalf("initialized = false, want true")
	}
	if !body.State.Stale || body.State.Status != "stale" {
		t.Fatalf("Stale/Status = %v/%q, want true/stale", body.State.Stale, body.State.Status)
	}
	if body.State.CoreTipHeight == nil || *body.State.CoreTipHeight != 99 {
		t.Fatalf("CoreTipHeight = %v, want 99", body.State.CoreTipHeight)
	}
	if body.State.ConfirmedIndexedHeight != -1 {
		t.Fatalf("ConfirmedIndexedHeight = %d, want -1 (bootstrap)", body.State.ConfirmedIndexedHeight)
	}
}

// TestDeploymentsEndpoint_MethodNotAllowed mirrors
// TestMempoolEndpoint_MethodNotAllowed for the two new deployment routes.
func TestDeploymentsEndpoint_MethodNotAllowed(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/deployments"},
		{"DELETE", "/api/v1/deployments/p2qpk"},
	} {
		rec := doRequest(t, s, tc.method, tc.path)
		assertJSONError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Fatalf("%s %s: missing Allow header", tc.method, tc.path)
		}
	}
}

// TestDeploymentDetailEndpoint_MalformedName confirms an oversized/empty
// name is rejected as a 400, never reaching the query layer.
func TestDeploymentDetailEndpoint_MalformedName(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/api/v1/deployments/"+strings.Repeat("p", 200))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized name status = %d, want 400", rec.Code)
	}
}

// TestDeploymentDetailEndpoint_LockedInActivatingBoundary is spec item 8:
// the LOCKED_IN -> ACTIVE transition boundary (current_state LOCKED_IN,
// next_state ACTIVE) must serialize exactly as Core reported it — status
// still "locked_in", status_next "active", active true, and
// activation_height present — never deriving "active" from "status" in
// the API layer.
func TestDeploymentDetailEndpoint_LockedInActivatingBoundary(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	cand := deploymentCandidateOf("p2qpk", "locked_in", 102_016, p2qpkLockedInActivatingFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(104_032, fakeHash("api-lockedin-activating-tip"), cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/deployments/p2qpk")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Deployment struct {
			Status           string `json:"status"`
			StatusNext       string `json:"status_next"`
			Active           bool   `json:"active"`
			Bit              *int   `json:"bit"`
			ActivationHeight *int64 `json:"activation_height"`
			Statistics       *struct {
				Threshold *int64 `json:"threshold"`
				Possible  *bool  `json:"possible"`
			} `json:"statistics"`
			Signalling *string `json:"signalling"`
		} `json:"deployment"`
	}
	decodeBody(t, rec, &detail)
	d := detail.Deployment
	if d.Status != "locked_in" {
		t.Fatalf("status = %q, want locked_in", d.Status)
	}
	if d.StatusNext != "active" {
		t.Fatalf("status_next = %q, want active", d.StatusNext)
	}
	if !d.Active {
		t.Fatalf("active = false, want true")
	}
	if d.ActivationHeight == nil || *d.ActivationHeight != 104_032 {
		t.Fatalf("activation_height = %v, want 104032", d.ActivationHeight)
	}
	if d.Bit == nil {
		t.Fatalf("bit = nil, want present (current_state is still LOCKED_IN)")
	}
	if d.Statistics == nil {
		t.Fatalf("statistics = nil, want present (current_state is still LOCKED_IN)")
	}
	if d.Statistics.Threshold != nil || d.Statistics.Possible != nil {
		t.Fatalf("statistics = %+v, want threshold/possible nil (LOCKED_IN semantics)", d.Statistics)
	}
	if d.Signalling == nil {
		t.Fatalf("signalling = nil, want present (current_state is still LOCKED_IN)")
	}
}

// TestDeploymentDetailEndpoint_EncodedNameRoundTrip is spec item 6: a
// writer-valid deployment name containing a literal '/' must resolve
// through its percent-encoded path segment to the exact same name. No new
// API shape or query parameter is introduced — this is ordinary path
// -segment encoding around the existing /api/v1/deployments/{name} route.
func TestDeploymentDetailEndpoint_EncodedNameRoundTrip(t *testing.T) {
	ctx, s, _, dstore := newDeploymentTestServer(t)

	name := "slash/name"
	cand := deploymentCandidateOf(name, "started", 100_000, p2qpkStartedFixture())
	if _, err := dstore.ReplaceSnapshot(ctx, deploymentSnapshotCandidate(100, fakeHash("api-encoded-name-tip"), cand)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/deployments/"+url.PathEscape(name))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Deployment struct {
			Name string `json:"name"`
		} `json:"deployment"`
	}
	decodeBody(t, rec, &detail)
	if detail.Deployment.Name != name {
		t.Fatalf("detail.Deployment.Name = %q, want %q", detail.Deployment.Name, name)
	}
}
