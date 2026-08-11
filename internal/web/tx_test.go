package web

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// I: transaction lookup by txid.
func TestTransaction_ByTxID(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("txi-g", 0, "", coinbaseTx("txi-g", 100_00000000, "qTxiG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/tx/"+g.Transactions[0].TxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, g.Transactions[0].TxID)
}

// J: transaction lookup by a genuinely DISTINCT wtxid (txid != wtxid),
// through the real decode pipeline, proving the TransactionByTxID-miss ->
// TransactionByWTxID-fallback path actually runs.
func TestTransaction_ByDistinctWTxID(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)
	f := buildDecodedFixture(t, ctx, st)

	if f.spendTxid == f.spendWtxid {
		t.Fatalf("fixture invalid: witness-bearing tx must have txid != wtxid")
	}

	byTxid := doRequest(t, s, "GET", "/tx/"+f.spendTxid)
	if byTxid.Code != http.StatusOK {
		t.Fatalf("by txid status = %d, body=%s", byTxid.Code, byTxid.Body.String())
	}
	bodyContains(t, byTxid, f.spendTxid)
	bodyContains(t, byTxid, f.spendWtxid)

	byWtxid := doRequest(t, s, "GET", "/tx/"+f.spendWtxid)
	if byWtxid.Code != http.StatusOK {
		t.Fatalf("by wtxid status = %d, body=%s", byWtxid.Code, byWtxid.Body.String())
	}
	bodyContains(t, byWtxid, f.spendTxid)
	bodyContains(t, byWtxid, f.spendWtxid)
}

// K/L: input/output ordering is preserved (vin/vout index order, never
// reordered), and money is rendered as the exact QOGE string plus
// satoshis, never a rounded/float rendering.
func TestTransaction_OrderingAndMoney(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("tom-g", 0, "", coinbaseTx("tom-g", 100_00000000, "qTomG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("tom-1", 1, g.Hash, coinbaseTx("tom-1", 50_00000000, "qTom1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}
	spend := spendTx("tom-spend", b1.Transactions[0].TxID, 0, 12_34567890, "qTomDest")
	b2 := block("tom-2", 2, b1.Hash, coinbaseTx("tom-2-cb", 50_00000000, "qTom2CB"), spend)
	if err := st.ApplyBlock(ctx, b2); err != nil {
		t.Fatalf("apply block2: %v", err)
	}

	rec := doRequest(t, s, "GET", "/tx/"+spend.TxID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Exact QOGE string, never a rounded/reformatted value.
	bodyContains(t, rec, "12.34567890 QOGE")
	bodyContains(t, rec, "(1234567890 sats)")

	// Input #0 precedes output #0 in document order (ordering sanity: the
	// single input's card appears before the single output's card).
	inputIdx := strings.Index(body, "Input #0")
	outputIdx := strings.Index(body, "Output #0")
	if inputIdx == -1 || outputIdx == -1 || inputIdx > outputIdx {
		t.Fatalf("expected Input #0 before Output #0 in body")
	}
}

// M/N covered in address_test.go. S/T: P2QPK and P2TR outputs render with
// their decoder-assigned script_type, witness version, and 32-byte
// program, visibly distinct from each other.
func TestTransaction_P2QPKAndP2TRPresentation(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)
	f := buildDecodedFixture(t, ctx, st)

	rec := doRequest(t, s, "GET", "/tx/"+f.spendTxid)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Output #0 &mdash; p2qpk") {
		t.Fatalf("output 0 not rendered as p2qpk:\n%s", body)
	}
	if !strings.Contains(body, "Output #1 &mdash; p2tr") {
		t.Fatalf("output 1 not rendered as p2tr:\n%s", body)
	}
	bodyContains(t, rec, "Witness version: 2")
	bodyContains(t, rec, "Witness version: 1")
	bodyContains(t, rec, hex.EncodeToString(f.p2qpkProgram))
	bodyContains(t, rec, "qWebDecP2QPKDest")
	bodyContains(t, rec, "qWebDecP2TRDest")

	p2qpkIdx := strings.Index(body, "Output #0")
	p2trIdx := strings.Index(body, "Output #1")
	if p2qpkIdx == -1 || p2trIdx == -1 || p2qpkIdx == p2trIdx {
		t.Fatalf("P2QPK and P2TR outputs not distinctly present")
	}
}

// U/V: default transaction page never contains the raw 17,088-byte P2QPK
// signature hex; explicit ?include_witness=true does, byte-exact.
func TestTransaction_P2QPKWitnessDefaultHiddenOptInExact(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)
	f := buildDecodedFixture(t, ctx, st)

	sigHex := hex.EncodeToString(f.sigItem)
	pubHex := hex.EncodeToString(f.pubkeyItem)

	defaultRec := doRequest(t, s, "GET", "/tx/"+f.spendTxid)
	if defaultRec.Code != http.StatusOK {
		t.Fatalf("default status = %d, body=%s", defaultRec.Code, defaultRec.Body.String())
	}
	bodyNotContains(t, defaultRec, sigHex)
	bodyContains(t, defaultRec, "17088 bytes")
	bodyContains(t, defaultRec, "(raw data hidden by default)")
	bodyContains(t, defaultRec, "?include_witness=true")

	rawRec := doRequest(t, s, "GET", "/tx/"+f.spendTxid+"?include_witness=true")
	if rawRec.Code != http.StatusOK {
		t.Fatalf("include_witness status = %d, body=%s", rawRec.Code, rawRec.Body.String())
	}
	bodyContains(t, rawRec, sigHex)
	bodyContains(t, rawRec, pubHex)

	// Malformed include_witness value -> HTML 400.
	rec := doRequest(t, s, "GET", "/tx/"+f.spendTxid+"?include_witness=maybe")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed include_witness status = %d, want 400", rec.Code)
	}
}
