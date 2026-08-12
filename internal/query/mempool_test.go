package query

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/script"
)

// TestMempoolState_Uninitialized is spec item 35: a fresh migration with no
// successful mempool publication must report initialized=false explicitly
// — never a fake synchronized-and-empty state, and Stale must never read
// true for an uninitialized cache (Stale only has meaning once Initialized
// is true).
func TestMempoolState_Uninitialized(t *testing.T) {
	ctx := context.Background()
	q, _, _ := newTestQueryStore(t)

	st, err := q.MempoolState(ctx)
	if err != nil {
		t.Fatalf("MempoolState: %v", err)
	}
	if st.Initialized {
		t.Fatalf("Initialized = true, want false")
	}
	if st.Stale {
		t.Fatalf("Stale = true, want false for an uninitialized cache")
	}
	if st.Status != "uninitialized" {
		t.Fatalf("Status = %q, want \"uninitialized\"", st.Status)
	}
	if st.Generation != 0 || st.TxCount != 0 {
		t.Fatalf("Generation/TxCount = %d/%d, want 0/0", st.Generation, st.TxCount)
	}
	if st.CoreTipHeight != nil || st.CoreTipHash != nil {
		t.Fatalf("CoreTipHeight/CoreTipHash = %v/%v, want nil/nil", st.CoreTipHeight, st.CoreTipHash)
	}

	overview, err := q.MempoolOverview(ctx, nil, 50)
	if err != nil {
		t.Fatalf("MempoolOverview: %v", err)
	}
	if overview.State.Initialized {
		t.Fatalf("overview.State.Initialized = true, want false")
	}
	if len(overview.Transactions.Transactions) != 0 {
		t.Fatalf("overview.Transactions = %+v, want empty", overview.Transactions.Transactions)
	}
}

// TestMempoolState_EmptyInitialized is spec item 36: after a real
// successful EMPTY ReplaceSnapshot, the cache is initialized=true,
// tx_count=0 — a state that API/UI must distinguish from "never
// synchronized" (TestMempoolState_Uninitialized above).
func TestMempoolState_EmptyInitialized(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	anchorHash := fakeHash("empty-initialized-tip")
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(5, anchorHash)); err != nil {
		t.Fatalf("ReplaceSnapshot(empty): %v", err)
	}

	st, err := q.MempoolState(ctx)
	if err != nil {
		t.Fatalf("MempoolState: %v", err)
	}
	if !st.Initialized {
		t.Fatalf("Initialized = false, want true")
	}
	if st.TxCount != 0 {
		t.Fatalf("TxCount = %d, want 0", st.TxCount)
	}
	if st.Generation != 1 {
		t.Fatalf("Generation = %d, want 1", st.Generation)
	}
	if st.CoreTipHeight == nil || *st.CoreTipHeight != 5 || st.CoreTipHash == nil || *st.CoreTipHash != anchorHash {
		t.Fatalf("CoreTipHeight/Hash = %v/%v, want 5/%s", st.CoreTipHeight, st.CoreTipHash, anchorHash)
	}
	// Confirmed tip is still the bootstrap -1/nil, so this is legitimately
	// stale (asynchronous, not corruption) — but crucially Status must
	// read "stale", never "uninitialized".
	if st.Status != "stale" || !st.Stale {
		t.Fatalf("Status/Stale = %q/%v, want stale/true", st.Status, st.Stale)
	}
}

// TestMempoolState_Staleness is spec item 34: confirmed tip B, mempool
// anchor A (different) => stale=true. Aligning the confirmed tip with A
// (same height AND hash) => stale=false. Both reads use one PostgreSQL
// snapshot (MempoolState -> mempoolStateFrom).
func TestMempoolState_Staleness(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	anchorLabel := "stale-genesis"
	anchorHash := fakeHash(anchorLabel)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(0, anchorHash)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	before, err := q.MempoolState(ctx)
	if err != nil {
		t.Fatalf("MempoolState (before): %v", err)
	}
	if !before.Initialized {
		t.Fatalf("Initialized = false, want true")
	}
	if !before.Stale || before.Status != "stale" {
		t.Fatalf("Status/Stale = %q/%v, want stale/true (confirmed tip has no blocks yet)", before.Status, before.Stale)
	}

	// Align the confirmed tip with the mempool anchor EXACTLY (same label
	// -> same fakeHash -> same hash, same height 0).
	g := block(anchorLabel, 0, "", coinbaseTx(anchorLabel, 100_00000000, "qStaleAlign"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	if g.Hash != anchorHash {
		t.Fatalf("fixture bug: g.Hash = %s, want %s", g.Hash, anchorHash)
	}

	after, err := q.MempoolState(ctx)
	if err != nil {
		t.Fatalf("MempoolState (after): %v", err)
	}
	if after.Stale || after.Status != "fresh" {
		t.Fatalf("Status/Stale = %q/%v, want fresh/false after aligning confirmed tip with the mempool anchor", after.Status, after.Stale)
	}
	if after.ConfirmedIndexedHeight != 0 || after.ConfirmedIndexedHash == nil || *after.ConfirmedIndexedHash != anchorHash {
		t.Fatalf("ConfirmedIndexedHeight/Hash = %d/%v, want 0/%s", after.ConfirmedIndexedHeight, after.ConfirmedIndexedHash, anchorHash)
	}
}

// TestMempoolState_StaleOnSameHeightMismatch is spec item 7: a mempool
// anchor and the confirmed indexed tip can disagree at the SAME height —
// a different hash at height H (e.g. a same-height canonical reorg,
// distinct from the "confirmed tip hasn't caught up yet" scenario
// TestMempoolState_Staleness covers). query.MempoolState's comparison only
// ever checks anchor != confirmed tip, never an ordering relationship, so
// this must still read stale — proving Stale/Status do not encode any
// forward-advancement assumption.
func TestMempoolState_StaleOnSameHeightMismatch(t *testing.T) {
	ctx := context.Background()
	q, st, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	anchorHash := fakeHash("same-height-reorg-A")
	confirmedHash := fakeHash("same-height-reorg-B")
	if anchorHash == confirmedHash {
		t.Fatalf("fixture bug: anchor and confirmed hashes must differ")
	}

	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(0, anchorHash)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	g := block("same-height-reorg-B", 0, "", coinbaseTx("same-height-reorg-B", 100_00000000, "qSameHeightReorgB"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	if g.Hash != confirmedHash {
		t.Fatalf("fixture bug: g.Hash = %s, want %s", g.Hash, confirmedHash)
	}

	got, err := q.MempoolState(ctx)
	if err != nil {
		t.Fatalf("MempoolState: %v", err)
	}
	if got.CoreTipHeight == nil || *got.CoreTipHeight != 0 || got.ConfirmedIndexedHeight != 0 {
		t.Fatalf("heights = anchor:%v confirmed:%d, want both 0 (same-height mismatch)", got.CoreTipHeight, got.ConfirmedIndexedHeight)
	}
	if !got.Stale || got.Status != "stale" {
		t.Fatalf("Status/Stale = %q/%v, want stale/true (same height, different hash)", got.Status, got.Stale)
	}
}

// TestMempoolOverview_GenerationSafePagination is spec item 37: a cursor
// minted against generation N must be explicitly rejected
// (ErrMempoolGenerationChanged) once the mempool has been replaced with
// generation N+1 — never silently paginated against the new snapshot.
func TestMempoolOverview_GenerationSafePagination(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	txA := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("gen-page-A", 0), 1000, 1_700_000_100, nil, nil, nil)
	txB := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("gen-page-B", 0), 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("gen-page-tip-1"), txA, txB)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen1): %v", err)
	}

	// pageSize=1 forces a cursor after the first (newest entry_time) row.
	page1, err := q.MempoolOverview(ctx, nil, 1)
	if err != nil {
		t.Fatalf("MempoolOverview(page1): %v", err)
	}
	if len(page1.Transactions.Transactions) != 1 || page1.Transactions.NextCursor == nil {
		t.Fatalf("page1 = %+v, want exactly 1 row and a NextCursor", page1.Transactions)
	}
	staleCursor := page1.Transactions.NextCursor

	// Replace with a completely different candidate -> generation 2.
	txC := mempoolCandidateTx(t, ctx, simpleMempoolRawTx("gen-page-C", 0), 1000, 1_700_000_050, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(2, fakeHash("gen-page-tip-2"), txC)); err != nil {
		t.Fatalf("ReplaceSnapshot(gen2): %v", err)
	}

	_, err = q.MempoolOverview(ctx, staleCursor, 1)
	if !errors.Is(err, ErrMempoolGenerationChanged) {
		t.Fatalf("MempoolOverview(stale cursor) error = %v, want ErrMempoolGenerationChanged", err)
	}

	// A fresh (no-cursor) call must see generation 2 cleanly.
	fresh, err := q.MempoolOverview(ctx, nil, 50)
	if err != nil {
		t.Fatalf("MempoolOverview(fresh): %v", err)
	}
	if fresh.State.Generation != 2 || len(fresh.Transactions.Transactions) != 1 || fresh.Transactions.Transactions[0].TxID != txC.TxID {
		t.Fatalf("fresh overview = %+v, want generation=2 with only txC", fresh)
	}
}

// TestMempoolTransaction_TxIDAndWTxIDLookup is spec item 38: a witness
// transaction (txid != wtxid) must be identically reachable by both
// identities, with both hashes preserved exactly.
func TestMempoolTransaction_TxIDAndWTxIDLookup(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	addr := "qLookupDest"
	raw := rawSpendTx("lookup-witness", 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(fakeHash("lookup-prev"), 0, "", "aa", "bb")},
		[]rpc.RawVout{rawVout(0, 5_00000000, p2pkhScript("lookup-out"), "pubkeyhash", &addr)},
	)
	if *raw.TxID == *raw.Hash {
		t.Fatalf("fixture bug: txid must differ from wtxid")
	}

	ctxn := mempoolCandidateTx(t, ctx, raw, 1000, 1_700_000_000, nil, nil, nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("lookup-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	byTxID, err := q.MempoolTransactionByTxID(ctx, ctxn.TxID, false)
	if err != nil {
		t.Fatalf("MempoolTransactionByTxID: %v", err)
	}
	byWTxID, err := q.MempoolTransactionByWTxID(ctx, ctxn.WTxID, false)
	if err != nil {
		t.Fatalf("MempoolTransactionByWTxID: %v", err)
	}

	if byTxID.TxID != ctxn.TxID || byTxID.WTxID != ctxn.WTxID {
		t.Fatalf("byTxID identities = %s/%s, want %s/%s", byTxID.TxID, byTxID.WTxID, ctxn.TxID, ctxn.WTxID)
	}
	if byWTxID.TxID != ctxn.TxID || byWTxID.WTxID != ctxn.WTxID {
		t.Fatalf("byWTxID identities = %s/%s, want %s/%s", byWTxID.TxID, byWTxID.WTxID, ctxn.TxID, ctxn.WTxID)
	}

	_, err = q.MempoolTransactionByWTxID(ctx, fakeHash("lookup-does-not-exist"), false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("MempoolTransactionByWTxID(missing) error = %v, want ErrNotFound", err)
	}
}

// TestMempoolTransaction_DetailNotFound is spec item 20: an initialized
// mempool with a missing txid/wtxid is ErrNotFound — never a fallback of
// any kind.
func TestMempoolTransaction_DetailNotFound(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("notfound-tip"))); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	_, err := q.MempoolTransactionByTxID(ctx, fakeHash("does-not-exist"), false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("MempoolTransactionByTxID(missing) error = %v, want ErrNotFound", err)
	}
}

// TestMempoolTransaction_P2QPKAndP2TR is spec items 39/40: real
// rpc.RawTransaction -> decode.DecodeTransaction -> mempool.Candidate ->
// Store.ReplaceSnapshot -> query layer pipeline. Default detail hides raw
// witness bytes; explicit include_witness=true returns them byte-exact,
// never truncated. P2TR (v1/32) must remain visibly distinct from P2QPK
// (v2/32).
func TestMempoolTransaction_P2QPKAndP2TR(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	sigItem := bytes.Repeat([]byte{0xab}, script.P2QPKSignatureLength)
	pubkeyItem := bytes.Repeat([]byte{0xcd}, script.P2QPKPublicKeyLength)
	p2qpkProgram := bytes.Repeat([]byte{0xef}, script.P2QPKProgramLength)
	p2trProgram := bytes.Repeat([]byte{0x11}, 32)

	p2qpkAddr := "qP2QPKQueryDest"
	p2trAddr := "qP2TRQueryDest"

	raw := rawSpendTx("mempool-p2qpk-query", 300, 250, 1000,
		[]rpc.RawVin{rawSpendVin(fakeHash("p2qpk-query-prev"), 0, "", hex.EncodeToString(sigItem), hex.EncodeToString(pubkeyItem))},
		[]rpc.RawVout{
			rawVout(0, 25_00000000, witnessProgramScript(script.P2QPKWitnessVersion, p2qpkProgram), "witness_unknown", &p2qpkAddr),
			rawVout(1, 10_00000000, witnessProgramScript(1, p2trProgram), "witness_v1_taproot", &p2trAddr),
		},
	)

	ctxn := mempoolCandidateTx(t, ctx, raw, 10_00000000, 1_700_000_000, i64Ptr(500), boolPtr(true), nil)
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(500, fakeHash("p2qpk-query-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	// Default: no raw witness.
	got, err := q.MempoolTransactionByTxID(ctx, ctxn.TxID, false)
	if err != nil {
		t.Fatalf("MempoolTransactionByTxID: %v", err)
	}
	if got.Outputs[0].ScriptType != string(script.TypeP2QPK) {
		t.Fatalf("Outputs[0].ScriptType = %s, want p2qpk", got.Outputs[0].ScriptType)
	}
	if got.Outputs[0].WitnessVersion == nil || *got.Outputs[0].WitnessVersion != script.P2QPKWitnessVersion {
		t.Fatalf("Outputs[0].WitnessVersion = %v, want %d", got.Outputs[0].WitnessVersion, script.P2QPKWitnessVersion)
	}
	if len(got.Inputs) != 1 || len(got.Inputs[0].Witness) != 2 {
		t.Fatalf("Inputs = %+v, want 1 input with a 2-item witness", got.Inputs)
	}
	w := got.Inputs[0].Witness
	if w[0].SizeBytes != script.P2QPKSignatureLength || w[0].DataHex != nil {
		t.Fatalf("default witness[0] = %+v, want size=%d hidden", w[0], script.P2QPKSignatureLength)
	}
	if w[1].SizeBytes != script.P2QPKPublicKeyLength || w[1].DataHex != nil {
		t.Fatalf("default witness[1] = %+v, want size=%d hidden", w[1], script.P2QPKPublicKeyLength)
	}

	// P2TR negative control.
	if got.Outputs[1].ScriptType != string(script.TypeP2TR) {
		t.Fatalf("Outputs[1].ScriptType = %s, want p2tr", got.Outputs[1].ScriptType)
	}
	if got.Outputs[1].WitnessVersion == nil || *got.Outputs[1].WitnessVersion != 1 {
		t.Fatalf("Outputs[1].WitnessVersion = %v, want 1", got.Outputs[1].WitnessVersion)
	}
	if got.Outputs[1].ScriptType == got.Outputs[0].ScriptType {
		t.Fatalf("P2TR and P2QPK classified identically: %s", got.Outputs[1].ScriptType)
	}

	// Explicit opt-in: byte-exact, never truncated.
	gotRaw, err := q.MempoolTransactionByTxID(ctx, ctxn.TxID, true)
	if err != nil {
		t.Fatalf("MempoolTransactionByTxID(includeRawWitness): %v", err)
	}
	wr := gotRaw.Inputs[0].Witness
	if wr[0].DataHex == nil || *wr[0].DataHex != hex.EncodeToString(sigItem) || len(*wr[0].DataHex)/2 != script.P2QPKSignatureLength {
		t.Fatalf("raw witness[0] not byte-exact")
	}
	if wr[1].DataHex == nil || *wr[1].DataHex != hex.EncodeToString(pubkeyItem) || len(*wr[1].DataHex)/2 != script.P2QPKPublicKeyLength {
		t.Fatalf("raw witness[1] not byte-exact")
	}

	// List pages structurally never carry witness data at all —
	// MempoolTxSummary has no witness field, so this is enforced by the
	// type itself; confirm the list call still succeeds and shows the
	// summary fields expected.
	overview, err := q.MempoolOverview(ctx, nil, 50)
	if err != nil {
		t.Fatalf("MempoolOverview: %v", err)
	}
	if len(overview.Transactions.Transactions) != 1 || overview.Transactions.Transactions[0].TxID != ctxn.TxID {
		t.Fatalf("overview transactions = %+v", overview.Transactions.Transactions)
	}
}

// TestMempoolTransaction_Multisig is spec item 41: a bare multisig output
// must show participants SEPARATELY from Address (never a single
// balance-accounting destination), and must never be summed once per
// participant.
func TestMempoolTransaction_Multisig(t *testing.T) {
	ctx := context.Background()
	q, _, pool := newTestQueryStore(t)
	mstore := newTestMempoolStore(pool)

	pub1 := compressedPubKey("query-multisig-1")
	pub2 := compressedPubKey("query-multisig-2")
	msScript := multisigScript([][]byte{pub1, pub2})

	raw := rawSpendTx("mempool-multisig-query", 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(fakeHash("multisig-query-prev"), 0, "473044")},
		[]rpc.RawVout{rawVout(0, 5_00000000, msScript, "multisig", nil)},
	)
	ctxn := mempoolCandidateTx(t, ctx, raw, 1000, 1_700_000_000, nil, nil, nil)
	if len(ctxn.Outputs[0].ParticipantAddresses) != 2 {
		t.Fatalf("fixture bug: want 2 participant addresses, got %d", len(ctxn.Outputs[0].ParticipantAddresses))
	}
	if _, err := mstore.ReplaceSnapshot(ctx, mempoolCandidate(1, fakeHash("multisig-query-tip"), ctxn)); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	got, err := q.MempoolTransactionByTxID(ctx, ctxn.TxID, false)
	if err != nil {
		t.Fatalf("MempoolTransactionByTxID: %v", err)
	}
	out := got.Outputs[0]
	if out.ScriptType != "multisig" {
		t.Fatalf("ScriptType = %s, want multisig", out.ScriptType)
	}
	if out.Address != nil {
		t.Fatalf("Address = %v, want nil (multisig has no single balance-accounting destination)", out.Address)
	}
	if len(out.Participants) != 2 {
		t.Fatalf("Participants = %+v, want 2", out.Participants)
	}
}

// simpleMempoolRawTx builds a minimal single-input, single-output
// non-coinbase rpc.RawTransaction (no witness) with distinct txid derived
// from label, and a fixed entry_time offset from a base so callers can
// control ordering precisely.
func simpleMempoolRawTx(label string, _ int) rpc.RawTransaction {
	addr := "q" + label
	return rawSpendTx(label, 200, 150, 600,
		[]rpc.RawVin{rawSpendVin(fakeHash(label+"-prev"), 0, "473044")},
		[]rpc.RawVout{rawVout(0, 1_00000000, p2pkhScript(label), "pubkeyhash", &addr)},
	)
}
