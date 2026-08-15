package query

import (
	"context"
	"strconv"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// multisigOutput builds a bare-multisig output whose value is never a
// balance-accounting destination for any of its participants — mirrors
// internal/store/apply_test.go's multisigOut fixture (duplicated rather
// than imported: internal/query must not depend on internal/store's test
// helpers).
func multisigOutput(index uint32, valueSats int64, pubkeys [][]byte, addrs []string) chain.Output {
	return chain.Output{
		Index:                index,
		Value:                chain.Amount(valueSats),
		ScriptPubKey:         []byte{0x51},
		ScriptType:           script.TypeMultisig,
		PubKeys:              pubkeys,
		ParticipantAddresses: addrs,
	}
}

// p2qpkOutput builds a structurally valid P2QPK output paying addr —
// exercises the ordinary (non-multisig, non-P2PKH) balance-accounting
// destination path.
func p2qpkOutput(index uint32, valueSats int64, addr string) chain.Output {
	v := script.P2QPKWitnessVersion
	return chain.Output{
		Index:          index,
		Value:          chain.Amount(valueSats),
		ScriptPubKey:   []byte{0x52, 0x20},
		ScriptType:     script.TypeP2QPK,
		WitnessVersion: &v,
		WitnessProgram: []byte{
			0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab,
			0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab,
			0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab,
			0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab, 0xab,
		},
		Address: addr,
	}
}

// TestRichListOverview_EmptyDatabase is spec item 18: an uninitialized
// explorer is a valid HTTP 200 (query-layer: no error) with an empty
// entries list — never confused with "zero wealth" or "zero holders".
func TestRichListOverview_EmptyDatabase(t *testing.T) {
	ctx := context.Background()
	q, _, _ := newTestQueryStore(t)

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview on empty database: %v", err)
	}
	if got.IndexedHeight != -1 {
		t.Errorf("IndexedHeight = %d, want -1", got.IndexedHeight)
	}
	if got.IndexedHash != nil {
		t.Errorf("IndexedHash = %v, want nil", got.IndexedHash)
	}
	if len(got.Entries) != 0 {
		t.Errorf("Entries = %+v, want empty", got.Entries)
	}
}

// TestRichListOverview_GenesisOnly is spec item 19: genesis outputs are
// never placed in Core's UTXO set (internal/store's applyOutput), so
// recomputeAddress's addr_utxos CTE — which JOINs output_addresses to
// utxo_state — never produces a row for the genesis destination address,
// and the addresses cache never gains an entry for it. A genesis-only
// database must therefore report entries=[] even though SupplyOverview
// reports non-zero subsidy/coinbase-output values for the same chain.
func TestRichListOverview_GenesisOnly(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-genesis-only", 0, "", coinbaseTx("richlist-genesis-only", 100*qoge, "qRichlistGenesisOnly"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview: %v", err)
	}
	if len(got.Entries) != 0 {
		t.Errorf("Entries = %+v, want empty (genesis outputs are never spendable UTXOs)", got.Entries)
	}
}

// TestRichListOverview_NormalOrdering is spec item 20: three positive
// addresses must be ranked strictly by descending balance.
func TestRichListOverview_NormalOrdering(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-normal-g", 0, "", coinbaseTx("richlist-normal-g", 100*qoge, "qRichlistNormalG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("richlist-normal-h1", 1, g.Hash, coinbaseTx("richlist-normal-h1", 50*qoge, "qRichlistNormalA"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}
	h2 := block("richlist-normal-h2", 2, h1.Hash, coinbaseTx("richlist-normal-h2", 100*qoge, "qRichlistNormalB"))
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply h2: %v", err)
	}
	h3 := block("richlist-normal-h3", 3, h2.Hash, coinbaseTx("richlist-normal-h3", 25*qoge, "qRichlistNormalC"))
	if err := st.ApplyBlock(ctx, h3); err != nil {
		t.Fatalf("apply h3: %v", err)
	}

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview: %v", err)
	}
	if len(got.Entries) != 3 {
		t.Fatalf("Entries = %+v, want 3", got.Entries)
	}
	want := []struct {
		rank    int
		addr    string
		balance int64
	}{
		{1, "qRichlistNormalB", 100 * qoge},
		{2, "qRichlistNormalA", 50 * qoge},
		{3, "qRichlistNormalC", 25 * qoge},
	}
	for i, w := range want {
		e := got.Entries[i]
		if e.Rank != w.rank || e.Address != w.addr || e.BalanceSatoshis != w.balance {
			t.Errorf("Entries[%d] = %+v, want rank=%d address=%s balance=%d", i, e, w.rank, w.addr, w.balance)
		}
	}
}

// TestRichListOverview_TieOrder is spec item 21: two addresses with
// exactly equal balances must be ordered deterministically (address
// COLLATE "C" ASC), and repeated calls must return the same order.
func TestRichListOverview_TieOrder(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-tie-g", 0, "", coinbaseTx("richlist-tie-g", 100*qoge, "qRichlistTieG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	// Same value, addresses chosen so lexical order is unambiguous:
	// "qRichlistTieA" < "qRichlistTieB" under a byte-wise/"C" comparison.
	h1 := block("richlist-tie-h1", 1, g.Hash, coinbaseTx("richlist-tie-h1", 50*qoge, "qRichlistTieB"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}
	h2 := block("richlist-tie-h2", 2, h1.Hash, coinbaseTx("richlist-tie-h2", 50*qoge, "qRichlistTieA"))
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply h2: %v", err)
	}

	for i := 0; i < 3; i++ {
		got, err := q.RichListOverview(ctx)
		if err != nil {
			t.Fatalf("RichListOverview call %d: %v", i, err)
		}
		if len(got.Entries) != 2 {
			t.Fatalf("call %d: Entries = %+v, want 2", i, got.Entries)
		}
		if got.Entries[0].Address != "qRichlistTieA" || got.Entries[0].Rank != 1 {
			t.Errorf("call %d: Entries[0] = %+v, want address=qRichlistTieA rank=1", i, got.Entries[0])
		}
		if got.Entries[1].Address != "qRichlistTieB" || got.Entries[1].Rank != 2 {
			t.Errorf("call %d: Entries[1] = %+v, want address=qRichlistTieB rank=2", i, got.Entries[1])
		}
	}
}

// TestRichListOverview_MultipleUTXOsSameAddress is spec item 22: one
// address receiving several currently-unspent outputs across different
// blocks must appear exactly once, with the already-cached aggregate
// balance — never one row per UTXO.
func TestRichListOverview_MultipleUTXOsSameAddress(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-multi-g", 0, "", coinbaseTx("richlist-multi-g", 100*qoge, "qRichlistMultiG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("richlist-multi-h1", 1, g.Hash, coinbaseTx("richlist-multi-h1", 30*qoge, "qRichlistMultiA"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}
	h2 := block("richlist-multi-h2", 2, h1.Hash, coinbaseTx("richlist-multi-h2", 20*qoge, "qRichlistMultiA"))
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply h2: %v", err)
	}

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview: %v", err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("Entries = %+v, want exactly 1 (one row per address, not per UTXO)", got.Entries)
	}
	if want := 50 * qoge; got.Entries[0].BalanceSatoshis != want {
		t.Errorf("Entries[0].BalanceSatoshis = %d, want %d (30+20)", got.Entries[0].BalanceSatoshis, want)
	}
}

// TestRichListOverview_SpendChangesRanking is spec item 23: build balances
// where A initially ranks above B, spend part of A's value to B so B
// becomes richer, and require the ranking switches correctly — proving
// the rich list trusts the same live addresses cache the address page
// uses, never a stale/independently-computed total.
func TestRichListOverview_SpendChangesRanking(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-spend-g", 0, "", coinbaseTx("richlist-spend-g", 100*qoge, "qRichlistSpendG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("richlist-spend-h1", 1, g.Hash, coinbaseTx("richlist-spend-h1", 80*qoge, "qRichlistSpendA"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}
	h2 := block("richlist-spend-h2", 2, h1.Hash, coinbaseTx("richlist-spend-h2", 40*qoge, "qRichlistSpendA"))
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply h2: %v", err)
	}
	h3 := block("richlist-spend-h3", 3, h2.Hash, coinbaseTx("richlist-spend-h3", 50*qoge, "qRichlistSpendB"))
	if err := st.ApplyBlock(ctx, h3); err != nil {
		t.Fatalf("apply h3: %v", err)
	}

	before, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview before spend: %v", err)
	}
	if len(before.Entries) != 2 || before.Entries[0].Address != "qRichlistSpendA" {
		t.Fatalf("before spend Entries = %+v, want A ranked first (120 > 50)", before.Entries)
	}

	// Spend h2's 40-QOGE output entirely to B: A retains h1's 80-QOGE
	// output (still positive, not dropped), B gains 40 (50+40=90 > 80).
	h2Txid := h2.Transactions[0].TxID
	spend := spendTx("richlist-spend-move", h2Txid, 0, 40*qoge, "qRichlistSpendB")
	h4 := block("richlist-spend-h4", 4, h3.Hash, coinbaseTx("richlist-spend-h4", 100*qoge, "qRichlistSpendMiner"), spend)
	if err := st.ApplyBlock(ctx, h4); err != nil {
		t.Fatalf("apply h4: %v", err)
	}

	after, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview after spend: %v", err)
	}
	var aBal, bBal int64 = -1, -1
	var aRank, bRank int
	for _, e := range after.Entries {
		switch e.Address {
		case "qRichlistSpendA":
			aBal, aRank = e.BalanceSatoshis, e.Rank
		case "qRichlistSpendB":
			bBal, bRank = e.BalanceSatoshis, e.Rank
		}
	}
	if aBal != 80*qoge {
		t.Errorf("after spend A balance = %d, want %d (must still hold h1's unspent output)", aBal, 80*qoge)
	}
	if bBal != 90*qoge {
		t.Errorf("after spend B balance = %d, want %d (50+40)", bBal, 90*qoge)
	}
	if !(bRank < aRank) {
		t.Errorf("after spend ranks: B=%d A=%d, want B ranked ahead of A", bRank, aRank)
	}
}

// TestRichListOverview_ZeroBalanceExcluded is spec item 24: an address
// whose complete balance has been spent away must not remain in the rich
// list merely because its addresses row still exists for history.
func TestRichListOverview_ZeroBalanceExcluded(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-zero-g", 0, "", coinbaseTx("richlist-zero-g", 100*qoge, "qRichlistZeroG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("richlist-zero-h1", 1, g.Hash, coinbaseTx("richlist-zero-h1", 50*qoge, "qRichlistZeroA"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}
	h1Txid := h1.Transactions[0].TxID
	spend := spendTx("richlist-zero-spend", h1Txid, 0, 50*qoge, "qRichlistZeroSink")
	h2 := block("richlist-zero-h2", 2, h1.Hash, coinbaseTx("richlist-zero-h2", 100*qoge, "qRichlistZeroMiner"), spend)
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply h2: %v", err)
	}

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview: %v", err)
	}
	for _, e := range got.Entries {
		if e.Address == "qRichlistZeroA" {
			t.Fatalf("Entries unexpectedly contains fully-spent address: %+v", e)
		}
	}
}

// TestRichListOverview_Reorg is spec item 25: after a real reorg, the
// ranking must reflect ONLY the new canonical branch — no orphan balance
// may leak in.
func TestRichListOverview_Reorg(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-reorg-g", 0, "", coinbaseTx("richlist-reorg-g", 100*qoge, "qRichlistReorgG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("richlist-reorg-a1", 1, g.Hash, coinbaseTx("richlist-reorg-a1", 100*qoge, "qRichlistReorgA"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}

	before, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview before reorg: %v", err)
	}
	if len(before.Entries) != 1 || before.Entries[0].Address != "qRichlistReorgA" {
		t.Fatalf("before reorg Entries = %+v, want [qRichlistReorgA]", before.Entries)
	}

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("richlist-reorg-b1", 1, g.Hash, coinbaseTx("richlist-reorg-b1", 100*qoge, "qRichlistReorgB"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	after, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview after reorg: %v", err)
	}
	if len(after.Entries) != 1 || after.Entries[0].Address != "qRichlistReorgB" {
		t.Fatalf("after reorg Entries = %+v, want [qRichlistReorgB] (A must not leak in)", after.Entries)
	}
}

// TestRichListOverview_NoAddressUTXOExcluded is spec item 28: a spendable
// UTXO with no output_addresses destination contributes to canonical
// UTXO-set value but must never create — or be invented as — a rich-list
// entry.
func TestRichListOverview_NoAddressUTXOExcluded(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-noaddr-g", 0, "", coinbaseTx("richlist-noaddr-g", 100*qoge, "qRichlistNoAddrG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("richlist-noaddr-h1", 1, g.Hash, coinbaseTxMulti("richlist-noaddr-h1", []chain.Output{
		unknownNoAddressOutput(0, 100*qoge),
	}))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview: %v", err)
	}
	if len(got.Entries) != 0 {
		t.Fatalf("Entries = %+v, want empty (no-address output must not produce an entry)", got.Entries)
	}
}

// TestRichListOverview_BareMultisigExcluded is spec item 29: bare-multisig
// participant identities (output_participants) must never receive credit
// for the multisig output's value in the rich list — not divided among
// participants, not credited in full to any one of them.
func TestRichListOverview_BareMultisigExcluded(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	g := block("richlist-multisig-g", 0, "", coinbaseTx("richlist-multisig-g", 100*qoge, "qRichlistMultisigG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	pub1 := []byte{0x02, 0x01}
	pub2 := []byte{0x02, 0x02}
	h1 := block("richlist-multisig-h1", 1, g.Hash, coinbaseTxMulti("richlist-multisig-h1", []chain.Output{
		multisigOutput(0, 100*qoge, [][]byte{pub1, pub2}, []string{"qRichlistMultisigP1", "qRichlistMultisigP2"}),
	}))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}
	h1Txid := h1.Transactions[0].TxID

	var participantCount int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM output_participants WHERE txid = $1", h1Txid).Scan(&participantCount); err != nil {
		t.Fatalf("count output_participants: %v", err)
	}
	if participantCount != 2 {
		t.Fatalf("output_participants count = %d, want 2", participantCount)
	}

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview: %v", err)
	}
	for _, e := range got.Entries {
		if e.Address == "qRichlistMultisigP1" || e.Address == "qRichlistMultisigP2" {
			t.Fatalf("Entries unexpectedly credits a multisig participant: %+v", e)
		}
	}
}

// TestRichListOverview_P2QPKParticipatesNormally is spec item 30: a P2QPK
// destination must rank exactly like any other balance-accounting
// address — no special-cased script semantics in this phase.
func TestRichListOverview_P2QPKParticipatesNormally(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-p2qpk-g", 0, "", coinbaseTx("richlist-p2qpk-g", 100*qoge, "qRichlistP2QPKG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("richlist-p2qpk-h1", 1, g.Hash, coinbaseTxMulti("richlist-p2qpk-h1", []chain.Output{
		p2qpkOutput(0, 42*qoge, "qRichlistP2QPKAddr"),
	}))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Address != "qRichlistP2QPKAddr" || got.Entries[0].BalanceSatoshis != 42*qoge {
		t.Fatalf("Entries = %+v, want exactly [qRichlistP2QPKAddr = %d]", got.Entries, 42*qoge)
	}
}

// TestRichListOverview_RePromotionRestoresRanking is the Phase 2H.3 internal
// review correction: the fourth reorg-adjacent gate (new output / spend /
// reorg / RE-PROMOTION) that spec item 41 required but the original test
// matrix omitted. A1 is applied, rolled back (orphaned but still persisted
// by internal/store), superseded by an unrelated B1, B1 is then also rolled
// back, and the EXACT SAME previously-persisted A1 block (same label, same
// deterministic hash — see fixtures_test.go's block/fakeHash) is re-applied
// via the real Store.ApplyBlock/RollbackTo paths. This must exercise
// Store's existing orphan re-promotion path, not a freshly constructed
// A1-like block with a different hash. The rich list must end up showing
// exactly A's restored balance/ranking and nothing of B's.
func TestRichListOverview_RePromotionRestoresRanking(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-repromote-g", 0, "", coinbaseTx("richlist-repromote-g", 100*qoge, "qRichlistRepromoteG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	a1 := block("richlist-repromote-a1", 1, g.Hash, coinbaseTx("richlist-repromote-a1", 70*qoge, "qRichlistRepromoteA"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}

	afterA, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview after A1: %v", err)
	}
	if len(afterA.Entries) != 1 || afterA.Entries[0].Address != "qRichlistRepromoteA" || afterA.Entries[0].Rank != 1 || afterA.Entries[0].BalanceSatoshis != 70*qoge {
		t.Fatalf("after A1 Entries = %+v, want [rank=1 qRichlistRepromoteA balance=%d]", afterA.Entries, 70*qoge)
	}

	// Orphan A1 (still persisted by internal/store, just not canonical).
	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis (orphan A1): %v", err)
	}

	b1 := block("richlist-repromote-b1", 1, g.Hash, coinbaseTx("richlist-repromote-b1", 50*qoge, "qRichlistRepromoteB"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	afterB, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview after B1: %v", err)
	}
	if len(afterB.Entries) != 1 || afterB.Entries[0].Address != "qRichlistRepromoteB" || afterB.Entries[0].BalanceSatoshis != 50*qoge {
		t.Fatalf("after B1 Entries = %+v, want ONLY [qRichlistRepromoteB balance=%d] (A must not leak)", afterB.Entries, 50*qoge)
	}

	// Orphan B1 too, then re-apply the EXACT SAME previously-persisted A1
	// block (same label -> same deterministic hash), exercising Store's
	// orphan re-promotion path rather than constructing a new block.
	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis (orphan B1): %v", err)
	}
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("re-apply persisted A1 (orphan re-promotion): %v", err)
	}

	final, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview after A1 re-promotion: %v", err)
	}
	if final.IndexedHash == nil || *final.IndexedHash != a1.Hash {
		t.Fatalf("IndexedHash = %v, want restored to A1's hash %s", final.IndexedHash, a1.Hash)
	}
	if len(final.Entries) != 1 {
		t.Fatalf("final Entries = %+v, want exactly 1 (no duplicate A entry, no orphan B leakage)", final.Entries)
	}
	if final.Entries[0].Address != "qRichlistRepromoteA" || final.Entries[0].Rank != 1 || final.Entries[0].BalanceSatoshis != 70*qoge {
		t.Fatalf("final Entries[0] = %+v, want rank=1 address=qRichlistRepromoteA balance=%d", final.Entries[0], 70*qoge)
	}
	for _, e := range final.Entries {
		if e.Address == "qRichlistRepromoteB" {
			t.Fatalf("final Entries unexpectedly still contains orphaned B: %+v", final.Entries)
		}
	}
}

// TestRichListOverview_Top100Boundary is spec item 51: with more than 100
// positive-balance addresses, exactly 100 must be returned — the 101st
// (lowest-ranked) address must be absent.
func TestRichListOverview_Top100Boundary(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("richlist-top100-g", 0, "", coinbaseTx("richlist-top100-g", 100*qoge, "qRichlistTop100G"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	const n = 101
	prev := g.Hash
	for i := 0; i < n; i++ {
		label := "richlist-top100-h" + strconv.Itoa(i)
		// Descending balances: address 0 gets the highest balance, address
		// 100 (the 101st, lowest) gets the smallest — must fall just off
		// the bottom of the top 100. Kept in raw satoshis (well under one
		// block's 100-QOGE subsidy cap) purely to keep each balance
		// distinct; the QOGE/satoshi scale is irrelevant to this test.
		balance := int64(n - i)
		b := block(label, int64(i+1), prev, coinbaseTx(label, balance, "qRichlistTop100Addr"+strconv.Itoa(i)))
		if err := st.ApplyBlock(ctx, b); err != nil {
			t.Fatalf("apply block %d: %v", i, err)
		}
		prev = b.Hash
	}

	got, err := q.RichListOverview(ctx)
	if err != nil {
		t.Fatalf("RichListOverview: %v", err)
	}
	if len(got.Entries) != RichListLimit {
		t.Fatalf("len(Entries) = %d, want exactly %d", len(got.Entries), RichListLimit)
	}
	for _, e := range got.Entries {
		if e.Address == "qRichlistTop100Addr100" {
			t.Fatalf("Entries unexpectedly contains the 101st (lowest-balance) address: %+v", e)
		}
	}
	if got.Entries[0].Address != "qRichlistTop100Addr0" {
		t.Errorf("Entries[0].Address = %s, want qRichlistTop100Addr0 (highest balance)", got.Entries[0].Address)
	}
}
