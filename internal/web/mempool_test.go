package web

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/QOGE/qoge-explorer/internal/store"
)

// TestMempoolPage_Uninitialized is spec item 35: an uninitialized mempool
// cache must render an explicit "not synchronized yet" state — never
// "0 transactions" as if a synchronized empty mempool had been observed.
func TestMempoolPage_Uninitialized(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/mempool")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "not synchronized yet")
	bodyNotContains(t, rec, "0 transactions")
}

// TestMempoolPage_ListAndDetail exercises the golden path end to end: the
// list page shows the cached transaction and links to its detail page,
// which clearly labels UNCONFIRMED/MEMPOOL when the snapshot is fresh.
func TestMempoolPage_ListAndDetail(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	addr := "qMempoolPageDest"
	raw := rawSpendTx("mempool-page", 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(fakeHash("mempool-page-prev"), 0, "", "aa", "bb")},
		[]rpc.RawVout{rawVout(0, 5_00000000, p2pkhScript("mempool-page"), "pubkeyhash", &addr)},
	)
	if *raw.TxID == *raw.Hash {
		t.Fatalf("fixture bug: txid must differ from wtxid")
	}
	ctxn := mempoolCandidateTx(t, ctx, raw, 1234, 1_700_000_000, i64Ptr(10), boolPtr(true), nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("mempool-page-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// List page.
	rec := doRequest(t, s, "GET", "/mempool")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, ctxn.TxID)
	bodyContains(t, rec, "/mempool/tx/"+ctxn.TxID)
	bodyNotContains(t, rec, "not synchronized yet")

	// Detail page by txid. No confirmed blocks were ever applied (confirmed
	// tip is still the bootstrap -1/nil), so this snapshot — anchored at
	// height 1 — is legitimately STALE; assert the qualified wording (see
	// TestMempoolPage_FreshLabel for the genuinely fresh case).
	rec = doRequest(t, s, "GET", "/mempool/tx/"+ctxn.TxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, ctxn.TxID)
	bodyContains(t, rec, ctxn.WTxID)
	bodyContains(t, rec, "Present in cached mempool snapshot")

	// Detail page by wtxid — must resolve to the same transaction.
	rec = doRequest(t, s, "GET", "/mempool/tx/"+ctxn.WTxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("wtxid detail status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, ctxn.TxID)

	// Missing transaction: 404.
	rec = doRequest(t, s, "GET", "/mempool/tx/"+fakeHash("mempool-page-missing"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing tx status = %d, want 404", rec.Code)
	}
}

// TestMempoolPage_FreshLabel confirms the UNCONFIRMED/MEMPOOL label appears
// when the mempool anchor genuinely matches the confirmed indexed tip.
func TestMempoolPage_FreshLabel(t *testing.T) {
	ctx, s, pool, mstore := newMempoolTestServer(t)
	stw := store.New(pool) // real confirmed Store, SAME pool/schema as s

	label := "mempool-fresh-genesis"
	g := chain.Block{
		Hash: fakeHash(label), Height: 0, MerkleRoot: fakeHash("merkle:" + label),
		Time: 1_700_000_000, Bits: "1d00ffff", Difficulty: 1.0, Nonce: 0,
		Size: 200, Weight: 800, TxCount: 1,
		Transactions: []chain.Transaction{coinbaseTx(label, 100_00000000, "qMempoolFreshGenesis")},
	}
	if err := stw.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}

	ctxn := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("mempool-fresh"), 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(0, g.Hash, ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/mempool/tx/"+ctxn.TxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "UNCONFIRMED / MEMPOOL")
}

// TestMempoolPage_GenerationCursorRedirects is spec item 11's SSR
// requirement: a stale pagination cursor must redirect/reset cleanly to the
// first page, never render an error for normal asynchronous replacement.
func TestMempoolPage_GenerationCursorRedirects(t *testing.T) {
	req := "/mempool?generation=1&before_entry_time=1700000000&before_txid=" + fakeHash("nonexistent-cursor-anchor")
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (stale/mismatched cursor against an uninitialized/empty cache)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/mempool" {
		t.Fatalf("Location = %q, want /mempool", loc)
	}
}

// TestMempoolPage_GenerationCursorPartial is a malformed-input 400: the
// three cursor parameters must be supplied together or not at all.
func TestMempoolPage_GenerationCursorPartial(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)
	rec := doRequest(t, s, "GET", "/mempool?generation=1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestMempoolTxWitness_DefaultHidden_OptInIncludes mirrors the confirmed-tx
// page's witness policy for the mempool detail page: default hides raw
// witness bytes; ?include_witness=true shows them byte-exact, and the
// P2QPK output witness program is never called a public key.
func TestMempoolTxWitness_DefaultHidden_OptInIncludes(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	sigItem := bytes.Repeat([]byte{0xab}, script.P2QPKSignatureLength)
	pubkeyItem := bytes.Repeat([]byte{0xcd}, script.P2QPKPublicKeyLength)
	p2qpkProgram := bytes.Repeat([]byte{0xef}, script.P2QPKProgramLength)

	p2qpkAddr := "qP2QPKWebDest"
	raw := rawSpendTx("mempool-witness-web", 300, 250, 1000,
		[]rpc.RawVin{rawSpendVin(fakeHash("mempool-witness-web-prev"), 0, "", hex.EncodeToString(sigItem), hex.EncodeToString(pubkeyItem))},
		[]rpc.RawVout{rawVout(0, 25_00000000, witnessProgramScript(script.P2QPKWitnessVersion, p2qpkProgram), "witness_unknown", &p2qpkAddr)},
	)
	ctxn := mempoolCandidateTx(t, ctx, raw, 10_00000000, 1_700_000_000, i64Ptr(500), boolPtr(true), nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(500, fakeHash("mempool-witness-web-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	sigHex := hex.EncodeToString(sigItem)

	defaultRec := doRequest(t, s, "GET", "/mempool/tx/"+ctxn.TxID)
	if defaultRec.Code != http.StatusOK {
		t.Fatalf("default status = %d, body=%s", defaultRec.Code, defaultRec.Body.String())
	}
	bodyNotContains(t, defaultRec, sigHex)
	bodyContains(t, defaultRec, "raw data hidden by default")
	bodyContains(t, defaultRec, string(script.TypeP2QPK))

	rawRec := doRequest(t, s, "GET", "/mempool/tx/"+ctxn.TxID+"?include_witness=true")
	if rawRec.Code != http.StatusOK {
		t.Fatalf("include_witness status = %d, body=%s", rawRec.Code, rawRec.Body.String())
	}
	bodyContains(t, rawRec, sigHex)
	bodyContains(t, rawRec, hex.EncodeToString(pubkeyItem))
}

// TestSearch_MempoolFallback is spec item 27: a 64-hex search that misses
// every confirmed lookup but matches the current mempool cache redirects to
// /mempool/tx/{id}.
func TestSearch_MempoolFallback(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	ctxn := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("search-mempool"), 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("search-mempool-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/search?q="+ctxn.TxID)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302, body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/mempool/tx/"+ctxn.TxID {
		t.Fatalf("Location = %q, want /mempool/tx/%s", loc, ctxn.TxID)
	}
}

// TestSearch_ConfirmedTakesPriorityOverMempool is spec item 27's ordering
// requirement: a confirmed transaction must never be shadowed by a mempool
// match, even for the exact same hash. This state — the SAME txid present
// as both a confirmed transaction AND a cached mempool row — is
// architecturally real, not a contrived collision: it is exactly the
// asynchronous interval after confirmed indexing observes a transaction
// but before the NEXT mempool ReplaceSnapshot clears the now-stale cached
// row for it (see docs/ARCHITECTURE.md §23 "Freshness"). The fixture
// therefore explicitly overrides the mempool candidate's TxID/WTxID to
// match the confirmed transaction's own — proving actual same-txid
// precedence, not merely "an unrelated mempool row also exists".
func TestSearch_ConfirmedTakesPriorityOverMempool(t *testing.T) {
	ctx, s, pool, mstore := newMempoolTestServer(t)

	// Build a REAL confirmed store against the SAME pool as the mempool
	// fixture (newMempoolTestServer's Server already shares this pool).
	confirmedStore := store.New(pool)

	g := block("search-priority-g", 0, "", coinbaseTx("search-priority-g", 100_00000000, "qSearchPriorityG"))
	if err := confirmedStore.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	confirmedTxid := g.Transactions[0].TxID

	mtx := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("search-priority-mempool"), 1000, 1_700_000_000, nil, nil, nil)
	mtx.TxID = confirmedTxid
	mtx.WTxID = confirmedTxid
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("search-priority-tip"), mtx)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// Confirm the fixture actually put the SAME txid in both places before
	// asserting anything about search's ordering behavior.
	var mempoolCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mempool_transactions WHERE txid = $1`, confirmedTxid).Scan(&mempoolCount); err != nil {
		t.Fatalf("count mempool_transactions: %v", err)
	}
	if mempoolCount != 1 {
		t.Fatalf("fixture bug: mempool_transactions has %d rows for %s, want 1 (same-txid collision must actually exist)", mempoolCount, confirmedTxid)
	}

	rec := doRequest(t, s, "GET", "/search?q="+confirmedTxid)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/tx/"+confirmedTxid {
		t.Fatalf("Location = %q, want /tx/%s (confirmed must win)", loc, confirmedTxid)
	}
	if loc := rec.Header().Get("Location"); loc == "/mempool/tx/"+confirmedTxid {
		t.Fatalf("Location = %q, must NOT be the mempool destination", loc)
	}
}

// TestMempoolTxPage_PrevOutLink_ConfirmedParent is spec item 3.A: a mempool
// transaction input spending an ALREADY-CONFIRMED transaction (not one of
// this transaction's own in-mempool "depends" parents) must link to
// /tx/{prevTxid}, never /mempool/tx/{prevTxid} — the previous version
// linked every prevout to /mempool/tx/{prevTxid} unconditionally, which
// 404s whenever the prevout is confirmed rather than itself in the
// mempool.
func TestMempoolTxPage_PrevOutLink_ConfirmedParent(t *testing.T) {
	ctx, s, pool, mstore := newMempoolTestServer(t)
	confirmedStore := store.New(pool)

	// P: a real confirmed transaction (the genesis coinbase).
	g := block("prevout-confirmed-g", 0, "", coinbaseTx("prevout-confirmed-g", 100_00000000, "qPrevOutConfirmedG"))
	if err := confirmedStore.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	confirmedTxid := g.Transactions[0].TxID

	// C: a mempool transaction spending P. C.Depends must NOT contain P —
	// P is confirmed, not part of this (or any) mempool candidate.
	addr := "qPrevOutConfirmedChild"
	raw := rawSpendTx("prevout-confirmed-child", 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(confirmedTxid, 0, "473044")},
		[]rpc.RawVout{rawVout(0, 5_00000000, p2pkhScript("prevout-confirmed-child"), "pubkeyhash", &addr)},
	)
	child := mempoolCandidateTx(t, ctx, raw, 1000, 1_700_000_000, nil, nil, nil)
	if len(child.Depends) != 0 {
		t.Fatalf("fixture bug: child.Depends = %v, want empty (P is confirmed, not a mempool parent)", child.Depends)
	}
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(0, g.Hash, child)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/mempool/tx/"+child.TxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, `href="/tx/`+confirmedTxid+`"`)
	bodyNotContains(t, rec, `href="/mempool/tx/`+confirmedTxid+`"`)
}

// TestMempoolTxPage_PrevOutLink_MempoolParent is spec item 3.B: a mempool
// transaction input spending another CURRENT mempool transaction (listed
// in Depends) must link to /mempool/tx/{prevTxid} — proving the UI does
// not treat every prevout as confirmed either.
func TestMempoolTxPage_PrevOutLink_MempoolParent(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	parent := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("prevout-mempool-parent"), 1000, 1_700_000_000, nil, nil, nil)

	childAddr := "qPrevOutMempoolChild"
	childRaw := rawSpendTx("prevout-mempool-child", 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(parent.TxID, 0, "473044")},
		[]rpc.RawVout{rawVout(0, 3_00000000, p2pkhScript("prevout-mempool-child"), "pubkeyhash", &childAddr)},
	)
	child := mempoolCandidateTx(t, ctx, childRaw, 500, 1_700_000_050, nil, nil, []string{parent.TxID})

	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("prevout-mempool-tip"), parent, child)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/mempool/tx/"+child.TxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, `href="/mempool/tx/`+parent.TxID+`"`)
	bodyNotContains(t, rec, `href="/tx/`+parent.TxID+`"`)
}

// TestMempoolPage_SameHeightReorgWording is spec item 7: a same-height,
// different-hash mismatch between the mempool anchor and the confirmed
// indexed tip must still render as stale, using neutral "no longer
// matches" wording — never "advanced past"/"ahead of"/"newer than", which
// would misleadingly imply an ordering relationship a same-height reorg
// does not have.
func TestMempoolPage_SameHeightReorgWording(t *testing.T) {
	ctx, s, pool, mstore := newMempoolTestServer(t)
	confirmedStore := store.New(pool)

	anchorHash := fakeHash("web-same-height-reorg-A")
	confirmedLabel := "web-same-height-reorg-B"
	confirmedHash := fakeHash(confirmedLabel)
	if anchorHash == confirmedHash {
		t.Fatalf("fixture bug: anchor and confirmed hashes must differ")
	}

	ctxn := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("web-same-height-reorg-tx"), 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(0, anchorHash, ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	g := block(confirmedLabel, 0, "", coinbaseTx(confirmedLabel, 100_00000000, "qWebSameHeightReorgB"))
	if err := confirmedStore.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	if g.Hash != confirmedHash {
		t.Fatalf("fixture bug: g.Hash = %s, want %s", g.Hash, confirmedHash)
	}

	for _, path := range []string{"/mempool", "/mempool/tx/" + ctxn.TxID} {
		rec := doRequest(t, s, "GET", path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body=%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "advanced past") || strings.Contains(body, "ahead of") || strings.Contains(body, "newer than") {
			t.Fatalf("%s: wording implies forward advancement for a same-height mismatch:\n%s", path, body)
		}
		bodyContains(t, rec, "no longer matches")
	}
}

// TestMempoolPage_Escaping mirrors security_test.go's
// TestEscaping_AddressContainingHTMLSpecialCharacters for the mempool
// transaction detail page: an address containing HTML-special bytes must
// never render as raw, unescaped markup (spec item 42).
func TestMempoolPage_Escaping(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	evilAddress := `<script>alert(1)</script>&"'`
	raw := rawSpendTx("mempool-escaping", 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(fakeHash("mempool-escaping-prev"), 0, "473044")},
		[]rpc.RawVout{rawVout(0, 5_00000000, p2pkhScript("mempool-escaping"), "pubkeyhash", &evilAddress)},
	)
	ctxn := mempoolCandidateTx(t, ctx, raw, 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("mempool-escaping-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/mempool/tx/"+ctxn.TxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyNotContains(t, rec, "<script>alert(1)</script>")
	bodyContains(t, rec, "&lt;script&gt;")
}
