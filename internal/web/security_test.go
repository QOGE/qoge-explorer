package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// AD/#24: blockchain-derived display content is auto-escaped by
// html/template, never rendered as executable markup. The `address` field
// is a genuinely suitable target: internal/store/internal/query/internal/api
// all treat it as an opaque string (see internal/web/validate.go's
// isValidAddressShape, mirroring internal/api's — any printable ASCII in
// 0x21-0x7e is accepted, which includes '<', '>', '&', '"', and '\”), so a
// real canonical output can legitimately carry these bytes as its address
// without the model needing to be weakened for this test. The evil string
// is built through the REAL Store.ApplyBlock write path (never injected via
// direct SQL), then read back through the real query.Store ->
// handleAddress -> html/template path exactly like any other address.
func TestEscaping_AddressContainingHTMLSpecialCharacters(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	evilAddress := `<script>alert(1)</script>&"'`

	g := block("esc-g", 0, "", coinbaseTx("esc-g", 100_00000000, "qEscG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	txid := fakeHash("esc-target-tx")
	tx := chain.Transaction{
		TxID: txid, WTxID: txid,
		Version: 1, LockTime: 0,
		Size: 100, VSize: 100, Weight: 400,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 0xffffffff},
		},
		Outputs: []chain.Output{
			{Index: 0, Value: chain.Amount(10_00000000), ScriptPubKey: p2pkhScript("esc-out"), ScriptType: script.TypeP2PKH, Address: evilAddress},
		},
	}
	b1 := block("esc-1", 1, g.Hash, tx)
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	// Round-trip through a real HTTP request path, exactly as a browser
	// would encode it — see the confirmed PathValue-decoding behavior this
	// relies on.
	rec := doRequest(t, s, "GET", "/address/"+url.PathEscape(evilAddress))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("raw, unescaped <script> tag present in rendered HTML — XSS:\n%s", body)
	}
	// html/template escapes '<' '>' '&' '"' '\'' distinctly; assert the
	// escaped form is present so this isn't a vacuous "absence" check.
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("expected escaped &lt;script&gt; in rendered HTML, not present:\n%s", body)
	}

	// Also exercise the transaction page's Address link for the same
	// output — a second independent template site using the same value.
	rec = doRequest(t, s, "GET", "/tx/"+txid)
	if rec.Code != http.StatusOK {
		t.Fatalf("tx status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("raw, unescaped <script> tag present on tx page — XSS:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("expected escaped &lt;script&gt; on tx page, not present:\n%s", body)
	}
}
