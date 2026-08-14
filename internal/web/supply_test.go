package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestSupplyPage_Uninitialized is spec item 41/15: an uninitialized
// explorer renders a clear "not indexed yet" message, never a fake supply
// of zero.
func TestSupplyPage_Uninitialized(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doRequest(t, s, "GET", "/supply")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	bodyContains(t, rec, "No block has been indexed yet")
	bodyContains(t, rec, "circulating supply")
}

// TestSupplyPage_GoldenPath applies one genesis block and confirms the page
// renders its title and monetary facts.
func TestSupplyPage_GoldenPath(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("web-supply-g", 0, "", coinbaseTx("web-supply-g", 100_00000000, "qWebSupplyG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/supply")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "Supply")
	bodyContains(t, rec, "100.00000000")
	bodyNotContains(t, rec, "No block has been indexed yet")
}

// TestSupplyPage_AccountingIncomplete is spec item 19: incomplete canonical
// accounting renders an explanatory HTTP 503 mentioning the operator
// remediation, never a partial page.
func TestSupplyPage_AccountingIncomplete(t *testing.T) {
	ctx := context.Background()
	s, st, pool := newTestServerWithPool(t)

	g := block("web-supply-incomplete-g", 0, "", coinbaseTx("web-supply-incomplete-g", 100_00000000, "qWebSupplyIncG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM block_accounting WHERE block_hash = $1", g.Hash); err != nil {
		t.Fatalf("delete block_accounting: %v", err)
	}

	rec := doRequest(t, s, "GET", "/supply")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "backfill-accounting")
}

// TestSupplyPage_MethodNotAllowed mirrors
// TestDeploymentsPage_MethodNotAllowed for the new /supply route.
func TestSupplyPage_MethodNotAllowed(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doRequest(t, s, "POST", "/supply")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

// TestSupplyPage_Navigation confirms every page's shared header links to
// /supply (spec item 35).
func TestSupplyPage_Navigation(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doRequest(t, s, "GET", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, `href="/supply"`)
}
