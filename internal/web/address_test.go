package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// M: address summary — exact balance/received/sent strings, tx count.
func TestAddress_Summary(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("as-g", 0, "", coinbaseTx("as-g", 100_00000000, "qASG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("as-1", 1, g.Hash, coinbaseTx("as-1", 40_00000000, "qASRecv"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	rec := doRequest(t, s, "GET", "/address/qASRecv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "40.00000000 QOGE")
	bodyContains(t, rec, "(4000000000 sats)")

	// Never-used address: 200, zero balance, not a 404.
	rec = doRequest(t, s, "GET", "/address/qNeverUsedAnywhereWeb")
	if rec.Code != http.StatusOK {
		t.Fatalf("unused address status = %d, want 200", rec.Code)
	}
	bodyContains(t, rec, "0.00000000 QOGE")
}

// N: address history lists canonical touches with links to tx/block pages.
func TestAddress_History(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("ah-g", 0, "", coinbaseTx("ah-g", 100_00000000, "qAHG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("ah-1", 1, g.Hash, coinbaseTx("ah-1", 40_00000000, "qAHTarget"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}

	rec := doRequest(t, s, "GET", "/address/qAHTarget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "/tx/"+b1.Transactions[0].TxID)
	bodyContains(t, rec, "/block/"+b1.Hash)
}

// O: genesis P2PK destination shows zero balance yet visible canonical
// history — balance/accounting and historical destination visibility are
// deliberately independent (see query.Store.AddressHistory's doc comment).
func TestAddress_GenesisDestinationZeroBalanceVisibleHistory(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	txid := fakeHash("gad-web-genesis-tx")
	genesisTx := chain.Transaction{
		TxID: txid, WTxID: txid,
		Version: 1, LockTime: 0,
		Size: 100, VSize: 100, Weight: 400,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 0xffffffff},
		},
		Outputs: []chain.Output{
			{Index: 0, Value: chain.Amount(100_00000000), ScriptPubKey: p2pkScript("gad-web"), ScriptType: script.TypeP2PK, Address: "qGADWebGenesis"},
		},
	}
	g := block("gad-web-genesis", 0, "", genesisTx)
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/address/qGADWebGenesis")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "0.00000000 QOGE") // zero balance
	bodyContains(t, rec, "/tx/"+txid)       // yet visible in history
	bodyContains(t, rec, "/block/"+g.Hash)
	bodyNotContains(t, rec, "No canonical history for this address.")
}

// p2pkScript builds a structurally valid 35-byte bare-P2PK scriptPubKey:
// <push 33><compressed pubkey><OP_CHECKSIG> — mirrors
// internal/query/rpcfixtures_test.go's helper of the same name (duplicated
// per-package, see dbtest_test.go's note), so ScriptType/script bytes
// agree exactly as that package's review fix required.
func p2pkScript(label string) []byte {
	pubKey := make([]byte, 33)
	pubKey[0] = 0x02
	copy(pubKey[1:], []byte(label + "................................")[:32])
	s := make([]byte, 0, 35)
	s = append(s, 0x21)
	s = append(s, pubKey...)
	s = append(s, 0xac)
	return s
}

// P/Q: address history follows a reorg's new canonical branch, and a
// flip-back restores the original branch's history — mirroring
// internal/query's TestAddressHistory_ReorgAndFlipBack at the HTML layer.
func TestAddress_History_ReorgAndFlipBack(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("ahr-web-g", 0, "", coinbaseTx("ahr-web-g", 100_00000000, "qAHRWebG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("ahr-web-A1", 1, g.Hash, coinbaseTx("ahr-web-A1", 30_00000000, "qAHRWebTarget"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}

	// P (branch A): history shows A1.
	rec := doRequest(t, s, "GET", "/address/qAHRWebTarget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	bodyContains(t, rec, "/block/"+a1.Hash)

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	b1 := block("ahr-web-B1", 1, g.Hash, coinbaseTx("ahr-web-B1", 30_00000000, "qAHRWebOther"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	// P (branch B): qAHRWebTarget's history is now empty (A1 orphaned).
	rec = doRequest(t, s, "GET", "/address/qAHRWebTarget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	bodyContains(t, rec, "No canonical history for this address.")
	bodyNotContains(t, rec, "/block/"+a1.Hash)

	// Q: flip back to branch A restores the history.
	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback (flip back): %v", err)
	}
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("re-apply A1 (flip back): %v", err)
	}
	rec = doRequest(t, s, "GET", "/address/qAHRWebTarget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	bodyContains(t, rec, "/block/"+a1.Hash)
}

// Address history pagination preserves the caller's requested limit across
// the next-page link, same as /blocks (see viewmodels.go's
// addressPagination) — a naive cursor-only link would silently reset
// paging forward back to query.DefaultPageSize.
func TestAddress_History_PaginationPreservesLimit(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("ahp-g", 0, "", coinbaseTx("ahp-g", 100_00000000, "qAHPG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	prev := g
	for h := int64(1); h <= 3; h++ {
		label := "ahp-" + string(rune('a'+h))
		b := block(label, h, prev.Hash, coinbaseTx(label, 10_00000000, "qAHPTarget"))
		if err := st.ApplyBlock(ctx, b); err != nil {
			t.Fatalf("apply block %d: %v", h, err)
		}
		prev = b
	}

	rec := doRequest(t, s, "GET", "/address/qAHPTarget?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "limit=2") {
		t.Fatalf("next-page link does not preserve limit=2:\n%s", body)
	}
}

// R: bare-multisig participant identities render under a clearly separate
// "Participants" label on the transaction page, and never appear as an
// address's own monetary history/balance.
func TestMultisig_ParticipantsVisuallySeparate(t *testing.T) {
	ctx := context.Background()
	s, st := newTestServer(t)

	g := block("ms-web-g", 0, "", coinbaseTx("ms-web-g", 100_00000000, "qMSWebG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	participant1, participant2 := "qMSWebParticipant1", "qMSWebParticipant2"
	multisigTxid := fakeHash("ms-web-multisig-tx")
	multisigTx := chain.Transaction{
		TxID: multisigTxid, WTxID: multisigTxid,
		Version: 1, LockTime: 0,
		Size: 120, VSize: 120, Weight: 480,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 0xffffffff},
		},
		Outputs: []chain.Output{
			{
				Index: 0, Value: chain.Amount(50_00000000),
				ScriptPubKey:         p2pkhScript("ms-web-multisig-out"),
				ScriptType:           script.TypeMultisig,
				PubKeys:              [][]byte{{0x01}, {0x02}},
				ParticipantAddresses: []string{participant1, participant2},
			},
		},
	}
	b1 := block("ms-web-1", 1, g.Hash, multisigTx)
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply multisig block: %v", err)
	}

	rec := doRequest(t, s, "GET", "/tx/"+multisigTxid)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	bodyContains(t, rec, "Participants (not independent balance owners):")
	bodyContains(t, rec, "/address/"+participant1)
	bodyContains(t, rec, "/address/"+participant2)

	for _, addr := range []string{participant1, participant2} {
		rec := doRequest(t, s, "GET", "/address/"+addr)
		if rec.Code != http.StatusOK {
			t.Fatalf("participant address status = %d", rec.Code)
		}
		bodyContains(t, rec, "0.00000000 QOGE")
		bodyContains(t, rec, "No canonical history for this address.")
	}
}
