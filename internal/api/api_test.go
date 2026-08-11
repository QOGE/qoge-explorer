package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/script"
)

func doRequest(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}

func TestHealthz(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", rec.Code, rec.Body.String())
	}
}

func TestStatusEndpoint(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	rec := doRequest(t, s, "GET", "/api/v1/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var status struct {
		IndexedHeight int64   `json:"indexed_height"`
		IndexedHash   *string `json:"indexed_block_hash"`
	}
	decodeBody(t, rec, &status)
	if status.IndexedHeight != -1 || status.IndexedHash != nil {
		t.Fatalf("bootstrap status = %+v, want (-1, nil)", status)
	}

	g := block("status-g", 0, "", coinbaseTx("status-g", 100_00000000, "qStatusG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	rec = doRequest(t, s, "GET", "/api/v1/status")
	decodeBody(t, rec, &status)
	if status.IndexedHeight != 0 || status.IndexedHash == nil || *status.IndexedHash != g.Hash {
		t.Fatalf("status after genesis = %+v, want (0, %s)", status, g.Hash)
	}
}

func TestBlocksList_And_BlockLookup(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("bl-g", 0, "", coinbaseTx("bl-g", 100_00000000, "qBlG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("bl-1", 1, g.Hash, coinbaseTx("bl-1", 50_00000000, "qBl1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	// List.
	rec := doRequest(t, s, "GET", "/api/v1/blocks?limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Blocks []struct {
			Hash   string `json:"hash"`
			Height int64  `json:"height"`
		} `json:"blocks"`
		NextBeforeHeight *int64 `json:"next_before_height"`
	}
	decodeBody(t, rec, &page)
	if len(page.Blocks) != 1 || page.Blocks[0].Height != 1 {
		t.Fatalf("page = %+v, want one block at height 1", page)
	}
	if page.NextBeforeHeight == nil || *page.NextBeforeHeight != 1 {
		t.Fatalf("NextBeforeHeight = %v, want 1", page.NextBeforeHeight)
	}

	// Block by height.
	rec = doRequest(t, s, "GET", "/api/v1/block/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("block by height status = %d", rec.Code)
	}
	var byHeight struct {
		Hash      string `json:"hash"`
		Canonical bool   `json:"canonical"`
	}
	decodeBody(t, rec, &byHeight)
	if byHeight.Hash != b1.Hash || !byHeight.Canonical {
		t.Fatalf("block by height = %+v, want hash=%s canonical=true", byHeight, b1.Hash)
	}

	// Block by hash.
	rec = doRequest(t, s, "GET", "/api/v1/block/"+g.Hash)
	if rec.Code != http.StatusOK {
		t.Fatalf("block by hash status = %d", rec.Code)
	}

	// Malformed identifier.
	rec = doRequest(t, s, "GET", "/api/v1/block/not-a-valid-id")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed block id status = %d, want 400", rec.Code)
	}
	assertErrorEnvelope(t, rec)

	// Well-formed but absent.
	rec = doRequest(t, s, "GET", "/api/v1/block/999999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing block status = %d, want 404", rec.Code)
	}
	assertErrorEnvelope(t, rec)
}

func TestTransactionEndpoint(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("txe-g", 0, "", coinbaseTx("txe-g", 100_00000000, "qTxeG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/tx/"+g.Transactions[0].TxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var tx struct {
		TxID  string `json:"txid"`
		WTxID string `json:"wtxid"`
	}
	decodeBody(t, rec, &tx)
	if tx.TxID != g.Transactions[0].TxID {
		t.Fatalf("tx.TxID = %s, want %s", tx.TxID, g.Transactions[0].TxID)
	}

	// Lookup by this coinbase's own wtxid. Coinbase txid == wtxid, so this
	// is a coincidental re-lookup, NOT a proof the wtxid fallback path
	// works — see TestTransactionEndpoint_WTxIDFallback below for a
	// fixture where txid != wtxid, which actually forces
	// TransactionByTxID to miss and TransactionByWTxID to be exercised.
	rec = doRequest(t, s, "GET", "/api/v1/tx/"+g.Transactions[0].WTxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("wtxid lookup status = %d", rec.Code)
	}

	// Malformed identifier.
	rec = doRequest(t, s, "GET", "/api/v1/tx/short")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed txid status = %d, want 400", rec.Code)
	}

	// Well-formed but absent.
	rec = doRequest(t, s, "GET", "/api/v1/tx/"+fakeHash("nonexistent"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing tx status = %d, want 404", rec.Code)
	}
}

// 3 (review round): the wtxid identifier space is only genuinely exercised
// when txid != wtxid — a witness-bearing transaction, decoded through the
// real pipeline. GET by txid must succeed via TransactionByTxID directly;
// GET by wtxid must MISS TransactionByTxID (ErrNotFound) and fall back to
// TransactionByWTxID, and the response must report both identities
// distinctly and correctly regardless of which one was requested.
func TestTransactionEndpoint_WTxIDFallback(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)
	f := buildDecodedP2QPKFixture(t, ctx, st)

	if f.spendTxid == f.spendWtxid {
		t.Fatalf("fixture invalid: witness-bearing tx must have txid != wtxid")
	}

	type txJSON struct {
		TxID  string `json:"txid"`
		WTxID string `json:"wtxid"`
	}

	byTxid := doRequest(t, s, "GET", "/api/v1/tx/"+f.spendTxid)
	if byTxid.Code != http.StatusOK {
		t.Fatalf("txid lookup status = %d, body=%s", byTxid.Code, byTxid.Body.String())
	}
	var gotByTxid txJSON
	decodeBody(t, byTxid, &gotByTxid)
	if gotByTxid.TxID != f.spendTxid || gotByTxid.WTxID != f.spendWtxid {
		t.Fatalf("txid lookup = %+v, want (txid=%s, wtxid=%s)", gotByTxid, f.spendTxid, f.spendWtxid)
	}

	byWtxid := doRequest(t, s, "GET", "/api/v1/tx/"+f.spendWtxid)
	if byWtxid.Code != http.StatusOK {
		t.Fatalf("wtxid lookup status = %d, body=%s", byWtxid.Code, byWtxid.Body.String())
	}
	var gotByWtxid txJSON
	decodeBody(t, byWtxid, &gotByWtxid)
	if gotByWtxid.TxID != f.spendTxid || gotByWtxid.WTxID != f.spendWtxid {
		t.Fatalf("wtxid lookup = %+v, want (txid=%s, wtxid=%s) — proves the fallback path, not a coincidence",
			gotByWtxid, f.spendTxid, f.spendWtxid)
	}
}

// P2QPK witness policy at the HTTP layer, through the REAL decode pipeline
// (review item 1/2): the transaction travels rpc.RawBlock -> decode.DecodeBlock
// -> Store.ApplyBlock -> query.Store -> HTTP, so script.TypeP2QPK is the
// decoder's own classification of a structurally valid OP_2 PUSH32 <program>
// scriptPubKey, never a fixture's say-so. The default response body must
// never contain the raw signature hex and must report exact witness sizes
// with no data_hex; ?include_witness=true must return both witness items
// byte-exact after decoding, not merely via a substring match.
func TestTransactionWitness_DefaultOmitsRaw_OptInIncludesIt(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)
	f := buildDecodedP2QPKFixture(t, ctx, st)

	if f.spendTxid == f.spendWtxid {
		t.Fatalf("fixture invalid: witness-bearing tx must have txid != wtxid")
	}

	type witnessJSON struct {
		ItemIndex int     `json:"item_index"`
		SizeBytes int     `json:"size_bytes"`
		DataHex   *string `json:"data_hex,omitempty"`
	}
	type outputJSON struct {
		ScriptType     string  `json:"script_type"`
		Address        *string `json:"address,omitempty"`
		WitnessVersion *int    `json:"witness_version,omitempty"`
		WitnessProgram *string `json:"witness_program,omitempty"`
	}
	type inputJSON struct {
		Witness []witnessJSON `json:"witness,omitempty"`
	}
	type txJSON struct {
		Outputs []outputJSON `json:"outputs"`
		Inputs  []inputJSON  `json:"inputs"`
	}

	sigHexFull := hex.EncodeToString(f.sigItem)
	pubHexFull := hex.EncodeToString(f.pubkeyItem)
	programHexFull := hex.EncodeToString(f.program)

	// Default: structural P2QPK classification, no raw witness anywhere.
	defaultRec := doRequest(t, s, "GET", "/api/v1/tx/"+f.spendTxid)
	if defaultRec.Code != http.StatusOK {
		t.Fatalf("default status = %d, body=%s", defaultRec.Code, defaultRec.Body.String())
	}
	var got txJSON
	decodeBody(t, defaultRec, &got)

	if len(got.Outputs) != 1 {
		t.Fatalf("Outputs = %+v, want exactly 1", got.Outputs)
	}
	out := got.Outputs[0]
	if out.ScriptType != string(script.TypeP2QPK) {
		t.Fatalf("script_type = %s, want %s (decoder-assigned)", out.ScriptType, script.TypeP2QPK)
	}
	if out.WitnessVersion == nil || *out.WitnessVersion != script.P2QPKWitnessVersion {
		t.Fatalf("witness_version = %v, want %d", out.WitnessVersion, script.P2QPKWitnessVersion)
	}
	if out.WitnessProgram == nil || *out.WitnessProgram != programHexFull {
		t.Fatalf("witness_program = %v, want %s", out.WitnessProgram, programHexFull)
	}
	if out.Address == nil || *out.Address != "qDP2QPKDest" {
		t.Fatalf("address = %v, want qDP2QPKDest (Core-provided, persisted as-is)", out.Address)
	}

	if len(got.Inputs) != 1 || len(got.Inputs[0].Witness) != 2 {
		t.Fatalf("Inputs = %+v, want 1 input with a 2-item witness", got.Inputs)
	}
	w := got.Inputs[0].Witness
	if w[0].SizeBytes != script.P2QPKSignatureLength || w[0].DataHex != nil {
		t.Fatalf("default witness[0] = %+v, want size=%d data_hex=absent", w[0], script.P2QPKSignatureLength)
	}
	if w[1].SizeBytes != script.P2QPKPublicKeyLength || w[1].DataHex != nil {
		t.Fatalf("default witness[1] = %+v, want size=%d data_hex=absent", w[1], script.P2QPKPublicKeyLength)
	}

	// Belt-and-suspenders: the default body must not leak the signature hex
	// as a raw substring either, in addition to the structural check above.
	if strings.Contains(defaultRec.Body.String(), sigHexFull) {
		t.Fatalf("default transaction response leaks the raw P2QPK signature hex")
	}

	// Explicit opt-in: byte-exact raw witness, verified by decoding the hex
	// back to bytes and checking both content and exact length — not a
	// substring match.
	rawRec := doRequest(t, s, "GET", "/api/v1/tx/"+f.spendTxid+"?include_witness=true")
	if rawRec.Code != http.StatusOK {
		t.Fatalf("include_witness status = %d, body=%s", rawRec.Code, rawRec.Body.String())
	}
	var gotRaw txJSON
	decodeBody(t, rawRec, &gotRaw)
	wr := gotRaw.Inputs[0].Witness

	if wr[0].DataHex == nil || *wr[0].DataHex != sigHexFull {
		t.Fatalf("raw witness[0] not byte-exact")
	}
	decodedSig, err := hex.DecodeString(*wr[0].DataHex)
	if err != nil {
		t.Fatalf("decode witness[0] hex: %v", err)
	}
	if len(decodedSig) != script.P2QPKSignatureLength {
		t.Fatalf("decoded witness[0] length = %d, want exactly %d", len(decodedSig), script.P2QPKSignatureLength)
	}

	if wr[1].DataHex == nil || *wr[1].DataHex != pubHexFull {
		t.Fatalf("raw witness[1] not byte-exact")
	}
	decodedPub, err := hex.DecodeString(*wr[1].DataHex)
	if err != nil {
		t.Fatalf("decode witness[1] hex: %v", err)
	}
	if len(decodedPub) != script.P2QPKPublicKeyLength {
		t.Fatalf("decoded witness[1] length = %d, want exactly %d", len(decodedPub), script.P2QPKPublicKeyLength)
	}

	// Malformed include_witness value.
	rec := doRequest(t, s, "GET", "/api/v1/tx/"+f.spendTxid+"?include_witness=maybe")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed include_witness status = %d, want 400", rec.Code)
	}
}

func TestAddressEndpoints(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("ae-g", 0, "", coinbaseTx("ae-g", 100_00000000, "qAeG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("ae-1", 1, g.Hash, coinbaseTx("ae-1", 30_00000000, "qAeAddr"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/address/qAeAddr")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var sum struct {
		BalanceSatoshis int64  `json:"balance_sats"`
		BalanceQOGE     string `json:"balance_qoge"`
	}
	decodeBody(t, rec, &sum)
	if sum.BalanceSatoshis != 30_00000000 || sum.BalanceQOGE != "30.00000000" {
		t.Fatalf("summary = %+v, want (3000000000, 30.00000000)", sum)
	}

	// Never-used address: 200 with zero balance, not 404.
	rec = doRequest(t, s, "GET", "/api/v1/address/qNeverUsedAnywhere")
	if rec.Code != http.StatusOK {
		t.Fatalf("unused address status = %d, want 200", rec.Code)
	}

	// History.
	rec = doRequest(t, s, "GET", "/api/v1/address/qAeAddr/transactions")
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var hist struct {
		Transactions []struct {
			TxID string `json:"txid"`
		} `json:"transactions"`
	}
	decodeBody(t, rec, &hist)
	if len(hist.Transactions) != 1 || hist.Transactions[0].TxID != b1.Transactions[0].TxID {
		t.Fatalf("history = %+v", hist)
	}

	// Cursor must be supplied as a pair.
	rec = doRequest(t, s, "GET", "/api/v1/address/qAeAddr/transactions?before=1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("partial cursor status = %d, want 400", rec.Code)
	}

	// Malformed address (too long).
	rec = doRequest(t, s, "GET", "/api/v1/address/"+strings.Repeat("q", 200))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized address status = %d, want 400", rec.Code)
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/api/v1/block/not-valid")
	assertJSONError(t, rec, http.StatusBadRequest, "bad_request")
}

// assertErrorEnvelope checks the JSON error envelope shape (code/message
// both present) and that the body never leaks internal detail, without
// asserting a specific status/code — used where the caller already checked
// those separately. rec.Body is only ever read here via its already-final
// string form (Body.String() does not consume/advance the buffer, unlike
// decoding straight from rec.Body, which would leave nothing for a second
// check to read).
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	raw := rec.Body.String()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode error body %q: %v", raw, err)
	}
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("error envelope incomplete: %+v", body)
	}
	if strings.Contains(strings.ToLower(raw), "select ") || strings.Contains(raw, "password") {
		t.Fatalf("error body leaks internal detail: %s", raw)
	}
}

// assertJSONError additionally requires the exact status code and
// error.code.
func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, wantStatus, rec.Body.String())
	}
	assertErrorEnvelope(t, rec)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if body.Error.Code != wantCode {
		t.Fatalf("error.code = %q, want %q", body.Error.Code, wantCode)
	}
}

// 10, 11, 12: unknown paths and wrong methods on known routes both return
// the JSON error envelope with the correct status code — never
// net/http's plain-text default, and a wrong method must never be
// silently reported as a fake 404 (or vice versa).
func TestMethodNotAllowed(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	g := block("mna-g", 0, "", coinbaseTx("mna-g", 100_00000000, "qMnaG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	validTxid := g.Transactions[0].TxID

	for _, tc := range []struct {
		method, path string
	}{
		{"POST", "/api/v1/status"},
		{"POST", "/api/v1/tx/" + validTxid},
		{"PUT", "/api/v1/blocks"},
		{"DELETE", "/api/v1/block/1"},
	} {
		rec := doRequest(t, s, tc.method, tc.path)
		assertJSONError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Fatalf("%s %s: missing Allow header", tc.method, tc.path)
		}
	}
}

func TestUnknownRoute(t *testing.T) {
	s, _ := newTestServer(t)

	// Unknown path, GET.
	rec := doRequest(t, s, "GET", "/api/v1/does-not-exist")
	assertJSONError(t, rec, http.StatusNotFound, "not_found")

	// Unknown path, non-GET: must still be a genuine 404, never a fake 405
	// (there is no real route at this path to be "wrong-methoded" against).
	rec = doRequest(t, s, "POST", "/api/v1/does-not-exist")
	assertJSONError(t, rec, http.StatusNotFound, "not_found")
}

func TestNoMutationEndpoints(t *testing.T) {
	s, _ := newTestServer(t)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		rec := doRequest(t, s, method, "/api/v1/status")
		if rec.Code == http.StatusOK {
			t.Fatalf("%s /api/v1/status returned 200, want a non-mutating rejection", method)
		}
	}
}
