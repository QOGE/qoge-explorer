package query

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
)

const qoge = int64(chain.SatoshisPerQOGE)

// coinbaseTxMulti builds a coinbase transaction with an arbitrary output
// set — coinbaseTx (fixtures_test.go) only supports exactly one output,
// which isn't enough to build an OP_RETURN-alongside-a-spendable-output or
// a no-address fixture.
func coinbaseTxMulti(label string, outs []chain.Output) chain.Transaction {
	txid := fakeHash(label + "-tx")
	return chain.Transaction{
		TxID:       txid,
		WTxID:      txid,
		Version:    1,
		LockTime:   0,
		Size:       100,
		VSize:      100,
		Weight:     400,
		IsCoinbase: true,
		Inputs: []chain.Input{
			{Index: 0, Coinbase: []byte{0x51}, Sequence: 0xffffffff},
		},
		Outputs: outs,
	}
}

func p2pkhOutput(index uint32, valueSats int64, label, addr string) chain.Output {
	return chain.Output{
		Index:        index,
		Value:        chain.Amount(valueSats),
		ScriptPubKey: p2pkhScript(label),
		ScriptType:   script.TypeP2PKH,
		Address:      addr,
	}
}

// nulldataOutput builds a structurally valid OP_RETURN output — Core-
// unspendable, so internal/store's applyOutput never adds it to
// utxo_state even though it carries value and is faithfully preserved in
// transaction_outputs (docs/ARCHITECTURE.md §2 item 3, §26).
func nulldataOutput(index uint32, valueSats int64) chain.Output {
	return chain.Output{
		Index:        index,
		Value:        chain.Amount(valueSats),
		ScriptPubKey: []byte{0x6a},
		ScriptType:   script.TypeNullData,
	}
}

// unknownNoAddressOutput builds a structurally valid, spendable-in-
// principle output whose script this codebase's classifier does not
// recognize (script_type='unknown') and which therefore has no owning
// address at all (Address left unset) — proving SupplyOverview's UTXO-set
// value comes from utxo_state/transaction_outputs directly, never from
// output_addresses/addresses (spec item 25/47).
func unknownNoAddressOutput(index uint32, valueSats int64) chain.Output {
	return chain.Output{
		Index:        index,
		Value:        chain.Amount(valueSats),
		ScriptPubKey: []byte{0x51, 0x51}, // arbitrary, non-witness, unrecognized shape
		ScriptType:   script.TypeUnknown,
	}
}

// TestSupplyOverview_EmptyDatabase is spec item 41: a freshly migrated
// database with no blocks at all is a valid HTTP 200 (query-layer: a valid
// zero-value SupplyOverview, no error) — never confused with "chain supply
// of zero".
func TestSupplyOverview_EmptyDatabase(t *testing.T) {
	ctx := context.Background()
	q, _, _ := newTestQueryStore(t)

	got, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview on empty database: %v", err)
	}
	if got.IndexedHeight != -1 {
		t.Errorf("IndexedHeight = %d, want -1", got.IndexedHeight)
	}
	if got.IndexedHash != nil {
		t.Errorf("IndexedHash = %v, want nil", got.IndexedHash)
	}
	for name, got := range map[string]int64{
		"ScheduledSubsidySatoshis": got.ScheduledSubsidySatoshis,
		"TransactionFeesSatoshis":  got.TransactionFeesSatoshis,
		"CoinbaseOutputsSatoshis":  got.CoinbaseOutputsSatoshis,
		"UnclaimedRewardSatoshis":  got.UnclaimedRewardSatoshis,
		"UTXOSetValueSatoshis":     got.UTXOSetValueSatoshis,
	} {
		if got != 0 {
			t.Errorf("%s = %d, want 0", name, got)
		}
	}
	if got.ScheduledSubsidyQOGE != "0.00000000" {
		t.Errorf("ScheduledSubsidyQOGE = %q, want 0.00000000", got.ScheduledSubsidyQOGE)
	}
}

// TestSupplyOverview_SingleBlock is spec item 42 (plus item 54's exact
// sats/QOGE format check): one genesis block, fully claimed (coinbase ==
// subsidy, no fees) — the simplest non-empty case. Genesis is never placed
// into utxo_state (internal/store's applyOutput), so UTXOSetValueSatoshis
// is legitimately 0 here; the exclusion is exercised with a non-zero
// result by TestSupplyOverview_GenesisExcludedFromUTXOSet below.
func TestSupplyOverview_SingleBlock(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("supply-single-genesis", 0, "", coinbaseTx("supply-single-genesis", 100*qoge, "qSupplySingleGenesis"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	got, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview: %v", err)
	}
	if got.IndexedHeight != 0 || got.IndexedHash == nil || *got.IndexedHash != g.Hash {
		t.Fatalf("indexed anchor = (%d, %v), want (0, %s)", got.IndexedHeight, got.IndexedHash, g.Hash)
	}
	if got.ScheduledSubsidySatoshis != 100*qoge {
		t.Errorf("ScheduledSubsidySatoshis = %d, want %d", got.ScheduledSubsidySatoshis, 100*qoge)
	}
	if got.ScheduledSubsidyQOGE != "100.00000000" {
		t.Errorf("ScheduledSubsidyQOGE = %q, want 100.00000000", got.ScheduledSubsidyQOGE)
	}
	if got.TransactionFeesSatoshis != 0 {
		t.Errorf("TransactionFeesSatoshis = %d, want 0", got.TransactionFeesSatoshis)
	}
	if got.CoinbaseOutputsSatoshis != 100*qoge {
		t.Errorf("CoinbaseOutputsSatoshis = %d, want %d", got.CoinbaseOutputsSatoshis, 100*qoge)
	}
	if got.UnclaimedRewardSatoshis != 0 {
		t.Errorf("UnclaimedRewardSatoshis = %d, want 0 (full claim)", got.UnclaimedRewardSatoshis)
	}
	if got.UTXOSetValueSatoshis != 0 {
		t.Errorf("UTXOSetValueSatoshis = %d, want 0 (genesis output excluded)", got.UTXOSetValueSatoshis)
	}
	if got.UTXOSetValueQOGE != "0.00000000" {
		t.Errorf("UTXOSetValueQOGE = %q, want 0.00000000", got.UTXOSetValueQOGE)
	}
}

// TestSupplyOverview_GenesisExcludedFromUTXOSet is spec item 45: genesis
// contributes to scheduled subsidy AND coinbase outputs, but its output is
// never spendable — a critical semantic regression to keep pinned down.
func TestSupplyOverview_GenesisExcludedFromUTXOSet(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("supply-genesis-excl", 0, "", coinbaseTx("supply-genesis-excl", 100*qoge, "qSupplyGenesisExclG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("supply-genesis-excl-h1", 1, g.Hash, coinbaseTx("supply-genesis-excl-h1", 100*qoge, "qSupplyGenesisExclH1"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}

	got, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview: %v", err)
	}
	if want := 200 * qoge; got.ScheduledSubsidySatoshis != want {
		t.Errorf("ScheduledSubsidySatoshis = %d, want %d (genesis included)", got.ScheduledSubsidySatoshis, want)
	}
	if want := 200 * qoge; got.CoinbaseOutputsSatoshis != want {
		t.Errorf("CoinbaseOutputsSatoshis = %d, want %d (genesis included)", got.CoinbaseOutputsSatoshis, want)
	}
	if want := 100 * qoge; got.UTXOSetValueSatoshis != want {
		t.Errorf("UTXOSetValueSatoshis = %d, want %d (ONLY height 1's output — genesis excluded)", got.UTXOSetValueSatoshis, want)
	}
}

// TestSupplyOverview_FullClaim is spec item 44: coinbase == subsidy + fees
// implies unclaimed_reward == 0. Built with a real fee (via a spend tx),
// not just the trivial fee=0 case TestSupplyOverview_SingleBlock already
// covers.
func TestSupplyOverview_FullClaim(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("supply-fullclaim-g", 0, "", coinbaseTx("supply-fullclaim-g", 100*qoge, "qSupplyFullClaimG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("supply-fullclaim-h1", 1, g.Hash, coinbaseTx("supply-fullclaim-h1", 100*qoge, "qSupplyFullClaimH1"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}
	h1Txid := h1.Transactions[0].TxID

	spend := spendTx("supply-fullclaim-spend", h1Txid, 0, 95*qoge, "qSupplyFullClaimSpend") // fee = 5 QOGE
	h2 := block("supply-fullclaim-h2", 2, h1.Hash,
		coinbaseTx("supply-fullclaim-h2", 105*qoge, "qSupplyFullClaimH2"), // subsidy(100) + fee(5), full claim
		spend,
	)
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply height 2: %v", err)
	}

	got, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview: %v", err)
	}
	if want := 5 * qoge; got.TransactionFeesSatoshis != want {
		t.Errorf("TransactionFeesSatoshis = %d, want %d", got.TransactionFeesSatoshis, want)
	}
	if got.UnclaimedRewardSatoshis != 0 {
		t.Errorf("UnclaimedRewardSatoshis = %d, want 0 (full claim: coinbase == subsidy + fees)", got.UnclaimedRewardSatoshis)
	}
}

// TestSupplyOverview_Underclaim is spec item 43: a positive fee with a
// coinbase payout strictly less than subsidy+fee must report a positive,
// accurate unclaimed_reward — never described anywhere as burn.
func TestSupplyOverview_Underclaim(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("supply-underclaim-g", 0, "", coinbaseTx("supply-underclaim-g", 100*qoge, "qSupplyUnderclaimG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("supply-underclaim-h1", 1, g.Hash, coinbaseTx("supply-underclaim-h1", 100*qoge, "qSupplyUnderclaimH1"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}
	h1Txid := h1.Transactions[0].TxID

	// fee = 1 QOGE; max available reward at height 2 = subsidy(100) + fee(1)
	// = 101 QOGE; coinbase claims only 90 QOGE => unclaimed = 11 QOGE.
	spend := spendTx("supply-underclaim-spend", h1Txid, 0, 99*qoge, "qSupplyUnderclaimSpend")
	h2 := block("supply-underclaim-h2", 2, h1.Hash,
		coinbaseTx("supply-underclaim-h2", 90*qoge, "qSupplyUnderclaimH2"),
		spend,
	)
	if err := st.ApplyBlock(ctx, h2); err != nil {
		t.Fatalf("apply height 2: %v", err)
	}

	got, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview: %v", err)
	}
	if want := 1 * qoge; got.TransactionFeesSatoshis != want {
		t.Errorf("TransactionFeesSatoshis = %d, want %d", got.TransactionFeesSatoshis, want)
	}
	if want := 11 * qoge; got.UnclaimedRewardSatoshis != want {
		t.Errorf("UnclaimedRewardSatoshis = %d, want %d", got.UnclaimedRewardSatoshis, want)
	}
	// Aggregate identity (spec item 53/22), checked directly against the
	// response: coinbase_outputs + unclaimed_reward == scheduled_subsidy +
	// transaction_fees.
	if lhs, rhs := got.CoinbaseOutputsSatoshis+got.UnclaimedRewardSatoshis, got.ScheduledSubsidySatoshis+got.TransactionFeesSatoshis; lhs != rhs {
		t.Errorf("aggregate identity violated: coinbase+unclaimed=%d, subsidy+fees=%d", lhs, rhs)
	}
}

// TestSupplyOverview_UnspendableOutputExcluded is spec item 46: a
// Core-unspendable (OP_RETURN) output contributes to coinbase_output_total
// (it's part of what the coinbase transaction actually paid) but never to
// UTXO-set value.
func TestSupplyOverview_UnspendableOutputExcluded(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("supply-unspendable-g", 0, "", coinbaseTx("supply-unspendable-g", 100*qoge, "qSupplyUnspendableG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("supply-unspendable-h1", 1, g.Hash, coinbaseTxMulti("supply-unspendable-h1", []chain.Output{
		p2pkhOutput(0, 99*qoge, "supply-unspendable-h1-normal", "qSupplyUnspendableH1"),
		nulldataOutput(1, 1*qoge),
	}))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}

	got, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview: %v", err)
	}
	if want := 200 * qoge; got.CoinbaseOutputsSatoshis != want {
		t.Errorf("CoinbaseOutputsSatoshis = %d, want %d (includes the OP_RETURN output's value)", got.CoinbaseOutputsSatoshis, want)
	}
	if want := 99 * qoge; got.UTXOSetValueSatoshis != want {
		t.Errorf("UTXOSetValueSatoshis = %d, want %d (OP_RETURN output must be excluded)", got.UTXOSetValueSatoshis, want)
	}
}

// TestSupplyOverview_NonAddressUTXOIncluded is spec item 47/25: an eligible
// output with no output_addresses row (an unrecognized script here — bare
// multisig is the production example, but the point under test is
// identical: no owning address) must still contribute to UTXO-set value —
// proving the query is based on utxo_state, never the addresses cache.
func TestSupplyOverview_NonAddressUTXOIncluded(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	g := block("supply-noaddr-g", 0, "", coinbaseTx("supply-noaddr-g", 100*qoge, "qSupplyNoAddrG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("supply-noaddr-h1", 1, g.Hash, coinbaseTxMulti("supply-noaddr-h1", []chain.Output{
		unknownNoAddressOutput(0, 100*qoge),
	}))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}
	h1Txid := h1.Transactions[0].TxID

	// genesis's OWN coinbase output legitimately has an output_addresses
	// row (applyOutput's address-destination bookkeeping doesn't depend on
	// isGenesis — only UTXO-set eligibility does); what this test actually
	// needs to confirm is that height 1's no-address fixture output
	// specifically has none.
	var addressRows int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM output_addresses WHERE txid = $1", h1Txid).Scan(&addressRows); err != nil {
		t.Fatalf("count output_addresses for h1: %v", err)
	}
	if addressRows != 0 {
		t.Fatalf("output_addresses has %d rows for h1's txid, want 0 (fixture output must have no owning address)", addressRows)
	}

	got, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview: %v", err)
	}
	if want := 100 * qoge; got.UTXOSetValueSatoshis != want {
		t.Errorf("UTXOSetValueSatoshis = %d, want %d (must include the no-address output)", got.UTXOSetValueSatoshis, want)
	}
}

// TestSupplyOverview_OrphanExcludedAndReorgSwitchesTotals is spec items
// 48/49/21: after a real reorg, canonical totals reflect ONLY the new
// branch; the old branch's immutable accounting remains stored but
// contributes zero.
func TestSupplyOverview_OrphanExcludedAndReorgSwitchesTotals(t *testing.T) {
	ctx := context.Background()
	q, st, _ := newTestQueryStore(t)

	g := block("supply-reorg-g", 0, "", coinbaseTx("supply-reorg-g", 100*qoge, "qSupplyReorgG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("supply-reorg-a1", 1, g.Hash, coinbaseTx("supply-reorg-a1", 40*qoge, "qSupplyReorgA1")) // underclaim: unclaimed=60
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}

	before, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview before reorg: %v", err)
	}
	if want := 140 * qoge; before.CoinbaseOutputsSatoshis != want {
		t.Fatalf("before reorg CoinbaseOutputsSatoshis = %d, want %d", before.CoinbaseOutputsSatoshis, want)
	}

	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("supply-reorg-b1", 1, g.Hash, coinbaseTx("supply-reorg-b1", 100*qoge, "qSupplyReorgB1")) // full claim
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	after, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview after reorg: %v", err)
	}
	if want := 200 * qoge; after.CoinbaseOutputsSatoshis != want {
		t.Errorf("after reorg CoinbaseOutputsSatoshis = %d, want %d (must reflect ONLY genesis+B1, never orphaned A1)", after.CoinbaseOutputsSatoshis, want)
	}
	if after.UnclaimedRewardSatoshis != 0 {
		t.Errorf("after reorg UnclaimedRewardSatoshis = %d, want 0 (B1 is a full claim; A1's underclaim must not leak in)", after.UnclaimedRewardSatoshis)
	}
	if after.IndexedHash == nil || *after.IndexedHash != b1.Hash {
		t.Errorf("after reorg IndexedHash = %v, want %s", after.IndexedHash, b1.Hash)
	}
}

// TestSupplyOverview_IncompleteAccountingRejected is spec item 51: a
// canonical block with no block_accounting row (simulating a pre-2H.1
// database that hasn't run backfill-accounting yet) must produce
// ErrAccountingIncomplete, never an understated total.
func TestSupplyOverview_IncompleteAccountingRejected(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	g := block("supply-incomplete-g", 0, "", coinbaseTx("supply-incomplete-g", 100*qoge, "qSupplyIncompleteG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("supply-incomplete-h1", 1, g.Hash, coinbaseTx("supply-incomplete-h1", 100*qoge, "qSupplyIncompleteH1"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}

	// Simulate a legacy, pre-backfill database: strip height 1's
	// accounting row via test-only raw SQL (never via production code).
	if _, err := pool.Exec(ctx, "DELETE FROM block_accounting WHERE block_hash = $1", h1.Hash); err != nil {
		t.Fatalf("delete block_accounting for h1: %v", err)
	}

	_, err := q.SupplyOverview(ctx)
	if !errors.Is(err, ErrAccountingIncomplete) {
		t.Fatalf("SupplyOverview error = %v, want ErrAccountingIncomplete", err)
	}
}

// TestSupplyOverview_OrphanAccountingMissingDoesNotBlockCanonical is spec
// item 52: a missing block_accounting row for an ORPHANED block must NOT
// make the current canonical supply unavailable — only canonical coverage
// matters.
func TestSupplyOverview_OrphanAccountingMissingDoesNotBlockCanonical(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	g := block("supply-orphanacct-g", 0, "", coinbaseTx("supply-orphanacct-g", 100*qoge, "qSupplyOrphanAcctG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	a1 := block("supply-orphanacct-a1", 1, g.Hash, coinbaseTx("supply-orphanacct-a1", 100*qoge, "qSupplyOrphanAcctA1"))
	if err := st.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("apply A1: %v", err)
	}
	if err := st.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("rollback to genesis: %v", err)
	}
	b1 := block("supply-orphanacct-b1", 1, g.Hash, coinbaseTx("supply-orphanacct-b1", 100*qoge, "qSupplyOrphanAcctB1"))
	if err := st.ApplyBlock(ctx, b1); err != nil {
		t.Fatalf("apply B1: %v", err)
	}

	// A1 is now orphaned but its accounting row remains (RollbackTo never
	// deletes it — docs/ARCHITECTURE.md §26). Delete it anyway to prove the
	// completeness check is genuinely canonical-only, not "every row that
	// happens to reference blocks".
	if _, err := pool.Exec(ctx, "DELETE FROM block_accounting WHERE block_hash = $1", a1.Hash); err != nil {
		t.Fatalf("delete orphan A1 accounting: %v", err)
	}

	got, err := q.SupplyOverview(ctx)
	if err != nil {
		t.Fatalf("SupplyOverview: %v (must succeed — the missing row belongs to an orphaned block)", err)
	}
	if want := 200 * qoge; got.CoinbaseOutputsSatoshis != want {
		t.Errorf("CoinbaseOutputsSatoshis = %d, want %d", got.CoinbaseOutputsSatoshis, want)
	}
}

// TestSupplyOverview_AggregateOverflowFailsLoudly is spec item 23: corrupt/
// extreme accounting data that would overflow int64 on summation must
// cause SupplyOverview to fail loudly, never silently wrap into a bogus
// (possibly negative-looking) total. Two canonical blocks are pushed to
// carry an artificially huge, but individually
// block_accounting_reward_identity-satisfying, subsidy/coinbase figure via
// test-only raw SQL — something internal/accounting's own checked
// arithmetic would never let real indexing produce, but which
// SupplyOverview must still detect defensively (docs/ARCHITECTURE.md §26
// "pure, checked-arithmetic functions").
func TestSupplyOverview_AggregateOverflowFailsLoudly(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)

	g := block("supply-overflow-g", 0, "", coinbaseTx("supply-overflow-g", 100*qoge, "qSupplyOverflowG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("supply-overflow-h1", 1, g.Hash, coinbaseTx("supply-overflow-h1", 100*qoge, "qSupplyOverflowH1"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply height 1: %v", err)
	}

	// math.MaxInt64 ~= 9.223e18. Two blocks at 6e18 each sum to 1.2e19,
	// which overflows int64 but individually still satisfies
	// coinbase + unclaimed == subsidy + fees (0 + 0 fees/unclaimed here).
	huge := int64(6_000_000_000_000_000_000)
	if huge <= math.MaxInt64/2 {
		t.Fatalf("test constant %d is not actually > MaxInt64/2 as intended", huge)
	}
	for _, hash := range []string{g.Hash, h1.Hash} {
		if _, err := pool.Exec(ctx, `
			UPDATE block_accounting
			SET subsidy_satoshis = $2, fee_satoshis = 0, coinbase_output_satoshis = $2, unclaimed_reward_satoshis = 0
			WHERE block_hash = $1
		`, hash, huge); err != nil {
			t.Fatalf("inflate block_accounting for %s: %v", hash, err)
		}
	}

	_, err := q.SupplyOverview(ctx)
	if err == nil {
		t.Fatal("SupplyOverview succeeded on an overflowing aggregate, want a loud error")
	}
}
