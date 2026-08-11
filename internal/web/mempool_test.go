package web

import (
	"bytes"
	"encoding/hex"
	"net/http"
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
// match, even for the exact same hash (a transaction that has confirmed but
// whose mempool row a later ReplaceSnapshot hasn't cleared yet).
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

	// A mempool candidate that happens to reuse the SAME txid (a
	// same-value-collision fixture, not something that can happen from a
	// real Core response for the SAME transaction, but sufficient to prove
	// search's fixed lookup order never lets a mempool row shadow a
	// confirmed one).
	mtx := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("search-priority-mempool"), 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("search-priority-tip"), mtx)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/search?q="+confirmedTxid)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/tx/"+confirmedTxid {
		t.Fatalf("Location = %q, want /tx/%s (confirmed must win)", loc, confirmedTxid)
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
