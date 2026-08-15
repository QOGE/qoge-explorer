package store

import (
	"context"
	"errors"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── bucket classification ──────────────────────────────────────────────

// TestDistributionBuckets_MatchMigrationSeed proves distributionBuckets
// (distribution.go) and migration 0007's seeded rows never drift apart —
// the Go-side hardcoded boundaries used for hot-path classification must
// exactly match what the migration/backfill/SQL side considers each
// bucket's range.
func TestDistributionBuckets_MatchMigrationSeed(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)

	rows, err := pool.Query(ctx, `SELECT bucket_id, min_balance_satoshis, max_balance_satoshis FROM address_balance_distribution`)
	if err != nil {
		t.Fatalf("query seeded buckets: %v", err)
	}
	defer rows.Close()

	seeded := map[string]struct {
		min int64
		max *int64
	}{}
	for rows.Next() {
		var id string
		var min int64
		var max *int64
		if err := rows.Scan(&id, &min, &max); err != nil {
			t.Fatalf("scan seeded bucket: %v", err)
		}
		seeded[id] = struct {
			min int64
			max *int64
		}{min, max}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate seeded buckets: %v", err)
	}

	if len(seeded) != len(distributionBuckets) {
		t.Fatalf("seeded bucket count = %d, want %d", len(seeded), len(distributionBuckets))
	}
	for _, b := range distributionBuckets {
		s, ok := seeded[b.id]
		if !ok {
			t.Errorf("bucket %s: present in Go code but not seeded in migration 0007", b.id)
			continue
		}
		if s.min != b.minSatoshis {
			t.Errorf("bucket %s: seeded min=%d, Go min=%d", b.id, s.min, b.minSatoshis)
		}
		if (s.max == nil) != (b.maxSatoshis == nil) {
			t.Errorf("bucket %s: seeded max nil=%t, Go max nil=%t", b.id, s.max == nil, b.maxSatoshis == nil)
		} else if s.max != nil && *s.max != *b.maxSatoshis {
			t.Errorf("bucket %s: seeded max=%d, Go max=%d", b.id, *s.max, *b.maxSatoshis)
		}
	}
}

// TestBucketForBalance_ExactBoundaries is Phase 2H.4a spec §3's mandatory
// boundary test list, exercised directly against the classification
// function the hot path uses.
func TestBucketForBalance_ExactBoundaries(t *testing.T) {
	const qoge = SatoshisPerQOGE
	cases := []struct {
		name    string
		balance int64
		want    string
	}{
		{"zero belongs to no bucket", 0, ""},
		{"1 sat", 1, "lt_1"},
		{"1 QOGE - 1 sat", qoge - 1, "lt_1"},
		{"exactly 1 QOGE", qoge, "1_10"},
		{"exactly 10 QOGE", 10 * qoge, "10_100"},
		{"exactly 100 QOGE", 100 * qoge, "100_1k"},
		{"exactly 1,000 QOGE", 1_000 * qoge, "1k_10k"},
		{"exactly 10,000 QOGE", 10_000 * qoge, "10k_100k"},
		{"exactly 100,000 QOGE", 100_000 * qoge, "100k_1m"},
		{"exactly 1,000,000 QOGE", 1_000_000 * qoge, "gte_1m"},
		{"well above 1,000,000 QOGE", 5_000_000 * qoge, "gte_1m"},
		{"negative belongs to no bucket", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := bucketForBalance(tc.balance)
			if tc.want == "" {
				if ok {
					t.Fatalf("bucketForBalance(%d) = (%q, true), want ok=false", tc.balance, got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("bucketForBalance(%d) = (%q, %t), want (%q, true)", tc.balance, got, ok, tc.want)
			}
		})
	}
}

// ─── incremental maintenance (ApplyBlock) ───────────────────────────────

// distBucket reads one bucket's current (address_count, balance_satoshis).
func distBucket(t *testing.T, ctx context.Context, pool *pgxpool.Pool, bucketID string) (count, balance int64) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT address_count, balance_satoshis FROM address_balance_distribution WHERE bucket_id = $1`, bucketID).Scan(&count, &balance); err != nil {
		t.Fatalf("read bucket %s: %v", bucketID, err)
	}
	return count, balance
}

// distState reads the singleton anchor.
func distState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (height int64, hash *string) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT indexed_height, indexed_block_hash FROM address_balance_distribution_state WHERE name = 'main'`).Scan(&height, &hash); err != nil {
		t.Fatalf("read distribution state: %v", err)
	}
	return height, hash
}

// requireBucketsSumToAddresses is the spec §12 global identity: sum of
// every bucket's address_count/balance_satoshis must equal a direct
// aggregate of addresses.balance_satoshis > 0.
func requireBucketsSumToAddresses(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var sumCount, sumBalance int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(address_count),0), COALESCE(sum(balance_satoshis),0) FROM address_balance_distribution`).Scan(&sumCount, &sumBalance); err != nil {
		t.Fatalf("sum buckets: %v", err)
	}
	var directCount, directBalance int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(balance_satoshis),0) FROM addresses WHERE balance_satoshis > 0`).Scan(&directCount, &directBalance); err != nil {
		t.Fatalf("direct aggregate addresses: %v", err)
	}
	if sumCount != directCount || sumBalance != directBalance {
		t.Fatalf("bucket sums (count=%d,balance=%d) != direct addresses aggregate (count=%d,balance=%d)",
			sumCount, sumBalance, directCount, directBalance)
	}
}

// TestApplyBlock_Distribution_ZeroToPositive covers §18's "fresh/
// uninitialized DB", "genesis only", "first positive address", and
// zero->positive transition, plus the anchor tracking sync_state.
func TestApplyBlock_Distribution_ZeroToPositive(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	height, hash := distState(t, ctx, pool)
	if height != -1 || hash != nil {
		t.Fatalf("distribution state before any block = (%d,%v), want uninitialized", height, hash)
	}

	block := testBlock(hash64("distZP"), 100, "",
		coinbaseTx(hash64("distZPtx"), out(0, 5*SatoshisPerQOGE, "qDistZP")),
	)
	mustApply(t, ctx, s, block)

	// 5 QOGE lands in bucket "1_10".
	count, balance := distBucket(t, ctx, pool, "1_10")
	if count != 1 || balance != 5*SatoshisPerQOGE {
		t.Fatalf("bucket 1_10 = (count=%d,balance=%d), want (1,%d)", count, balance, 5*SatoshisPerQOGE)
	}
	for _, id := range []string{"lt_1", "10_100", "100_1k", "1k_10k", "10k_100k", "100k_1m", "gte_1m"} {
		c, b := distBucket(t, ctx, pool, id)
		if c != 0 || b != 0 {
			t.Errorf("bucket %s = (count=%d,balance=%d), want (0,0)", id, c, b)
		}
	}

	newHeight, newHash := distState(t, ctx, pool)
	if newHeight != 100 || newHash == nil || *newHash != hash64("distZP") {
		t.Fatalf("distribution state after block = (%d,%v), want (100,%s)", newHeight, newHash, hash64("distZP"))
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_MultipleUTXOsSameAddress covers §18
// "multiple UTXOs same address": address_count still 1, balance is the
// sum, even though the address holds several distinct unspent outputs.
func TestApplyBlock_Distribution_MultipleUTXOsSameAddress(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	// Two separate coinbase outputs to the SAME address in one block —
	// two distinct UTXOs, one address.
	block := testBlock(hash64("distMU"), 100, "",
		coinbaseTx(hash64("distMUtx"), out(0, 50_000_000, "qDistMU"), out(1, 40_000_000, "qDistMU")),
	)
	mustApply(t, ctx, s, block)

	count, balance := distBucket(t, ctx, pool, "lt_1")
	if count != 1 || balance != 90_000_000 {
		t.Fatalf("bucket lt_1 = (count=%d,balance=%d), want (1,90000000)", count, balance)
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_MultipleAddressesSameBucket covers §18
// "multiple addresses same bucket": address_count increments per address.
func TestApplyBlock_Distribution_MultipleAddressesSameBucket(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("distMA"), 100, "",
		coinbaseTx(hash64("distMAtx"),
			out(0, 2*SatoshisPerQOGE, "qDistMA1"),
			out(1, 3*SatoshisPerQOGE, "qDistMA2"),
		),
	)
	mustApply(t, ctx, s, block)

	count, balance := distBucket(t, ctx, pool, "1_10")
	if count != 2 || balance != 5*SatoshisPerQOGE {
		t.Fatalf("bucket 1_10 = (count=%d,balance=%d), want (2,%d)", count, balance, 5*SatoshisPerQOGE)
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_SameBucketIncreaseDecrease covers §18
// "same-bucket balance increase/decrease": address_count for the bucket
// stays 1, balance moves by exactly the delta.
func TestApplyBlock_Distribution_SameBucketIncreaseDecrease(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	// qDistSB starts with 2 QOGE (output 0); a second, separate 2 QOGE
	// output (output 1) is available to merge in later WITHOUT creating
	// value from nothing.
	g := testBlock(hash64("distSB0"), 100, "",
		coinbaseTx(hash64("distSB0tx"), out(0, 2*SatoshisPerQOGE, "qDistSB"), out(1, 2*SatoshisPerQOGE, "qDistSBExtra")),
	)
	mustApply(t, ctx, s, g)

	// Increase: merge both existing UTXOs (2+2=4 QOGE available) into one
	// 3.9 QOGE output back to qDistSB (0.1 QOGE fee) — 2 -> 3.9, still
	// bucket 1_10.
	spendIn := spendTx(hash64("distSB1tx"), hash64("distSB1tx"), 200,
		[]chain.Input{spendInput(0, hash64("distSB0tx"), 0, nil), spendInput(1, hash64("distSB0tx"), 1, nil)},
		[]chain.Output{out(0, 39*SatoshisPerQOGE/10, "qDistSB")},
	)
	a := testBlock(hash64("distSB1"), 101, hash64("distSB0"), minerCoinbase("distSB1"), spendIn)
	mustApply(t, ctx, s, a)

	count, balance := distBucket(t, ctx, pool, "1_10")
	if count != 1 || balance != 39*SatoshisPerQOGE/10 {
		t.Fatalf("after increase, bucket 1_10 = (count=%d,balance=%d), want (1,%d)", count, balance, 39*SatoshisPerQOGE/10)
	}

	// Decrease: spend 3.9 QOGE down to 2.5 QOGE (1.4 QOGE fee), still
	// bucket 1_10.
	spendDec := spendTx(hash64("distSB2tx"), hash64("distSB2tx"), 200,
		[]chain.Input{spendInput(0, hash64("distSB1tx"), 0, nil)},
		[]chain.Output{out(0, 25*SatoshisPerQOGE/10, "qDistSB")},
	)
	b := testBlock(hash64("distSB2"), 102, hash64("distSB1"), minerCoinbase("distSB2"), spendDec)
	mustApply(t, ctx, s, b)

	count, balance = distBucket(t, ctx, pool, "1_10")
	if count != 1 || balance != 25*SatoshisPerQOGE/10 {
		t.Fatalf("after decrease, bucket 1_10 = (count=%d,balance=%d), want (1,%d)", count, balance, 25*SatoshisPerQOGE/10)
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_CrossBucketIncreaseDecrease covers §18
// "cross-bucket increase/decrease": the address moves out of its old
// bucket entirely and into a new one.
func TestApplyBlock_Distribution_CrossBucketIncreaseDecrease(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	// qDistCB starts with 5 QOGE (output 0); nine more 5-QOGE outputs
	// (outputs 1-9, 45 QOGE total) are available to merge in later without
	// creating value from nothing (5+45=50 QOGE available).
	coinbaseOuts := []chain.Output{out(0, 5*SatoshisPerQOGE, "qDistCB")}
	for i := uint32(1); i <= 9; i++ {
		coinbaseOuts = append(coinbaseOuts, out(i, 5*SatoshisPerQOGE, "qDistCBExtra"))
	}
	g := testBlock(hash64("distCB0"), 100, "", coinbaseTx(hash64("distCB0tx"), coinbaseOuts...))
	mustApply(t, ctx, s, g)

	// Cross-bucket increase: merge all ten 5-QOGE UTXOs (50 QOGE) into one
	// 49 QOGE output back to qDistCB (1 QOGE fee): 5 QOGE (bucket 1_10) ->
	// 49 QOGE (bucket 10_100).
	upInputs := make([]chain.Input, 10)
	for i := uint32(0); i < 10; i++ {
		upInputs[i] = spendInput(i, hash64("distCB0tx"), i, nil)
	}
	spendUp := spendTx(hash64("distCB1tx"), hash64("distCB1tx"), 200,
		upInputs,
		[]chain.Output{out(0, 49*SatoshisPerQOGE, "qDistCB")},
	)
	a := testBlock(hash64("distCB1"), 101, hash64("distCB0"), minerCoinbase("distCB1"), spendUp)
	mustApply(t, ctx, s, a)

	if c, _ := distBucket(t, ctx, pool, "1_10"); c != 0 {
		t.Errorf("bucket 1_10 count = %d, want 0 after cross-bucket move away", c)
	}
	count, balance := distBucket(t, ctx, pool, "10_100")
	if count != 1 || balance != 49*SatoshisPerQOGE {
		t.Fatalf("bucket 10_100 = (count=%d,balance=%d), want (1,%d)", count, balance, 49*SatoshisPerQOGE)
	}

	// Cross-bucket decrease: 49 QOGE (10_100) -> 3 QOGE (1_10).
	spendDown := spendTx(hash64("distCB2tx"), hash64("distCB2tx"), 200,
		[]chain.Input{spendInput(0, hash64("distCB1tx"), 0, nil)},
		[]chain.Output{out(0, 3*SatoshisPerQOGE, "qDistCB")},
	)
	b := testBlock(hash64("distCB2"), 102, hash64("distCB1"), minerCoinbase("distCB2"), spendDown)
	mustApply(t, ctx, s, b)

	if c, _ := distBucket(t, ctx, pool, "10_100"); c != 0 {
		t.Errorf("bucket 10_100 count = %d, want 0 after cross-bucket move away", c)
	}
	count, balance = distBucket(t, ctx, pool, "1_10")
	if count != 1 || balance != 3*SatoshisPerQOGE {
		t.Fatalf("bucket 1_10 = (count=%d,balance=%d), want (1,%d)", count, balance, 3*SatoshisPerQOGE)
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_FullSpendToZero covers §18 "full spend ->
// zero": the address's addresses row is deleted (existing recomputeAddress
// behavior) and its bucket contribution is fully removed.
func TestApplyBlock_Distribution_FullSpendToZero(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("distFS0"), 100, "",
		coinbaseTx(hash64("distFS0tx"), out(0, 5*SatoshisPerQOGE, "qDistFS")),
	)
	mustApply(t, ctx, s, g)

	count, _ := distBucket(t, ctx, pool, "1_10")
	if count != 1 {
		t.Fatalf("precondition: bucket 1_10 count = %d, want 1", count)
	}

	spendAll := spendTx(hash64("distFS1tx"), hash64("distFS1tx"), 200,
		[]chain.Input{spendInput(0, hash64("distFS0tx"), 0, nil)},
		[]chain.Output{out(0, 5*SatoshisPerQOGE, "qDistFSDest")},
	)
	a := testBlock(hash64("distFS1"), 101, hash64("distFS0"), minerCoinbase("distFS1"), spendAll)
	mustApply(t, ctx, s, a)

	// qDistFS fully spent: recomputeAddress keeps its addresses row (it has
	// real history — a prior received output — so it isn't the "zero
	// remaining activity ever" case recomputeAddress deletes for; that
	// case is specific to a rolled-back CREATION, not an ordinary spend —
	// see TestRollbackTo_ReplacementBranchAndAddressRecompute), but its
	// balance goes to 0, so it contributes to NO distribution bucket.
	var cachedBalance int64
	if err := pool.QueryRow(ctx, `SELECT balance_satoshis FROM addresses WHERE address = 'qDistFS'`).Scan(&cachedBalance); err != nil {
		t.Fatalf("read qDistFS addresses row: %v", err)
	}
	if cachedBalance != 0 {
		t.Errorf("qDistFS balance_satoshis = %d, want 0 after full spend", cachedBalance)
	}
	count, balance := distBucket(t, ctx, pool, "1_10")
	if count != 1 || balance != 5*SatoshisPerQOGE {
		t.Fatalf("bucket 1_10 = (count=%d,balance=%d), want (1,%d) [only qDistFSDest]", count, balance, 5*SatoshisPerQOGE)
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_NoAddressExcluded covers §10/§18: an
// OP_RETURN (no-address) output contributes to no addresses row and
// therefore no distribution bucket.
func TestApplyBlock_Distribution_NoAddressExcluded(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("distNA"), 100, "",
		coinbaseTx(hash64("distNAtx"), nullOut(0), out(1, 1*SatoshisPerQOGE, "qDistNA")),
	)
	mustApply(t, ctx, s, block)

	count, balance := distBucket(t, ctx, pool, "1_10")
	if count != 1 || balance != 1*SatoshisPerQOGE {
		t.Fatalf("bucket 1_10 = (count=%d,balance=%d), want (1,%d) [nullOut must not appear]", count, balance, 1*SatoshisPerQOGE)
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_BareMultisigExcluded covers §10/§18: bare
// multisig participant addresses are search/display identities only and
// must never contribute to any distribution bucket.
func TestApplyBlock_Distribution_BareMultisigExcluded(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("distBM"), 100, "",
		coinbaseTx(hash64("distBMtx"),
			multisigOut(0, 7*SatoshisPerQOGE, [][]byte{{0x01}, {0x02}}, []string{"qDistBM1", "qDistBM2"}),
		),
	)
	mustApply(t, ctx, s, block)

	for _, id := range DistributionBucketIDs() {
		c, b := distBucket(t, ctx, pool, id)
		if c != 0 || b != 0 {
			t.Errorf("bucket %s = (count=%d,balance=%d), want (0,0) [bare multisig participants excluded]", id, c, b)
		}
	}
	var addrCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM addresses WHERE address IN ('qDistBM1','qDistBM2')`).Scan(&addrCount); err != nil {
		t.Fatalf("count multisig participant addresses: %v", err)
	}
	if addrCount != 0 {
		t.Fatalf("multisig participants must never get an addresses row, count = %d", addrCount)
	}
}

// TestApplyBlock_Distribution_P2QPKParticipatesNormally covers §10/§18: a
// P2QPK destination address participates exactly like any ordinary
// address.
func TestApplyBlock_Distribution_P2QPKParticipatesNormally(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	block := testBlock(hash64("distPQ"), 100, "",
		coinbaseTx(hash64("distPQtx"), p2qpkOut(0, 12*SatoshisPerQOGE)),
	)
	block.Transactions[0].Outputs[0].Address = "qDistPQAddr"
	mustApply(t, ctx, s, block)

	count, balance := distBucket(t, ctx, pool, "10_100")
	if count != 1 || balance != 12*SatoshisPerQOGE {
		t.Fatalf("bucket 10_100 = (count=%d,balance=%d), want (1,%d) [P2QPK must participate normally]", count, balance, 12*SatoshisPerQOGE)
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_Nonnegativity is §16: an accounting bug that
// would push a bucket negative must fail the transaction, never clamp. We
// force this by directly corrupting the bucket row's count to 0 and then
// causing a decrement via a full-spend-to-zero transition — the delta
// application must detect the impossible negative and reject it, and
// ApplyBlock must roll back entirely (§7 atomicity).
func TestApplyBlock_Distribution_Nonnegativity(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("distNN0"), 100, "",
		coinbaseTx(hash64("distNN0tx"), out(0, 2*SatoshisPerQOGE, "qDistNN")),
	)
	mustApply(t, ctx, s, g)

	// Corrupt the bucket row directly (bypassing the immutability trigger,
	// which only protects bucket_id/min/max — address_count/balance_satoshis
	// remain application-mutable) to simulate a state where a decrement
	// would go negative.
	if _, err := pool.Exec(ctx, `UPDATE address_balance_distribution SET address_count = 0, balance_satoshis = 0 WHERE bucket_id = '1_10'`); err != nil {
		t.Fatalf("corrupt bucket row: %v", err)
	}

	spendAll := spendTx(hash64("distNN1tx"), hash64("distNN1tx"), 200,
		[]chain.Input{spendInput(0, hash64("distNN0tx"), 0, nil)},
		[]chain.Output{nullOut(0)},
	)
	a := testBlock(hash64("distNN1"), 101, hash64("distNN0"), minerCoinbase("distNN1"), spendAll)
	err := s.ApplyBlock(ctx, a)
	if err == nil {
		t.Fatal("expected ApplyBlock to fail when a distribution delta would go negative")
	}
	if !errors.Is(err, ErrDistributionInvariant) {
		t.Fatalf("expected ErrDistributionInvariant, got: %v", err)
	}

	// Atomicity: the whole block, including sync_state, must be unchanged.
	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != hash64("distNN0") {
		t.Errorf("Tip.Hash = %s, want unchanged at %s (failed ApplyBlock must not advance the checkpoint)", tip.Hash, hash64("distNN0"))
	}
}

// ─── reorg / re-promotion ────────────────────────────────────────────────

// TestRollbackTo_Distribution_NormalReorg covers §8/§18: a rollback must
// remove the orphaned branch's contribution entirely and leave the
// distribution representing only the surviving chain.
func TestRollbackTo_Distribution_NormalReorg(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("distRB0"), 100, "",
		coinbaseTx(hash64("distRB0tx"), out(0, 5*SatoshisPerQOGE, "qDistRBFund")),
	)
	mustApply(t, ctx, s, g)

	spend := spendTx(hash64("distRB1tx"), hash64("distRB1tx"), 200,
		[]chain.Input{spendInput(0, hash64("distRB0tx"), 0, nil)},
		[]chain.Output{out(0, 4*SatoshisPerQOGE, "qDistRBOrphan")},
	)
	a := testBlock(hash64("distRB1"), 101, hash64("distRB0"), minerCoinbase("distRB1"), spend)
	mustApply(t, ctx, s, a)

	if c, _ := distBucket(t, ctx, pool, "1_10"); c != 1 {
		t.Fatalf("precondition: bucket 1_10 count = %d, want 1 (qDistRBOrphan)", c)
	}

	if err := s.RollbackTo(ctx, hash64("distRB0")); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}

	var orphanRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM addresses WHERE address = 'qDistRBOrphan'`).Scan(&orphanRows); err != nil {
		t.Fatalf("count orphan address: %v", err)
	}
	if orphanRows != 0 {
		t.Errorf("qDistRBOrphan should have no addresses row after rollback")
	}
	// The rolled-back spend is undone, so qDistRBFund's original 5 QOGE
	// output becomes unspent again — the bucket must show ONLY that
	// restored balance, not qDistRBOrphan (no orphan leakage).
	if c, b := distBucket(t, ctx, pool, "1_10"); c != 1 || b != 5*SatoshisPerQOGE {
		t.Errorf("bucket 1_10 after rollback = (count=%d,balance=%d), want (1,%d) [qDistRBFund restored, no orphan leakage]",
			c, b, 5*SatoshisPerQOGE)
	}

	height, hash := distState(t, ctx, pool)
	if height != 100 || hash == nil || *hash != hash64("distRB0") {
		t.Fatalf("distribution state after rollback = (%d,%v), want (100,%s)", height, hash, hash64("distRB0"))
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}

// TestApplyBlock_Distribution_OrphanRePromotion is §9's exact sequence:
// G, G->A1, rollback G, G->B1, rollback G, reapply EXACT SAME persisted
// A1. The distribution must return exactly to A1's state — no doubled
// counts/balances, no B leakage.
func TestApplyBlock_Distribution_OrphanRePromotion(t *testing.T) {
	ctx := context.Background()
	s, pool := newTestStore(t)

	g := testBlock(hash64("distRP0"), 100, "",
		coinbaseTx(hash64("distRP0tx"), out(0, 5*SatoshisPerQOGE, "qDistRPFund")),
	)
	mustApply(t, ctx, s, g)

	spendA := spendTx(hash64("distRPA1tx"), hash64("distRPA1tx"), 200,
		[]chain.Input{spendInput(0, hash64("distRP0tx"), 0, nil)},
		[]chain.Output{out(0, 4*SatoshisPerQOGE, "qDistRPA")},
	)
	a1 := testBlock(hash64("distRPA1"), 101, hash64("distRP0"), minerCoinbase("distRPA1"), spendA)
	mustApply(t, ctx, s, a1)

	countA, balanceA := distBucket(t, ctx, pool, "1_10")
	if countA != 1 || balanceA != 4*SatoshisPerQOGE {
		t.Fatalf("after A1: bucket 1_10 = (count=%d,balance=%d), want (1,%d)", countA, balanceA, 4*SatoshisPerQOGE)
	}

	if err := s.RollbackTo(ctx, hash64("distRP0")); err != nil {
		t.Fatalf("rollback to genesis (orphan A1): %v", err)
	}

	spendB := spendTx(hash64("distRPB1tx"), hash64("distRPB1tx"), 200,
		[]chain.Input{spendInput(0, hash64("distRP0tx"), 0, nil)},
		[]chain.Output{out(0, 2*SatoshisPerQOGE, "qDistRPB")},
	)
	b1 := testBlock(hash64("distRPB1"), 101, hash64("distRP0"), minerCoinbase("distRPB1"), spendB)
	mustApply(t, ctx, s, b1)

	countB, balanceB := distBucket(t, ctx, pool, "1_10")
	if countB != 1 || balanceB != 2*SatoshisPerQOGE {
		t.Fatalf("after B1: bucket 1_10 = (count=%d,balance=%d), want (1,%d) [only B]", countB, balanceB, 2*SatoshisPerQOGE)
	}

	if err := s.RollbackTo(ctx, hash64("distRP0")); err != nil {
		t.Fatalf("rollback to genesis (orphan B1): %v", err)
	}

	// Re-apply the EXACT SAME already-persisted A1 block — the real orphan
	// re-promotion path.
	if err := s.ApplyBlock(ctx, a1); err != nil {
		t.Fatalf("re-apply persisted A1 (orphan re-promotion): %v", err)
	}

	countFinal, balanceFinal := distBucket(t, ctx, pool, "1_10")
	if countFinal != 1 || balanceFinal != 4*SatoshisPerQOGE {
		t.Fatalf("after A1 re-promotion: bucket 1_10 = (count=%d,balance=%d), want (1,%d) [exactly A1, no doubling, no B leakage]",
			countFinal, balanceFinal, 4*SatoshisPerQOGE)
	}

	height, hash := distState(t, ctx, pool)
	if height != 101 || hash == nil || *hash != hash64("distRPA1") {
		t.Fatalf("distribution state after re-promotion = (%d,%v), want (101,%s)", height, hash, hash64("distRPA1"))
	}

	var bRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM addresses WHERE address = 'qDistRPB'`).Scan(&bRows); err != nil {
		t.Fatalf("count qDistRPB: %v", err)
	}
	if bRows != 0 {
		t.Errorf("qDistRPB should have no addresses row after being orphaned, count = %d", bRows)
	}
	requireBucketsSumToAddresses(t, ctx, pool)
}
