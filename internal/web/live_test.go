package web

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

// A: the shared layout loads /static/live.js on every page, deferred, with
// no inline script.
func TestLive_LayoutLoadsScript(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	bodyContains(t, rec, `<script src="/static/live.js" defer></script>`)
}

// B/C: the shared live-status banner is present on every page, hidden by
// default, and uses the accessible role/aria-live contract — never stealing
// focus, never requiring JS to exist at all (it's just inert markup until
// live.js unhides it).
func TestLive_BannerHiddenAndAccessible(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="live-chain-banner"`) {
		t.Fatalf("live-chain-banner not present:\n%s", body)
	}
	if !strings.Contains(body, `role="status"`) {
		t.Fatalf("banner missing role=\"status\":\n%s", body)
	}
	if !strings.Contains(body, `aria-live="polite"`) {
		t.Fatalf("banner missing aria-live=\"polite\":\n%s", body)
	}
	// The banner element itself must carry the "hidden" boolean attribute
	// so it renders inert without any JS running at all.
	bannerIdx := strings.Index(body, `id="live-chain-banner"`)
	tagStart := strings.LastIndex(body[:bannerIdx], "<div")
	tagEnd := strings.Index(body[bannerIdx:], ">") + bannerIdx
	if tagStart == -1 || tagEnd == -1 || !strings.Contains(body[tagStart:tagEnd], "hidden") {
		t.Fatalf("live-chain-banner div is not marked hidden:\n%s", body[tagStart:tagEnd+1])
	}
}

// D: the home page exposes its server-rendered baseline tip via harmless
// data attributes (never inside executable JavaScript) so live.js's first
// poll can detect a block indexed after the HTML snapshot was taken.
func TestLive_HomeExposesBaselineTip(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	// Before any block is indexed: height still present (-1), no hash.
	rec := doRequest(t, s, "GET", "/")
	bodyContains(t, rec, `data-live-refresh="home"`)
	bodyContains(t, rec, `data-indexed-height="-1"`)
	bodyNotContains(t, rec, "data-indexed-hash=")

	g := block("live-home-g", 0, "", coinbaseTx("live-home-g", 100_00000000, "qLiveHomeG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	rec = doRequest(t, s, "GET", "/")
	bodyContains(t, rec, `data-indexed-height="0"`)
	bodyContains(t, rec, `data-indexed-hash="`+g.Hash+`"`)
}

// E: canonical block rows on the blocks page expose their own
// height/hash via harmless data attributes, and only the FIRST page (no
// "before" cursor) is marked live-refresh eligible — a historical page
// must never auto-reload just because the global tip advances.
func TestLive_BlockRowsExposeDataAndFirstPageOnly(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("live-blk-g", 0, "", coinbaseTx("live-blk-g", 100_00000000, "qLiveBlkG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("live-blk-1", 1, g.Hash, coinbaseTx("live-blk-1", 10_00000000, "qLiveBlk1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	rec := doRequest(t, s, "GET", "/blocks")
	bodyContains(t, rec, `data-live-refresh="blocks"`)
	bodyContains(t, rec, `data-block-height="1" data-block-hash="`+b1.Hash+`"`)
	bodyContains(t, rec, `data-block-height="0" data-block-hash="`+g.Hash+`"`)

	rec = doRequest(t, s, "GET", "/blocks?before=1")
	bodyNotContains(t, rec, `data-live-refresh="blocks"`)
	// Row-level data attributes may still be present (harmless everywhere),
	// but the live-refresh eligibility wrapper must be absent.
	bodyContains(t, rec, `data-block-height="0" data-block-hash="`+g.Hash+`"`)
}

// F/G: the embedded live.js script is served with a 200 and a
// JavaScript-compatible Content-Type, with no working-directory dependency
// (embedded via go:embed, same as app.css).
func TestLive_ScriptServedCorrectly(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/static/live.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q, want a javascript MIME type", ct)
	}
	bodyContains(t, rec, "/api/v1/status")
}

// H: the CSP explicitly permits self-hosted script (script-src 'self'),
// with no unsafe-inline/unsafe-eval anywhere.
func TestLive_CSPPermitsSelfScript(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/")
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP = %q, want script-src 'self'", csp)
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP = %q must never contain unsafe-inline/unsafe-eval", csp)
	}
}

// I: no external script or CDN reference exists anywhere in a rendered
// page — the only <script>/<link> sources are same-origin /static/ paths.
func TestLive_NoExternalScriptOrCDN(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)
	g := block("live-ext-g", 0, "", coinbaseTx("live-ext-g", 100_00000000, "qLiveExtG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	for _, path := range []string{"/", "/blocks", "/block/0"} {
		rec := doRequest(t, s, "GET", path)
		body := rec.Body.String()
		if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
			t.Fatalf("%s: unexpected external URL reference in rendered HTML:\n%s", path, body)
		}
		if strings.Contains(body, "cdn.") {
			t.Fatalf("%s: unexpected CDN reference in rendered HTML:\n%s", path, body)
		}
	}
}

// J: existing pages still render fully-formed, meaningful content directly
// in the server-rendered HTML — none of it depends on live.js executing.
func TestLive_PagesRemainJSIndependent(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)
	g := block("live-noscript-g", 0, "", coinbaseTx("live-noscript-g", 100_00000000, "qLiveNoScriptG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("live-noscript-1", 1, g.Hash, coinbaseTx("live-noscript-1", 50_00000000, "qLiveNoScript1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	rec := doRequest(t, s, "GET", "/")
	bodyContains(t, rec, "Indexed height <strong>1</strong>")
	bodyContains(t, rec, b1.Hash)

	rec = doRequest(t, s, "GET", "/block/0")
	bodyContains(t, rec, g.Hash)

	rec = doRequest(t, s, "GET", "/address/qLiveNoScript1")
	bodyContains(t, rec, "50.00000000 QOGE")
}

// live.js's static architectural contract, pinned without a JS runtime:
// enforces the required substrings (status endpoint, no-store caching,
// the two field names it's allowed to read) and forbids anything that
// would turn it into mempool/WebSocket/SSE machinery, an eval/innerHTML
// injection vector, or an external script dependency.
func TestLive_ScriptContract(t *testing.T) {
	src, err := os.ReadFile("static/live.js")
	if err != nil {
		t.Fatalf("read static/live.js: %v", err)
	}
	body := string(src)

	for _, want := range []string{
		"/api/v1/status",
		"no-store",
		"indexed_height",
		"indexed_block_hash",
		// The notify-only-page baseline-initialization guard (see the file's
		// "Two baseline modes" comment): without it, a detail/historical
		// page's first real status response would show a false "chain
		// changed" banner immediately, since there is no rendered baseline
		// to compare against. This pins that the guard exists at all,
		// without needing a JS runtime to execute the state machine.
		"hasStatusBaseline",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("live.js missing required substring %q", want)
		}
	}

	for _, forbidden := range []string{
		"WebSocket",
		"EventSource",
		"/mempool",
		"getrawmempool",
		"innerHTML",
		"eval(",
		"new Function",
		"external http://",
		"external https://",
		"http://",
		"https://",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("live.js must not contain %q", forbidden)
		}
	}
}
