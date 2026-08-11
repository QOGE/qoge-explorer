package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doRequest(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func bodyContains(t *testing.T, rec *httptest.ResponseRecorder, substr string) {
	t.Helper()
	if !strings.Contains(rec.Body.String(), substr) {
		t.Fatalf("response body does not contain %q\nbody=%s", substr, rec.Body.String())
	}
}

func bodyNotContains(t *testing.T, rec *httptest.ResponseRecorder, substr string) {
	t.Helper()
	if strings.Contains(rec.Body.String(), substr) {
		t.Fatalf("response body unexpectedly contains %q\nbody=%s", substr, rec.Body.String())
	}
}

// A/B/C: homepage 200, shows the indexed tip, and only ever lists
// canonical blocks (RecentBlocks itself is canonical-only — see
// query.Store.RecentBlocks).
func TestHome(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	rec := doRequest(t, s, "GET", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "No blocks indexed yet.")

	g := block("home-g", 0, "", coinbaseTx("home-g", 100_00000000, "qHomeG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("home-1", 1, g.Hash, coinbaseTx("home-1", 50_00000000, "qHome1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	rec = doRequest(t, s, "GET", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status after indexing = %d", rec.Code)
	}
	bodyContains(t, rec, "Indexed height <strong>1</strong>")
	bodyContains(t, rec, b1.Hash)
	bodyContains(t, rec, "/block/"+b1.Hash)
	bodyContains(t, rec, "/block/"+g.Hash)
}

// D: /blocks pagination uses the existing height cursor, not OFFSET.
func TestBlocksPagination(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("bp-g", 0, "", coinbaseTx("bp-g", 100_00000000, "qBPG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	prev := g
	var heights []string
	for h := int64(1); h <= 5; h++ {
		label := "bp-" + string(rune('a'+h))
		b := block(label, h, prev.Hash, coinbaseTx(label, 10_00000000, "qBPRepeat"))
		if err := st.ApplyBlock(ctx, b); err != nil {
			t.Fatalf("apply block %d: %v", h, err)
		}
		heights = append(heights, b.Hash)
		prev = b
	}

	rec := doRequest(t, s, "GET", "/blocks?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, heights[len(heights)-1]) // height 5
	bodyContains(t, rec, heights[len(heights)-2]) // height 4
	bodyNotContains(t, rec, heights[len(heights)-3])
	// Next-page cursor link preserves the caller's requested limit, not just
	// the cursor — a naive "/blocks?before=4" would silently reset paging
	// forward back to query.DefaultPageSize. html/template escapes the "&"
	// separator in the href attribute, so assert the escaped form.
	bodyContains(t, rec, "/blocks?before=4&amp;limit=2")

	rec2 := doRequest(t, s, "GET", "/blocks?limit=2&before=4")
	if rec2.Code != http.StatusOK {
		t.Fatalf("page 2 status = %d", rec2.Code)
	}
	bodyContains(t, rec2, heights[len(heights)-3]) // height 3
	bodyContains(t, rec2, heights[len(heights)-4]) // height 2
	bodyNotContains(t, rec2, heights[len(heights)-1])
}

// E/F: canonical block lookup by height and by hash.
func TestBlockDetail_Canonical(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("bd-g", 0, "", coinbaseTx("bd-g", 100_00000000, "qBDG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("bd-1", 1, g.Hash, coinbaseTx("bd-1", 50_00000000, "qBD1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	rec := doRequest(t, s, "GET", "/block/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("by height status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, b1.Hash)
	bodyContains(t, rec, "Canonical")
	bodyNotContains(t, rec, "Orphaned block")

	rec = doRequest(t, s, "GET", "/block/"+b1.Hash)
	if rec.Code != http.StatusOK {
		t.Fatalf("by hash status = %d", rec.Code)
	}
	bodyContains(t, rec, b1.Hash)
	bodyContains(t, rec, "Canonical")
}

// G/H: after a reorg, the orphaned block is still reachable BY HASH (and
// visibly marked orphaned), but is never exposed as canonical at its old
// height — that height now serves the new canonical block instead (see
// query.Store.BlockByHeight's doc comment: there is no orphan-by-height
// lookup at all).
func TestBlockDetail_OrphanByHash_NotByHeight(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("bo-g", 0, "", coinbaseTx("bo-g", 100_00000000, "qBOG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("bo-A1", 1, g.Hash, coinbaseTx("bo-A1", 30_00000000, "qBOA1"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}
	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	b1 := block("bo-B1", 1, g.Hash, coinbaseTx("bo-B1", 30_00000000, "qBOB1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	// H: height 1 now serves B1 (canonical), never A1.
	rec := doRequest(t, s, "GET", "/block/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("by height status = %d", rec.Code)
	}
	bodyContains(t, rec, b1.Hash)
	bodyNotContains(t, rec, a1.Hash)

	// G: A1 is still reachable by hash, clearly marked orphaned.
	rec = doRequest(t, s, "GET", "/block/"+a1.Hash)
	if rec.Code != http.StatusOK {
		t.Fatalf("orphan by hash status = %d", rec.Code)
	}
	bodyContains(t, rec, "Orphaned block")
	bodyContains(t, rec, a1.Hash)
}

// AB: malformed identifiers render an HTML 400.
func TestMalformedIdentifier_HTML400(t *testing.T) {
	s, _ := newTestServer(t)

	for _, path := range []string{
		"/block/not-a-valid-id",
		"/tx/short",
		"/address/" + strings.Repeat("q", 200),
	} {
		rec := doRequest(t, s, "GET", path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body=%s)", path, rec.Code, rec.Body.String())
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s: Content-Type = %q, want text/html", path, ct)
		}
		bodyContains(t, rec, "Bad Request")
	}
}

// AC: unknown objects and unknown paths render an HTML 404.
func TestUnknownObject_HTML404(t *testing.T) {
	s, _ := newTestServer(t)

	for _, path := range []string{
		"/block/999999",
		"/tx/" + strings.Repeat("a", 64),
		"/no-such-page",
	} {
		rec := doRequest(t, s, "GET", path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (body=%s)", path, rec.Code, rec.Body.String())
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s: Content-Type = %q, want text/html", path, ct)
		}
		bodyContains(t, rec, "Not Found")
	}
}

// Wrong method on a known route also renders an HTML error (405), never
// JSON — routing precedence mirrors internal/api's reviewed pattern.
func TestMethodNotAllowed_HTML405(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "POST", "/blocks")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want GET", allow)
	}

	// An unknown route must remain a plain 404, never a fake 405 with an
	// Allow header for a route that was never registered at all.
	rec = doRequest(t, s, "GET", "/no-such-page")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "" {
		t.Fatalf("unknown route Allow = %q, want empty", allow)
	}
}

// Security headers are present on every HTML response.
func TestSecurityHeaders(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/")
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", rec.Header().Get("X-Content-Type-Options"))
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", rec.Header().Get("Referrer-Policy"))
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("Content-Security-Policy is empty")
	}
}

// Embedded static assets are served without any working-directory
// dependency.
func TestStaticAssets(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/static/app.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	bodyContains(t, rec, "QOGE Explorer")
}

// Every embedded page template parses successfully — a build-time
// invariant, checked directly rather than only incidentally via other
// tests exercising each page.
func TestTemplatesParse(t *testing.T) {
	p, err := loadTemplates()
	if err != nil {
		t.Fatalf("loadTemplates: %v", err)
	}
	for name := range pageTemplateFiles {
		if _, ok := p[name]; !ok {
			t.Fatalf("page %q not loaded", name)
		}
	}
}
