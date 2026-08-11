package api

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// TestMempoolEndpoint_Uninitialized is spec item 19: an uninitialized
// mempool cache is a valid HTTP 200 response with initialized=false and no
// transactions — never a fake synchronized-and-empty response.
func TestMempoolEndpoint_Uninitialized(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/api/v1/mempool")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		State struct {
			Initialized bool   `json:"initialized"`
			Status      string `json:"status"`
		} `json:"state"`
		Transactions struct {
			Transactions []any `json:"transactions"`
		} `json:"transactions"`
	}
	decodeBody(t, rec, &body)
	if body.State.Initialized {
		t.Fatalf("state.initialized = true, want false")
	}
	if body.State.Status != "uninitialized" {
		t.Fatalf("state.status = %q, want uninitialized", body.State.Status)
	}
	if len(body.Transactions.Transactions) != 0 {
		t.Fatalf("transactions = %+v, want empty", body.Transactions.Transactions)
	}
}

// TestMempoolEndpoint_ListAndDetail exercises the golden path: list shows
// the cached transaction, detail is reachable by both txid and wtxid, and
// the associated snapshot state/generation is included.
func TestMempoolEndpoint_ListAndDetail(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	addr := "qMempoolListDest"
	raw := rawSpendTx("mempool-list", 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(fakeHash("mempool-list-prev"), 0, "", "aa", "bb")},
		[]rpc.RawVout{rawVout(0, 5_00000000, p2pkhScript("mempool-list"), "pubkeyhash", &addr)},
	)
	if *raw.TxID == *raw.Hash {
		t.Fatalf("fixture bug: txid must differ from wtxid")
	}
	ctxn := mempoolCandidateTx(t, ctx, raw, 1234, 1_700_000_000, i64Ptr(10), boolPtr(true), nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("mempool-list-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// List.
	rec := doRequest(t, s, "GET", "/api/v1/mempool")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		State struct {
			Initialized bool   `json:"initialized"`
			Generation  int64  `json:"generation"`
			Status      string `json:"status"`
		} `json:"state"`
		Transactions struct {
			Transactions []struct {
				TxID        string `json:"txid"`
				WTxID       string `json:"wtxid"`
				FeeSatoshis int64  `json:"fee_satoshis"`
			} `json:"transactions"`
			Generation int64 `json:"generation"`
		} `json:"transactions"`
	}
	decodeBody(t, rec, &list)
	if !list.State.Initialized || list.State.Generation != 1 {
		t.Fatalf("list.State = %+v, want initialized=true generation=1", list.State)
	}
	if len(list.Transactions.Transactions) != 1 || list.Transactions.Transactions[0].TxID != ctxn.TxID {
		t.Fatalf("list transactions = %+v, want exactly %s", list.Transactions.Transactions, ctxn.TxID)
	}
	if list.Transactions.Transactions[0].FeeSatoshis != 1234 {
		t.Fatalf("fee_satoshis = %d, want 1234", list.Transactions.Transactions[0].FeeSatoshis)
	}

	// Detail by txid.
	recTxid := doRequest(t, s, "GET", "/api/v1/mempool/tx/"+ctxn.TxID)
	if recTxid.Code != http.StatusOK {
		t.Fatalf("detail by txid status = %d, body=%s", recTxid.Code, recTxid.Body.String())
	}
	var detail struct {
		TxID     string `json:"txid"`
		WTxID    string `json:"wtxid"`
		Snapshot struct {
			Generation int64 `json:"generation"`
		} `json:"snapshot"`
	}
	decodeBody(t, recTxid, &detail)
	if detail.TxID != ctxn.TxID || detail.WTxID != ctxn.WTxID {
		t.Fatalf("detail identities = %s/%s, want %s/%s", detail.TxID, detail.WTxID, ctxn.TxID, ctxn.WTxID)
	}
	if detail.Snapshot.Generation != 1 {
		t.Fatalf("detail.Snapshot.Generation = %d, want 1", detail.Snapshot.Generation)
	}

	// Detail by wtxid — must resolve to the same transaction.
	recWtxid := doRequest(t, s, "GET", "/api/v1/mempool/tx/"+ctxn.WTxID)
	if recWtxid.Code != http.StatusOK {
		t.Fatalf("detail by wtxid status = %d, body=%s", recWtxid.Code, recWtxid.Body.String())
	}
	var detailByW struct {
		TxID  string `json:"txid"`
		WTxID string `json:"wtxid"`
	}
	decodeBody(t, recWtxid, &detailByW)
	if detailByW.TxID != ctxn.TxID || detailByW.WTxID != ctxn.WTxID {
		t.Fatalf("wtxid-lookup identities = %s/%s, want %s/%s", detailByW.TxID, detailByW.WTxID, ctxn.TxID, ctxn.WTxID)
	}

	// Malformed identifier.
	rec = doRequest(t, s, "GET", "/api/v1/mempool/tx/short")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id status = %d, want 400", rec.Code)
	}

	// Well-formed but absent.
	rec = doRequest(t, s, "GET", "/api/v1/mempool/tx/"+fakeHash("mempool-not-present"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing mempool tx status = %d, want 404", rec.Code)
	}
}

// TestMempoolEndpoint_GenerationCursor is spec item 11: a cursor minted
// against a generation the mempool has since moved past must be rejected
// with HTTP 409 mempool_generation_changed, and a partial cursor (missing
// one of the three required parameters) must be rejected with 400.
func TestMempoolEndpoint_GenerationCursor(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	txA := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("cursor-A"), 1000, 1_700_000_100, nil, nil, nil)
	txB := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("cursor-B"), 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("cursor-tip-1"), txA, txB)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen1): %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/mempool?limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("page1 status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Transactions struct {
			NextCursor *struct {
				Generation int64  `json:"generation"`
				EntryTime  int64  `json:"before_entry_time"`
				TxID       string `json:"before_txid"`
			} `json:"next_cursor"`
		} `json:"transactions"`
	}
	decodeBody(t, rec, &page)
	if page.Transactions.NextCursor == nil {
		t.Fatalf("page1 has no next_cursor, want one (pageSize=1 with 2 rows available)")
	}
	cur := page.Transactions.NextCursor

	// Partial cursor: 400.
	rec = doRequest(t, s, "GET", "/api/v1/mempool?generation=1&before_entry_time=1700000000")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("partial cursor status = %d, want 400", rec.Code)
	}

	// Replace with a new generation.
	txC := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("cursor-C"), 1000, 1_700_000_050, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(2, fakeHash("cursor-tip-2"), txC)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen2): %v", err)
	}

	url := "/api/v1/mempool?generation=" + strconv.FormatInt(cur.Generation, 10) +
		"&before_entry_time=" + strconv.FormatInt(cur.EntryTime, 10) + "&before_txid=" + cur.TxID
	rec = doRequest(t, s, "GET", url)
	assertJSONError(t, rec, http.StatusConflict, "mempool_generation_changed")
}

// TestMempoolEndpoint_Stale is spec item 21: an initialized-but-stale
// snapshot must still return the cached rows, with stale=true and the
// mismatched anchor/confirmed-tip both visible.
func TestMempoolEndpoint_Stale(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(99, fakeHash("stale-anchor"))); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/mempool")
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

// TestMempoolTransactionWitness_DefaultOmitsRaw_OptInIncludesIt mirrors
// TestTransactionWitness_DefaultOmitsRaw_OptInIncludesIt for the mempool
// endpoint: default hides raw witness bytes, ?include_witness=true returns
// them byte-exact.
func TestMempoolTransactionWitness_DefaultOmitsRaw_OptInIncludesIt(t *testing.T) {
	ctx, s, _, mstore := newMempoolTestServer(t)

	sigItem := bytes.Repeat([]byte{0xab}, script.P2QPKSignatureLength)
	pubkeyItem := bytes.Repeat([]byte{0xcd}, script.P2QPKPublicKeyLength)
	p2qpkProgram := bytes.Repeat([]byte{0xef}, script.P2QPKProgramLength)
	p2trProgram := bytes.Repeat([]byte{0x11}, 32)

	p2qpkAddr := "qP2QPKApiDest"
	p2trAddr := "qP2TRApiDest"

	raw := rawSpendTx("mempool-witness-api", 300, 250, 1000,
		[]rpc.RawVin{rawSpendVin(fakeHash("mempool-witness-api-prev"), 0, "", hex.EncodeToString(sigItem), hex.EncodeToString(pubkeyItem))},
		[]rpc.RawVout{
			rawVout(0, 25_00000000, witnessProgramScript(script.P2QPKWitnessVersion, p2qpkProgram), "witness_unknown", &p2qpkAddr),
			rawVout(1, 10_00000000, witnessProgramScript(1, p2trProgram), "witness_v1_taproot", &p2trAddr),
		},
	)
	ctxn := mempoolCandidateTx(t, ctx, raw, 10_00000000, 1_700_000_000, i64Ptr(500), boolPtr(true), nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(500, fakeHash("mempool-witness-api-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	type witnessJSON struct {
		SizeBytes int     `json:"size_bytes"`
		DataHex   *string `json:"data_hex,omitempty"`
	}
	type outputJSON struct {
		ScriptType     string `json:"script_type"`
		WitnessVersion *int   `json:"witness_version,omitempty"`
	}
	type inputJSON struct {
		Witness []witnessJSON `json:"witness,omitempty"`
	}
	type txJSON struct {
		Outputs []outputJSON `json:"outputs"`
		Inputs  []inputJSON  `json:"inputs"`
	}

	defaultRec := doRequest(t, s, "GET", "/api/v1/mempool/tx/"+ctxn.TxID)
	if defaultRec.Code != http.StatusOK {
		t.Fatalf("default status = %d, body=%s", defaultRec.Code, defaultRec.Body.String())
	}
	var got txJSON
	decodeBody(t, defaultRec, &got)
	if got.Outputs[0].ScriptType != string(script.TypeP2QPK) {
		t.Fatalf("Outputs[0].ScriptType = %s, want p2qpk", got.Outputs[0].ScriptType)
	}
	if got.Outputs[1].ScriptType != string(script.TypeP2TR) {
		t.Fatalf("Outputs[1].ScriptType = %s, want p2tr", got.Outputs[1].ScriptType)
	}
	w := got.Inputs[0].Witness
	if len(w) != 2 || w[0].DataHex != nil || w[1].DataHex != nil {
		t.Fatalf("default witness = %+v, want 2 items with data_hex absent", w)
	}
	if w[0].SizeBytes != script.P2QPKSignatureLength || w[1].SizeBytes != script.P2QPKPublicKeyLength {
		t.Fatalf("default witness sizes = %+v", w)
	}

	rawWitnessRec := doRequest(t, s, "GET", "/api/v1/mempool/tx/"+ctxn.TxID+"?include_witness=true")
	if rawWitnessRec.Code != http.StatusOK {
		t.Fatalf("include_witness status = %d, body=%s", rawWitnessRec.Code, rawWitnessRec.Body.String())
	}
	var gotRaw txJSON
	decodeBody(t, rawWitnessRec, &gotRaw)
	wr := gotRaw.Inputs[0].Witness
	if wr[0].DataHex == nil || *wr[0].DataHex != hex.EncodeToString(sigItem) || len(*wr[0].DataHex)/2 != script.P2QPKSignatureLength {
		t.Fatalf("raw witness[0] not byte-exact")
	}
	if wr[1].DataHex == nil || *wr[1].DataHex != hex.EncodeToString(pubkeyItem) || len(*wr[1].DataHex)/2 != script.P2QPKPublicKeyLength {
		t.Fatalf("raw witness[1] not byte-exact")
	}
}

// TestMempoolEndpoint_MethodNotAllowed mirrors TestMethodNotAllowed for the
// two new mempool routes.
func TestMempoolEndpoint_MethodNotAllowed(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/mempool"},
		{"DELETE", "/api/v1/mempool/tx/" + fakeHash("x")},
	} {
		rec := doRequest(t, s, tc.method, tc.path)
		assertJSONError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}
