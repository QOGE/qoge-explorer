package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestRichListPage_Uninitialized is spec item 18: an uninitialized
// explorer renders a clear "not indexed yet" message and the disclaimer,
// never a fake empty ranking framed as "zero wealth".
func TestRichListPage_Uninitialized(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doRequest(t, s, "GET", "/richlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	bodyContains(t, rec, "No block has been indexed yet")
	bodyContains(t, rec, "This list ranks addresses, not people, wallets, or entities.")
}

// TestRichListPage_GoldenPath applies two blocks and confirms the page
// renders rank, address, and QOGE/satoshi balances in order.
func TestRichListPage_GoldenPath(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("web-richlist-g", 0, "", coinbaseTx("web-richlist-g", 100_00000000, "qWebRichlistG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("web-richlist-h1", 1, g.Hash, coinbaseTx("web-richlist-h1", 30_00000000, "qWebRichlistA"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}
	h2 := block("web-richlist-h2", 2, h1.Hash, coinbaseTx("web-richlist-h2", 60_00000000, "qWebRichlistB"))
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply h2: %v", err)
	}

	rec := doRequest(t, s, "GET", "/richlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "Rich List")
	bodyContains(t, rec, "qWebRichlistA")
	bodyContains(t, rec, "qWebRichlistB")
	bodyContains(t, rec, "30.00000000")
	bodyContains(t, rec, "60.00000000")
	bodyContains(t, rec, `href="/address/qWebRichlistA"`)
	bodyContains(t, rec, `href="/address/qWebRichlistB"`)
	bodyNotContains(t, rec, "No block has been indexed yet")

	// B (60) must be ranked ahead of A (30) — check ordering by byte
	// offset within the rendered body.
	body := rec.Body.String()
	if strings.Index(body, "qWebRichlistB") > strings.Index(body, "qWebRichlistA") {
		t.Fatalf("expected qWebRichlistB (rank 1) to appear before qWebRichlistA (rank 2) in the rendered page")
	}
}

// TestRichListPage_Disclaimer is spec item 34: the page must visibly
// explain address-vs-owner, bare-multisig, and no-address-UTXO semantics
// in non-alarming technical language.
func TestRichListPage_Disclaimer(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doRequest(t, s, "GET", "/richlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "This list ranks addresses, not people, wallets, or entities.")
	bodyContains(t, rec, "bare-multisig")
	bodyContains(t, rec, "no single attributed destination address")
}

// TestRichListPage_MethodNotAllowed mirrors TestSupplyPage_MethodNotAllowed
// for the new /richlist route.
func TestRichListPage_MethodNotAllowed(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doRequest(t, s, "POST", "/richlist")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

// TestRichListPage_Navigation confirms the shared header links to
// /richlist (spec item 35).
func TestRichListPage_Navigation(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doRequest(t, s, "GET", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, `href="/richlist"`)
}

// TestRichListPage_SecurityHeaders confirms the baseline security headers
// ServeHTTP sets are present, mirroring other pages' coverage.
func TestRichListPage_SecurityHeaders(t *testing.T) {
	s, _ := newTestServer(t)

	rec := doRequest(t, s, "GET", "/richlist")
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing X-Content-Type-Options header")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("missing Content-Security-Policy header")
	}
}
