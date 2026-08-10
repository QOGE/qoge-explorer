package query

import (
	"context"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// O: address canonical balance.
func TestAddressSummary(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("addr-genesis", 0, "", coinbaseTx("addr-genesis", 100_00000000, "qAddrGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("addr-1", 1, g.Hash, coinbaseTx("addr-1", 40_00000000, "qAddrRecv"))
	// Genesis output can never be spent (isGenesis skip), so build a
	// spendable chain: block1's own coinbase is spent in block2.
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}
	b1cbTxid := b1.Transactions[0].TxID
	spend := spendTx("addr-spend", b1cbTxid, 0, 25_00000000, "qAddrRecv2")
	b2 := block("addr-2", 2, b1.Hash, coinbaseTx("addr-2-cb", 50_00000000, "qAddrCB2"), spend)
	if err := st.ApplyBlock(ctx, b2); err != nil {
		t.Fatalf("apply block2: %v", err)
	}

	// qAddrRecv received 40, spent all of it (down to 25 after a 15 fee):
	// balance 0, total_received 40, total_sent 40.
	sum, err := q.AddressSummary(ctx, "qAddrRecv")
	if err != nil {
		t.Fatalf("AddressSummary(qAddrRecv): %v", err)
	}
	if sum.BalanceSatoshis != 0 || sum.TotalReceivedSatoshis != 40_00000000 || sum.TotalSentSatoshis != 40_00000000 {
		t.Fatalf("qAddrRecv summary = %+v", sum)
	}
	if sum.BalanceQOGE != "0.00000000" || sum.TotalReceivedQOGE != "40.00000000" {
		t.Fatalf("qAddrRecv money strings = %+v", sum)
	}

	// qAddrRecv2 received 25, unspent: balance 25.
	sum2, err := q.AddressSummary(ctx, "qAddrRecv2")
	if err != nil {
		t.Fatalf("AddressSummary(qAddrRecv2): %v", err)
	}
	if sum2.BalanceSatoshis != 25_00000000 || sum2.BalanceQOGE != "25.00000000" {
		t.Fatalf("qAddrRecv2 summary = %+v", sum2)
	}

	// An address with no canonical activity at all: zero-value summary,
	// not an error.
	unused, err := q.AddressSummary(ctx, "qNeverUsed")
	if err != nil {
		t.Fatalf("AddressSummary(unused): %v", err)
	}
	if unused.BalanceSatoshis != 0 || unused.TxCount != 0 || unused.BalanceQOGE != "0.00000000" {
		t.Fatalf("unused address summary = %+v, want all-zero", unused)
	}
}

// P: address history pagination.
func TestAddressHistory_Pagination(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("addrh-genesis", 0, "", coinbaseTx("addrh-genesis", 100_00000000, "qAddrHGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	prev := g
	const n = 5
	for h := int64(1); h <= n; h++ {
		label := labelFor("addrh", h)
		b := block(label, h, prev.Hash, coinbaseTx(label, 10_00000000, "qAddrHRepeat"))
		if err := st.ApplyBlock(ctx, b); err != nil {
			t.Fatalf("apply block %d: %v", h, err)
		}
		prev = b
	}

	page1, err := q.AddressHistory(ctx, "qAddrHRepeat", nil, nil, 2)
	if err != nil {
		t.Fatalf("AddressHistory page1: %v", err)
	}
	if len(page1.Transactions) != 2 || page1.Transactions[0].BlockHeight != 5 || page1.Transactions[1].BlockHeight != 4 {
		t.Fatalf("page1 = %+v, want heights [5,4]", page1.Transactions)
	}
	if page1.NextBeforeHeight == nil {
		t.Fatalf("page1.NextBeforeHeight = nil, want a cursor")
	}

	page2, err := q.AddressHistory(ctx, "qAddrHRepeat", page1.NextBeforeHeight, page1.NextBeforeTxID, 2)
	if err != nil {
		t.Fatalf("AddressHistory page2: %v", err)
	}
	if len(page2.Transactions) != 2 || page2.Transactions[0].BlockHeight != 3 || page2.Transactions[1].BlockHeight != 2 {
		t.Fatalf("page2 = %+v, want heights [3,2]", page2.Transactions)
	}

	page3, err := q.AddressHistory(ctx, "qAddrHRepeat", page2.NextBeforeHeight, page2.NextBeforeTxID, 2)
	if err != nil {
		t.Fatalf("AddressHistory page3: %v", err)
	}
	if len(page3.Transactions) != 1 || page3.Transactions[0].BlockHeight != 1 {
		t.Fatalf("page3 = %+v, want [height 1]", page3.Transactions)
	}
	if page3.NextBeforeHeight != nil {
		t.Fatalf("page3.NextBeforeHeight = %v, want nil (last page)", page3.NextBeforeHeight)
	}
}

// Q: orphaned output excluded from current address balance and history.
func TestAddressSummary_OrphanExcluded(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("addro-genesis", 0, "", coinbaseTx("addro-genesis", 100_00000000, "qAddrOGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("addro-A1", 1, g.Hash, coinbaseTx("addro-A1", 30_00000000, "qAddrOTarget"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}

	sumBefore, err := q.AddressSummary(ctx, "qAddrOTarget")
	if err != nil {
		t.Fatalf("AddressSummary before reorg: %v", err)
	}
	if sumBefore.BalanceSatoshis != 30_00000000 {
		t.Fatalf("balance before reorg = %d, want 3000000000", sumBefore.BalanceSatoshis)
	}

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	// Replace A1 with a different block at height 1 that never pays
	// qAddrOTarget at all.
	b1 := block("addro-B1", 1, g.Hash, coinbaseTx("addro-B1", 30_00000000, "qAddrOOther"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply replacement B1: %v", err)
	}

	sumAfter, err := q.AddressSummary(ctx, "qAddrOTarget")
	if err != nil {
		t.Fatalf("AddressSummary after reorg: %v", err)
	}
	if sumAfter.BalanceSatoshis != 0 || sumAfter.TxCount != 0 {
		t.Fatalf("qAddrOTarget summary after reorg = %+v, want all-zero (orphaned output excluded)", sumAfter)
	}

	hist, err := q.AddressHistory(ctx, "qAddrOTarget", nil, nil, 10)
	if err != nil {
		t.Fatalf("AddressHistory after reorg: %v", err)
	}
	if len(hist.Transactions) != 0 {
		t.Fatalf("qAddrOTarget history after reorg = %+v, want empty", hist.Transactions)
	}
}

// genesisP2PKTx builds a coinbase transaction with a single bare-P2PK
// output — used to build the genesis-address edge fixture below. Unlike
// coinbaseTx (P2PKH), this exercises a script_type that Store deliberately
// never inserts into utxo_state for height 0 (isGenesis) regardless of
// script type — see Store.ApplyBlock's "Core UTXO semantics".
func genesisP2PKTx(label string, valueSats int64, addr string) chain.Transaction {
	txid := fakeHash(label + "-tx")
	return chain.Transaction{
		TxID: txid, WTxID: txid,
		Version: 1, LockTime: 0,
		Size: 100, VSize: 100, Weight: 400,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 0xffffffff},
		},
		Outputs: []chain.Output{
			{Index: 0, Value: chain.Amount(valueSats), ScriptPubKey: p2pkhScript(label), ScriptType: script.TypeP2PK, Address: addr},
		},
	}
}

// 9: genesis address negative/edge test — balance semantics != historical
// visibility. The canonical genesis P2PK destination has zero spendable
// balance (Core/Store never insert height-0 outputs into utxo_state at
// all — see Store.ApplyBlock's "Core UTXO semantics" doc comment), but its
// address MUST still show the canonical genesis transaction as a
// received/destination occurrence in AddressHistory — dropping it would
// mean real, canonical, immutably-persisted destination data becomes
// invisible to the explorer merely because Core never treats a genesis
// coinbase as spendable.
func TestAddressHistory_GenesisDestinationVisibleDespiteZeroBalance(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	genesisTx := genesisP2PKTx("gad-genesis", 100_00000000, "qGenesisP2PK")
	g := block("gad-genesis", 0, "", genesisTx)
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	// Balance semantics: zero, exactly like any other never-spendable
	// output — genesis was never inserted into utxo_state, so there is no
	// addresses cache row for it at all.
	sum, err := q.AddressSummary(ctx, "qGenesisP2PK")
	if err != nil {
		t.Fatalf("AddressSummary(genesis destination): %v", err)
	}
	if sum.BalanceSatoshis != 0 || sum.TxCount != 0 || sum.BalanceQOGE != "0.00000000" {
		t.Fatalf("genesis destination summary = %+v, want all-zero (never in utxo_state)", sum)
	}

	// Historical visibility: the canonical genesis transaction MUST still
	// appear as a receive-side occurrence.
	hist, err := q.AddressHistory(ctx, "qGenesisP2PK", nil, nil, 10)
	if err != nil {
		t.Fatalf("AddressHistory(genesis destination): %v", err)
	}
	if len(hist.Transactions) != 1 {
		t.Fatalf("genesis destination history = %+v, want exactly 1 entry", hist.Transactions)
	}
	entry := hist.Transactions[0]
	if entry.TxID != genesisTx.TxID || entry.BlockHash != g.Hash || entry.BlockHeight != 0 {
		t.Fatalf("genesis destination history entry = %+v, want (txid=%s, block=%s, height=0)",
			entry, genesisTx.TxID, g.Hash)
	}

	// Also confirm the underlying block/tx data really is canonical and
	// really does carry this destination — the history entry isn't
	// papering over missing data.
	detail, err := q.BlockByHeight(ctx, 0)
	if err != nil {
		t.Fatalf("BlockByHeight(0): %v", err)
	}
	if !detail.Canonical {
		t.Fatalf("genesis block Canonical = false, want true")
	}
}

// 9 (continued): reorg A->B updates address history to the new canonical
// branch, and flip-back (B->A) restores the original branch's history —
// mirroring the block-level reorg/flip-back tests in reorg_test.go, but for
// AddressHistory specifically.
func TestAddressHistory_ReorgAndFlipBack(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("ahr-genesis", 0, "", coinbaseTx("ahr-genesis", 100_00000000, "qAHRGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("ahr-A1", 1, g.Hash, coinbaseTx("ahr-A1", 30_00000000, "qAHRTarget"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}

	hist, err := q.AddressHistory(ctx, "qAHRTarget", nil, nil, 10)
	if err != nil {
		t.Fatalf("AddressHistory (branch A): %v", err)
	}
	if len(hist.Transactions) != 1 || hist.Transactions[0].BlockHash != a1.Hash {
		t.Fatalf("history on branch A = %+v, want [A1]", hist.Transactions)
	}

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("ahr-B1", 1, g.Hash, coinbaseTx("ahr-B1", 30_00000000, "qAHROther"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	histAfterReorg, err := q.AddressHistory(ctx, "qAHRTarget", nil, nil, 10)
	if err != nil {
		t.Fatalf("AddressHistory (branch B): %v", err)
	}
	if len(histAfterReorg.Transactions) != 0 {
		t.Fatalf("qAHRTarget history on branch B = %+v, want empty (A1 now orphaned)", histAfterReorg.Transactions)
	}

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis (flip back): %v", err)
	}
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("re-apply A1 (flip back): %v", err)
	}

	histFlippedBack, err := q.AddressHistory(ctx, "qAHRTarget", nil, nil, 10)
	if err != nil {
		t.Fatalf("AddressHistory (flipped back to A): %v", err)
	}
	if len(histFlippedBack.Transactions) != 1 || histFlippedBack.Transactions[0].BlockHash != a1.Hash {
		t.Fatalf("history after flip-back = %+v, want [A1] restored", histFlippedBack.Transactions)
	}
}

// 9 (continued): canonical spends appear in the spent-from address's
// history, as a distinct entry from the receiving transaction.
func TestAddressHistory_CanonicalSpendAppears(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("ahs-genesis", 0, "", coinbaseTx("ahs-genesis", 100_00000000, "qAHSGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	b1 := block("ahs-1", 1, g.Hash, coinbaseTx("ahs-1", 40_00000000, "qAHSSource"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply block1: %v", err)
	}
	spend := spendTx("ahs-spend", b1.Transactions[0].TxID, 0, 30_00000000, "qAHSDest")
	b2 := block("ahs-2", 2, b1.Hash, coinbaseTx("ahs-2-cb", 50_00000000, "qAHSCB2"), spend)
	if err := st.ApplyBlock(ctx, b2); err != nil {
		t.Fatalf("apply block2: %v", err)
	}

	hist, err := q.AddressHistory(ctx, "qAHSSource", nil, nil, 10)
	if err != nil {
		t.Fatalf("AddressHistory(qAHSSource): %v", err)
	}
	if len(hist.Transactions) != 2 {
		t.Fatalf("qAHSSource history = %+v, want 2 entries (received + spent)", hist.Transactions)
	}
	var sawReceive, sawSpend bool
	for _, e := range hist.Transactions {
		switch e.TxID {
		case b1.Transactions[0].TxID:
			sawReceive = true
		case spend.TxID:
			sawSpend = true
		}
	}
	if !sawReceive || !sawSpend {
		t.Fatalf("qAHSSource history = %+v, want both the receiving tx (%s) and the spending tx (%s)",
			hist.Transactions, b1.Transactions[0].TxID, spend.TxID)
	}
}

// 9 (continued): bare-multisig participant identities never become
// monetary address history — output_participants is search/display only.
func TestAddressHistory_MultisigParticipantsExcluded(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("ahm-genesis", 0, "", coinbaseTx("ahm-genesis", 100_00000000, "qAHMGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	participant1, participant2 := "qAHMParticipant1", "qAHMParticipant2"
	multisigTxid := fakeHash("ahm-multisig-tx")
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
				ScriptPubKey:         p2pkhScript("ahm-multisig-out"),
				ScriptType:           script.TypeMultisig,
				PubKeys:              [][]byte{{0x01}, {0x02}},
				ParticipantAddresses: []string{participant1, participant2},
			},
		},
	}
	b1 := block("ahm-1", 1, g.Hash, multisigTx)
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply multisig block: %v", err)
	}

	for _, addr := range []string{participant1, participant2} {
		hist, err := q.AddressHistory(ctx, addr, nil, nil, 10)
		if err != nil {
			t.Fatalf("AddressHistory(%s): %v", addr, err)
		}
		if len(hist.Transactions) != 0 {
			t.Fatalf("participant %s history = %+v, want empty (participants are never monetary history)", addr, hist.Transactions)
		}
		sum, err := q.AddressSummary(ctx, addr)
		if err != nil {
			t.Fatalf("AddressSummary(%s): %v", addr, err)
		}
		if sum.BalanceSatoshis != 0 || sum.TxCount != 0 {
			t.Fatalf("participant %s summary = %+v, want all-zero", addr, sum)
		}
	}
}
