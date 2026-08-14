package store

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// ─── 39: genesis ──────────────────────────────────────────────────────────

func TestSupplyRollup_Genesis(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("rollG0"), 0, "", coinbaseTx(hash64("rollG0tx"), out(0, era0Subsidy, "qGenesis")))
	mustApply(t, ctx, s, g)

	r, err := s.GetBlockSupplyRollup(ctx, g.Hash)
	if err != nil {
		t.Fatalf("GetBlockSupplyRollup: %v", err)
	}
	if r == nil {
		t.Fatal("expected a rollup row for genesis")
	}
	if r.ExcludedOutputSatoshis != era0Subsidy {
		t.Errorf("ExcludedOutputSatoshis = %d, want %d", r.ExcludedOutputSatoshis, era0Subsidy)
	}
	if r.CumulativeSubsidySatoshis != era0Subsidy {
		t.Errorf("CumulativeSubsidySatoshis = %d, want %d", r.CumulativeSubsidySatoshis, era0Subsidy)
	}
	if r.CumulativeCoinbaseOutputSatoshis != era0Subsidy {
		t.Errorf("CumulativeCoinbaseOutputSatoshis = %d, want %d", r.CumulativeCoinbaseOutputSatoshis, era0Subsidy)
	}
	if r.CumulativeFeeSatoshis != 0 {
		t.Errorf("CumulativeFeeSatoshis = %d, want 0", r.CumulativeFeeSatoshis)
	}
	if r.CumulativeUnclaimedRewardSatoshis != 0 {
		t.Errorf("CumulativeUnclaimedRewardSatoshis = %d, want 0", r.CumulativeUnclaimedRewardSatoshis)
	}
	if r.CumulativeExcludedOutputSatoshis != era0Subsidy {
		t.Errorf("CumulativeExcludedOutputSatoshis = %d, want %d", r.CumulativeExcludedOutputSatoshis, era0Subsidy)
	}
	if r.CumulativeUTXOSetValueSatoshis != 0 {
		t.Errorf("CumulativeUTXOSetValueSatoshis = %d, want 0 (genesis output never enters Core's UTXO set)", r.CumulativeUTXOSetValueSatoshis)
	}
}

// ─── 40: normal coinbase ──────────────────────────────────────────────────

func TestSupplyRollup_NormalCoinbase(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("rollN0"), 0, "", coinbaseTx(hash64("rollN0tx"), out(0, era0Subsidy, "qGenesis")))
	mustApply(t, ctx, s, g)

	cb := coinbaseTx(hash64("rollN1tx"), out(0, era0Subsidy, "qMiner"))
	block1 := testBlock(hash64("rollN1"), 1, g.Hash, cb)
	mustApply(t, ctx, s, block1)

	r, err := s.GetBlockSupplyRollup(ctx, block1.Hash)
	if err != nil || r == nil {
		t.Fatalf("GetBlockSupplyRollup: r=%+v err=%v", r, err)
	}
	if r.CumulativeUTXOSetValueSatoshis != era0Subsidy {
		t.Errorf("tip cumulative UTXO = %d, want %d (100 QOGE — only h1's full-claim coinbase, genesis excluded)",
			r.CumulativeUTXOSetValueSatoshis, era0Subsidy)
	}
}

// ─── 41: fee ────────────────────────────────────────────────────────────

func TestSupplyRollup_Fee(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	// Genesis's own coinbase output can never be spent later (it never
	// enters Core's UTXO set at all — see applyOutput's isGenesis
	// exclusion), so the spendable output this test's fee transaction
	// consumes must come from block1's coinbase, not genesis's.
	g := testBlock(hash64("rollF0"), 0, "", coinbaseTx(hash64("rollF0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	block1 := testBlock(hash64("rollF1"), 1, g.Hash, coinbaseTx(hash64("rollF1tx"), out(0, era0Subsidy, "qMiner1")))
	mustApply(t, ctx, s, block1)

	const fee = int64(250_000_000)
	spend := spendTx(hash64("rollFSpend"), hash64("rollFSpend"), 200,
		[]chain.Input{spendInput(0, hash64("rollF1tx"), 0, nil)},
		[]chain.Output{out(0, era0Subsidy-fee, "qBob")},
	)
	cb := coinbaseTx(hash64("rollFCb"), out(0, era0Subsidy+fee, "qMiner2"))
	block2 := testBlock(hash64("rollF2"), 2, block1.Hash, cb, spend)
	mustApply(t, ctx, s, block2)

	r, err := s.GetBlockSupplyRollup(ctx, block2.Hash)
	if err != nil || r == nil {
		t.Fatalf("GetBlockSupplyRollup: r=%+v err=%v", r, err)
	}
	if r.CumulativeFeeSatoshis != fee {
		t.Errorf("CumulativeFeeSatoshis = %d, want %d", r.CumulativeFeeSatoshis, fee)
	}
	wantUTXO := r.CumulativeCoinbaseOutputSatoshis - r.CumulativeFeeSatoshis - r.CumulativeExcludedOutputSatoshis
	if r.CumulativeUTXOSetValueSatoshis != wantUTXO {
		t.Errorf("UTXO identity violated: got %d, want coinbase(%d)-fee(%d)-excluded(%d)=%d",
			r.CumulativeUTXOSetValueSatoshis, r.CumulativeCoinbaseOutputSatoshis, r.CumulativeFeeSatoshis, r.CumulativeExcludedOutputSatoshis, wantUTXO)
	}
	// Live UTXO set after block2: block2's spend output (era0Subsidy-fee)
	// plus block2's own coinbase (era0Subsidy+fee) — block1's original
	// coinbase output was consumed by the spend. Genesis remains excluded.
	if want := 2 * era0Subsidy; r.CumulativeUTXOSetValueSatoshis != want {
		t.Errorf("CumulativeUTXOSetValueSatoshis = %d, want %d", r.CumulativeUTXOSetValueSatoshis, want)
	}
}

// ─── 42: underclaim ───────────────────────────────────────────────────────

func TestSupplyRollup_Underclaim(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("rollU0"), 0, "", coinbaseTx(hash64("rollU0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	const shortfall = int64(777_000)
	cb := coinbaseTx(hash64("rollU1tx"), out(0, era0Subsidy-shortfall, "qMiner"))
	block1 := testBlock(hash64("rollU1"), 1, g.Hash, cb)
	mustApply(t, ctx, s, block1)

	r, err := s.GetBlockSupplyRollup(ctx, block1.Hash)
	if err != nil || r == nil {
		t.Fatalf("GetBlockSupplyRollup: r=%+v err=%v", r, err)
	}
	if r.CumulativeUnclaimedRewardSatoshis != shortfall {
		t.Errorf("CumulativeUnclaimedRewardSatoshis = %d, want %d", r.CumulativeUnclaimedRewardSatoshis, shortfall)
	}
	wantCoinbase := 2*era0Subsidy - shortfall
	if r.CumulativeCoinbaseOutputSatoshis != wantCoinbase {
		t.Errorf("CumulativeCoinbaseOutputSatoshis = %d, want %d (actual encoded payout, not max reward)", r.CumulativeCoinbaseOutputSatoshis, wantCoinbase)
	}
	// UTXO cumulative uses the ACTUAL coinbase output, not the maximum
	// reward: genesis excluded, block1's actual (underclaimed) coinbase
	// output is the only value in the UTXO set.
	if want := era0Subsidy - shortfall; r.CumulativeUTXOSetValueSatoshis != want {
		t.Errorf("CumulativeUTXOSetValueSatoshis = %d, want %d (actual coinbase output, not scheduled subsidy)", r.CumulativeUTXOSetValueSatoshis, want)
	}
}

// ─── 43: OP_RETURN ─────────────────────────────────────────────────────────

func TestSupplyRollup_OpReturn(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("rollR0"), 0, "", coinbaseTx(hash64("rollR0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	const opReturnValue = int64(0) // OP_RETURN outputs are conventionally zero-value, but the field itself is what's tested
	cb := coinbaseTx(hash64("rollR1tx"), out(0, era0Subsidy, "qMiner"), nullOut(1))
	block1 := testBlock(hash64("rollR1"), 1, g.Hash, cb)
	mustApply(t, ctx, s, block1)

	r, err := s.GetBlockSupplyRollup(ctx, block1.Hash)
	if err != nil || r == nil {
		t.Fatalf("GetBlockSupplyRollup: r=%+v err=%v", r, err)
	}
	if r.ExcludedOutputSatoshis != opReturnValue {
		t.Errorf("ExcludedOutputSatoshis = %d, want %d (nullOut is zero-value in this fixture)", r.ExcludedOutputSatoshis, opReturnValue)
	}
	// Cumulative excluded only grew by genesis's contribution — nullOut's
	// own value is zero in this fixture, but the important assertion is
	// that the OP_RETURN output is excluded from the UTXO set at all: the
	// coinbase output total includes it, yet UTXO cumulative does not.
	if r.CumulativeUTXOSetValueSatoshis != era0Subsidy {
		t.Errorf("CumulativeUTXOSetValueSatoshis = %d, want %d (OP_RETURN output must never enter the UTXO set)", r.CumulativeUTXOSetValueSatoshis, era0Subsidy)
	}

	// A second variant with a genuinely nonzero-value OP_RETURN output
	// proves excluded_output_satoshis actually tracks its value, not just
	// its presence.
	const opReturnPayload = int64(12_345)
	valuableOpReturn := chain.Output{Index: 1, Value: chain.Amount(opReturnPayload), ScriptPubKey: []byte{0x6a, 0x01, 0xff}, ScriptType: script.TypeNullData}
	// Total coinbase output must not exceed subsidy+fees(0) — the ordinary
	// output claims the remainder after the OP_RETURN payload.
	cb2 := coinbaseTx(hash64("rollR2tx"), out(0, era0Subsidy-opReturnPayload, "qMiner2"), valuableOpReturn)
	block2 := testBlock(hash64("rollR2"), 2, block1.Hash, cb2)
	mustApply(t, ctx, s, block2)

	r2, err := s.GetBlockSupplyRollup(ctx, block2.Hash)
	if err != nil || r2 == nil {
		t.Fatalf("GetBlockSupplyRollup(block2): r=%+v err=%v", r2, err)
	}
	if r2.ExcludedOutputSatoshis != opReturnPayload {
		t.Errorf("block2 ExcludedOutputSatoshis = %d, want %d", r2.ExcludedOutputSatoshis, opReturnPayload)
	}
	wantCum := r.CumulativeExcludedOutputSatoshis + opReturnPayload
	if r2.CumulativeExcludedOutputSatoshis != wantCum {
		t.Errorf("block2 CumulativeExcludedOutputSatoshis = %d, want %d", r2.CumulativeExcludedOutputSatoshis, wantCum)
	}
}

// ─── 44: oversized script (the OTHER IsUnspendable branch) ───────────────

func TestSupplyRollup_OversizedScript(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("rollO0"), 0, "", coinbaseTx(hash64("rollO0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	// A non-OP_RETURN scriptPubKey longer than script.MaxScriptSize is
	// unspendable purely by length — script.IsUnspendable's "len(pkScript) >
	// MaxScriptSize" branch, independent of OP_RETURN.
	oversizedScript := make([]byte, script.MaxScriptSize+1)
	for i := range oversizedScript {
		oversizedScript[i] = 0x51 // OP_TRUE-ish filler, deliberately NOT OP_RETURN
	}
	if oversizedScript[0] == 0x6a {
		t.Fatal("test fixture bug: oversized script must not start with OP_RETURN")
	}
	const oversizedValue = int64(999_000)
	oversizedOut := chain.Output{Index: 1, Value: chain.Amount(oversizedValue), ScriptPubKey: oversizedScript, ScriptType: script.TypeUnknown}
	if !script.IsUnspendable(oversizedScript) {
		t.Fatal("test fixture bug: oversized script must be IsUnspendable")
	}

	cb := coinbaseTx(hash64("rollO1tx"), out(0, era0Subsidy-oversizedValue, "qMiner"), oversizedOut)
	block1 := testBlock(hash64("rollO1"), 1, g.Hash, cb)
	mustApply(t, ctx, s, block1)

	r, err := s.GetBlockSupplyRollup(ctx, block1.Hash)
	if err != nil || r == nil {
		t.Fatalf("GetBlockSupplyRollup: r=%+v err=%v", r, err)
	}
	if r.ExcludedOutputSatoshis != oversizedValue {
		t.Errorf("ExcludedOutputSatoshis = %d, want %d", r.ExcludedOutputSatoshis, oversizedValue)
	}
	if want := era0Subsidy - oversizedValue; r.CumulativeUTXOSetValueSatoshis != want {
		t.Errorf("CumulativeUTXOSetValueSatoshis = %d, want %d (oversized-script output must never enter the UTXO set)", r.CumulativeUTXOSetValueSatoshis, want)
	}
}

// ─── 45: address independence ─────────────────────────────────────────────

func TestSupplyRollup_AddressIndependence(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("rollAI0"), 0, "", coinbaseTx(hash64("rollAI0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	// A perfectly ordinary, spendable, non-multisig output with NO
	// destination address at all (Address left empty) — applyOutput never
	// writes an output_addresses row for it, but it must still be eligible
	// for utxo_state (and therefore this rollup's cumulative UTXO value):
	// balance-cache/address accounting must never be a dependency of supply
	// accounting.
	noAddrOut := chain.Output{Index: 0, Value: chain.Amount(era0Subsidy), ScriptPubKey: []byte{0x00}, ScriptType: script.TypeP2PKH}
	cb := coinbaseTx(hash64("rollAI1tx"), noAddrOut)
	block1 := testBlock(hash64("rollAI1"), 1, g.Hash, cb)
	mustApply(t, ctx, s, block1)

	var addrRowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM output_addresses WHERE txid = $1`, hash64("rollAI1tx")).Scan(&addrRowCount); err != nil {
		t.Fatalf("count output_addresses: %v", err)
	}
	if addrRowCount != 0 {
		t.Fatalf("test fixture bug: expected no output_addresses row, got %d", addrRowCount)
	}

	r, err := s.GetBlockSupplyRollup(ctx, block1.Hash)
	if err != nil || r == nil {
		t.Fatalf("GetBlockSupplyRollup: r=%+v err=%v", r, err)
	}
	if r.CumulativeUTXOSetValueSatoshis != era0Subsidy {
		t.Errorf("CumulativeUTXOSetValueSatoshis = %d, want %d (address-less UTXO must still be represented)", r.CumulativeUTXOSetValueSatoshis, era0Subsidy)
	}
}

// ─── 46: reorg ────────────────────────────────────────────────────────────

func TestSupplyRollup_Reorg(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("rollRG0"), 0, "", coinbaseTx(hash64("rollRG0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	cbA := coinbaseTx(hash64("rollRGAtx"), out(0, era0Subsidy, "qMinerA"))
	blockA := testBlock(hash64("rollRGA"), 1, g.Hash, cbA)
	mustApply(t, ctx, s, blockA)

	cbB := coinbaseTx(hash64("rollRGBtx"), out(0, era0Subsidy, "qMinerB"))
	blockB := testBlock(hash64("rollRGB"), 2, blockA.Hash, cbB)
	mustApply(t, ctx, s, blockB)

	rA, err := s.GetBlockSupplyRollup(ctx, blockA.Hash)
	if err != nil || rA == nil {
		t.Fatalf("GetBlockSupplyRollup(A): r=%+v err=%v", rA, err)
	}
	rB, err := s.GetBlockSupplyRollup(ctx, blockB.Hash)
	if err != nil || rB == nil {
		t.Fatalf("GetBlockSupplyRollup(B): r=%+v err=%v", rB, err)
	}

	if err := s.RollbackTo(ctx, g.Hash); err != nil {
		t.Fatalf("RollbackTo(genesis): %v", err)
	}

	// A/B rollups must remain byte-for-byte unchanged — RollbackTo never
	// touches block_supply_rollup.
	rAAfter, err := s.GetBlockSupplyRollup(ctx, blockA.Hash)
	if err != nil || rAAfter == nil {
		t.Fatalf("GetBlockSupplyRollup(A) after rollback: r=%+v err=%v", rAAfter, err)
	}
	if *rAAfter != *rA {
		t.Errorf("blockA rollup changed after rollback: before=%+v after=%+v", rA, rAAfter)
	}
	rBAfter, err := s.GetBlockSupplyRollup(ctx, blockB.Hash)
	if err != nil || rBAfter == nil {
		t.Fatalf("GetBlockSupplyRollup(B) after rollback: r=%+v err=%v", rBAfter, err)
	}
	if *rBAfter != *rB {
		t.Errorf("blockB rollup changed after rollback: before=%+v after=%+v", rB, rBAfter)
	}

	// Apply the replacement branch G-C-D. blockA/blockB both fully claimed
	// their coinbase reward; blockD deliberately UNDERCLAIMS by a distinct
	// amount, so its cumulative state is guaranteed to differ numerically
	// from the orphaned blockB's, not merely occupy a different hash.
	cbC := coinbaseTx(hash64("rollRGCtx"), out(0, era0Subsidy, "qMinerC"))
	blockC := testBlock(hash64("rollRGC"), 1, g.Hash, cbC)
	mustApply(t, ctx, s, blockC)

	const shortfallD = int64(333_000)
	cbD := coinbaseTx(hash64("rollRGDtx"), out(0, era0Subsidy-shortfallD, "qMinerD"))
	blockD := testBlock(hash64("rollRGD"), 2, blockC.Hash, cbD)
	mustApply(t, ctx, s, blockD)

	rD, err := s.GetBlockSupplyRollup(ctx, blockD.Hash)
	if err != nil || rD == nil {
		t.Fatalf("GetBlockSupplyRollup(D): r=%+v err=%v", rD, err)
	}
	if *rD == *rB {
		t.Errorf("blockD's cumulative state must be distinct from the orphaned blockB's — both got %+v", rD)
	}
	if rD.CumulativeUnclaimedRewardSatoshis != shortfallD {
		t.Errorf("blockD.CumulativeUnclaimedRewardSatoshis = %d, want %d", rD.CumulativeUnclaimedRewardSatoshis, shortfallD)
	}

	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != blockD.Hash {
		t.Errorf("tip = %s, want %s (blockD)", tip.Hash, blockD.Hash)
	}
}

// ─── 47: re-promotion ─────────────────────────────────────────────────────

func TestSupplyRollup_RePromotion(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	g := testBlock(hash64("rollRP0"), 0, "", coinbaseTx(hash64("rollRP0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	cbA := coinbaseTx(hash64("rollRPAtx"), out(0, era0Subsidy, "qMinerA"))
	blockA := testBlock(hash64("rollRPA"), 1, g.Hash, cbA)
	mustApply(t, ctx, s, blockA)

	cbB := coinbaseTx(hash64("rollRPBtx"), out(0, era0Subsidy, "qMinerB"))
	blockB := testBlock(hash64("rollRPB"), 2, blockA.Hash, cbB)
	mustApply(t, ctx, s, blockB)

	rBBefore, err := s.GetBlockSupplyRollup(ctx, blockB.Hash)
	if err != nil || rBBefore == nil {
		t.Fatalf("GetBlockSupplyRollup(B) before: r=%+v err=%v", rBBefore, err)
	}

	if err := s.RollbackTo(ctx, blockA.Hash); err != nil {
		t.Fatalf("RollbackTo(A): %v", err)
	}

	// Re-promote blockB by re-applying the exact same block as the
	// immediate child of the (still-canonical) tip A.
	if err := s.ApplyBlock(ctx, blockB); err != nil {
		t.Fatalf("re-apply (re-promote) blockB: %v", err)
	}

	rBAfter, err := s.GetBlockSupplyRollup(ctx, blockB.Hash)
	if err != nil || rBAfter == nil {
		t.Fatalf("GetBlockSupplyRollup(B) after: r=%+v err=%v", rBAfter, err)
	}
	if *rBAfter != *rBBefore {
		t.Errorf("re-promoted blockB rollup differs: before=%+v after=%+v", rBBefore, rBAfter)
	}
}

// ─── 48: arbitrary bootstrap ───────────────────────────────────────────────

func TestSupplyRollup_ArbitraryBootstrap(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)

	// Mirrors this codebase's existing arbitrary-synthetic-bootstrap-height
	// test convention (apply_test.go's testBlock usage elsewhere): the
	// FIRST block ever applied to an uninitialized store need not be
	// height 0.
	boot := testBlock(hash64("rollAB100"), 100, "", coinbaseTx(hash64("rollAB100tx"), out(0, 5_000_000_000, "qAlice")))
	if err := s.ApplyBlock(ctx, boot); err != nil {
		t.Fatalf("bootstrap ApplyBlock at arbitrary height must succeed: %v", err)
	}

	next := testBlock(hash64("rollAB101"), 101, boot.Hash, coinbaseTx(hash64("rollAB101tx"), out(0, era0Subsidy, "qMiner")))
	if err := s.ApplyBlock(ctx, next); err != nil {
		t.Fatalf("appending on top of an arbitrary bootstrap must succeed: %v", err)
	}

	r, err := s.GetBlockSupplyRollup(ctx, boot.Hash)
	if err != nil {
		t.Fatalf("GetBlockSupplyRollup(boot): %v", err)
	}
	if r != nil {
		t.Errorf("expected NO rollup row for an arbitrary-height bootstrap block, got %+v", r)
	}
	r2, err := s.GetBlockSupplyRollup(ctx, next.Hash)
	if err != nil {
		t.Fatalf("GetBlockSupplyRollup(next): %v", err)
	}
	if r2 != nil {
		t.Errorf("expected NO rollup row for a block appended on an arbitrary bootstrap chain, got %+v", r2)
	}
}

// ─── 49: missing parent ────────────────────────────────────────────────────

func TestSupplyRollup_MissingParent(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("rollMP0"), 0, "", coinbaseTx(hash64("rollMP0tx"), out(0, era0Subsidy, "qAlice")))
	mustApply(t, ctx, s, g)

	cb1 := coinbaseTx(hash64("rollMP1tx"), out(0, era0Subsidy, "qMiner1"))
	block1 := testBlock(hash64("rollMP1"), 1, g.Hash, cb1)
	mustApply(t, ctx, s, block1)

	// Simulate external corruption: block1's rollup row disappears even
	// though block1 remains canonical and genesis's rollup lineage is
	// still active.
	if _, err := pool.Exec(ctx, `DELETE FROM block_supply_rollup WHERE block_hash = $1`, block1.Hash); err != nil {
		t.Fatalf("simulate corrupted rollup: %v", err)
	}

	tipBefore, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip (before): %v", err)
	}

	cb2 := coinbaseTx(hash64("rollMP2tx"), out(0, era0Subsidy, "qMiner2"))
	block2 := testBlock(hash64("rollMP2"), 2, block1.Hash, cb2)
	err = s.ApplyBlock(ctx, block2)
	if err == nil {
		t.Fatal("expected ApplyBlock to fail when the parent's rollup row is missing on an active lineage")
	}
	if !errors.Is(err, ErrRollupParentMissing) {
		t.Errorf("error = %v, want ErrRollupParentMissing", err)
	}

	tipAfter, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip (after): %v", err)
	}
	if tipAfter != tipBefore {
		t.Errorf("checkpoint advanced despite the failed ApplyBlock: before=%+v after=%+v", tipBefore, tipAfter)
	}

	var blockCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, block2.Hash).Scan(&blockCount); err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if blockCount != 0 {
		t.Errorf("block2 persisted despite the failed ApplyBlock: count = %d", blockCount)
	}
}

// ─── 50: overflow ──────────────────────────────────────────────────────────

func TestSupplyRollup_Overflow(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	// Fabricate a parent block + block_accounting + block_supply_rollup row
	// directly via SQL (bypassing Store's write path — the exact same
	// "impossible state" fixture technique used elsewhere in this package
	// for integrity tests), with cumulative values near math.MaxInt64, so a
	// single ordinary-sized block appended on top is guaranteed to overflow
	// the checked cumulative addition.
	parentHash := hash64("rollOV0")
	nearMax := int64(math.MaxInt64) - 1000

	if _, err := pool.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, 0, NULL, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, parentHash); err != nil {
		t.Fatalf("fixture: insert parent block: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO block_accounting (block_hash, subsidy_satoshis, fee_satoshis, coinbase_output_satoshis, unclaimed_reward_satoshis)
		VALUES ($1, $2, 0, $2, 0)
	`, parentHash, nearMax); err != nil {
		t.Fatalf("fixture: insert parent block_accounting: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO block_supply_rollup (
			block_hash, excluded_output_satoshis,
			cumulative_subsidy_satoshis, cumulative_fee_satoshis,
			cumulative_coinbase_output_satoshis, cumulative_unclaimed_reward_satoshis,
			cumulative_excluded_output_satoshis, cumulative_utxo_set_value_satoshis
		) VALUES ($1, 0, $2, 0, $2, 0, 0, $2)
	`, parentHash, nearMax); err != nil {
		t.Fatalf("fixture: insert parent block_supply_rollup: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'`, parentHash); err != nil {
		t.Fatalf("fixture: set checkpoint to parent: %v", err)
	}

	// era0Subsidy (~1e10) added to nearMax (MaxInt64-1000) overflows int64.
	cb := coinbaseTx(hash64("rollOV1tx"), out(0, era0Subsidy, "qMiner"))
	child := testBlock(hash64("rollOV1"), 1, parentHash, cb)
	err := s.ApplyBlock(ctx, child)
	if err == nil {
		t.Fatal("expected ApplyBlock to reject an overflowing cumulative subsidy addition")
	}
	if !errors.Is(err, ErrAmountOverflow) {
		t.Errorf("error = %v, want ErrAmountOverflow", err)
	}

	var childBlockCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE hash = $1`, child.Hash).Scan(&childBlockCount); err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	if childBlockCount != 0 {
		t.Errorf("child block persisted despite the overflow rejection: count = %d", childBlockCount)
	}
}
