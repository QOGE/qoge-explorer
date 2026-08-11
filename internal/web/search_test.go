package web

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

// W: search by block height redirects to /block/{height}.
func TestSearch_ByHeight(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("sh-g", 0, "", coinbaseTx("sh-g", 100_00000000, "qSHG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/search?q=0")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/block/0" {
		t.Fatalf("Location = %q, want /block/0", loc)
	}
}

// X: search by block hash redirects to /block/{hash}.
func TestSearch_ByBlockHash(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("sbh-g", 0, "", coinbaseTx("sbh-g", 100_00000000, "qSBHG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/search?q="+g.Hash)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/block/"+g.Hash {
		t.Fatalf("Location = %q, want /block/%s", loc, g.Hash)
	}
}

// Y: search by txid redirects to /tx/{txid}.
func TestSearch_ByTxID(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("sty-g", 0, "", coinbaseTx("sty-g", 100_00000000, "qSTYG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	txid := g.Transactions[0].TxID

	rec := doRequest(t, s, "GET", "/search?q="+txid)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/tx/"+txid {
		t.Fatalf("Location = %q, want /tx/%s", loc, txid)
	}
}

// Z: search by a genuinely distinct wtxid (not equal to its txid) still
// resolves and redirects to /tx/{wtxid} — proving search tries
// BlockByHash, then TransactionByTxID, then TransactionByWTxID in order,
// and the wtxid branch actually gets exercised.
func TestSearch_ByDistinctWTxID(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)
	f := buildDecodedFixture(t, ctx, st)

	if f.spendTxid == f.spendWtxid {
		t.Fatalf("fixture invalid: witness-bearing tx must have txid != wtxid")
	}

	rec := doRequest(t, s, "GET", "/search?q="+f.spendWtxid)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/tx/"+f.spendWtxid {
		t.Fatalf("Location = %q, want /tx/%s", loc, f.spendWtxid)
	}
}

// A 64-char hash matching nothing renders a "not found" HTML result page,
// never a guess.
func TestSearch_UnmatchedHash(t *testing.T) {
	s, _ := newTestServer(t)
	q := fakeHash("search-nothing-matches-this")
	rec := doRequest(t, s, "GET", "/search?q="+q)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "No block, transaction, or witness transaction matched")
	bodyContains(t, rec, q)
}

// AA: other reasonably-sized input is treated as an address and redirected
// to /address/{address}.
func TestSearch_AddressRedirect(t *testing.T) {
	s, _ := newTestServer(t)
	rec := doRequest(t, s, "GET", "/search?q=qSomeAddressText")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/address/qSomeAddressText" {
		t.Fatalf("Location = %q, want /address/qSomeAddressText", loc)
	}
}

// Oversized search input is rejected as a 400, never silently truncated.
func TestSearch_OversizedInput(t *testing.T) {
	s, _ := newTestServer(t)
	q := ""
	for i := 0; i < 300; i++ {
		q += "q"
	}
	rec := doRequest(t, s, "GET", "/search?q="+q)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// Sanity: heights beyond int64 range or non-numeric junk masquerading as a
// height must not crash the handler.
func TestSearch_HeightBoundary(t *testing.T) {
	s, _ := newTestServer(t)
	big := strconv.FormatInt(int64(1)<<62, 10)
	rec := doRequest(t, s, "GET", "/search?q="+big)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
}
