package query

import (
	"context"
	"testing"
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
