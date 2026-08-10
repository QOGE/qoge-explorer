package decode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/rpc"
	"github.com/QOGE/qoge-explorer/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file is a decoder -> Store PIPELINE test only (task item 17): a
// typed RPC fixture, decoded via DecodeBlock, applied via
// Store.ApplyBlock, verified against real PostgreSQL. It deliberately does
// NOT implement any polling/fetch loop, height iteration, or reorg
// handling — that is explicitly out of scope for Phase 2C.1 (task item 17
// / "PHASE BOUNDARY").

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("QOGE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("QOGE_TEST_DATABASE_URL not set; skipping PostgreSQL-backed test")
	}
	return url
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random schema suffix: %v", err)
	}
	return hex.EncodeToString(b)
}

// migratedStorePool creates a fresh, uniquely-named, isolated PostgreSQL
// schema (never `public`, never any pre-existing schema), applies every
// migration to it, and returns both a *store.Store and the raw pool for
// direct assertion queries. Mirrors internal/store/dbtest_test.go's own
// pattern; duplicated here (rather than imported) because that helper is
// unexported and this is a different package.
func migratedStorePool(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	baseURL := testDatabaseURL(t)

	schemaName := "qoge_decode_test_" + randomHex(t, 12)
	identifier := pgx.Identifier{schemaName}.Sanitize()

	setupConn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect to create test schema: %v", err)
	}
	_, err = setupConn.Exec(ctx, "CREATE SCHEMA "+identifier)
	closeErr := setupConn.Close(ctx)
	if err != nil {
		t.Fatalf("create test schema %s: %v", schemaName, err)
	}
	if closeErr != nil {
		t.Fatalf("close schema-setup connection: %v", closeErr)
	}

	poolCfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+identifier)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("open pool for test schema %s: %v", schemaName, err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx := context.Background()
		dropConn, err := pgx.Connect(dropCtx, baseURL)
		if err != nil {
			t.Logf("warning: could not connect to drop test schema %s: %v", schemaName, err)
			return
		}
		defer dropConn.Close(dropCtx)
		if _, err := dropConn.Exec(dropCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Logf("warning: could not drop test schema %s: %v", schemaName, err)
		}
	})

	migrations, err := store.LoadMigrations(os.DirFS("../../migrations"))
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := store.Up(ctx, pool, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return store.New(pool), pool
}

func TestDecodeBlock_ThroughStore_PipelineTest(t *testing.T) {
	ctx := context.Background()
	s, pool := migratedStorePool(t)

	p2pkPubKeyHex := "029f94e03d2ba37bda673eb132687705ac284d380478d63cbce0c19e2f0bd597cd"
	p2pkScriptHex := "21" + p2pkPubKeyHex + "ac"
	resolver := newFakeResolver(map[string]string{p2pkPubKeyHex: "qPipelineP2PKAddress"})

	// ─── Block 0 (real genesis, height 0): a trivial anchor block. Its
	// output is intentionally NOT the one this test exercises spendability
	// against — Store's genesis-height UTXO exclusion (§16) would make it
	// a poor vector for that — this block exists only so block 1 below can
	// carry a real, FK-satisfying previousblockhash (the decoder's new
	// genesis/previousblockhash relation, task item 4 this round, requires
	// a genuine parent hash for any height > 0). ───

	rawAnchor := rawBlockFixture(rawHash("pipelineAnchor"), 0, "", rawCoinbaseTx(rawHash("pipelineAnchorTx"), rawP2PKHVout(0, "1", "qPipelineAnchor")))
	anchorBlock, err := DecodeBlock(ctx, rawAnchor, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock (anchor genesis block): %v", err)
	}
	if err := s.ApplyBlock(ctx, anchorBlock); err != nil {
		t.Fatalf("ApplyBlock (anchor genesis block): %v", err)
	}

	// ─── Block 1 (height 1, non-genesis): a coinbase with a P2PK output
	// (Core omits an address; the resolver supplies one), an OP_RETURN
	// output, and a present-but-empty scriptPubKey output (legitimate Core
	// data — task item 2). ───

	genTxID := rawHash("pipelineGenTx")
	rawGen := rawBlockFixture(rawHash("pipelineGen"), 1, *rawAnchor.Hash, rpc.RawTransaction{
		TxID: strPtr(genTxID), Hash: strPtr(genTxID),
		Version: uint32Ptr(2), Size: intPtr(100), VSize: intPtr(100), Weight: intPtr(400), LockTime: uint32Ptr(0),
		Vin: []rpc.RawVin{{Coinbase: strPtr("51"), Sequence: uint32Ptr(4294967295)}},
		Vout: []rpc.RawVout{
			{Value: "10", N: uint32Ptr(0), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr(p2pkScriptHex)}},
			{Value: "0", N: uint32Ptr(1), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr("6a04deadbeef"), Type: "nulldata"}},
			{Value: "0.5", N: uint32Ptr(2), ScriptPubKey: &rpc.RawScriptPubKey{Hex: strPtr("")}}, // present, legitimately empty
		},
	})

	genBlock, err := DecodeBlock(ctx, rawGen, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock (genesis-like block): %v", err)
	}
	if err := s.ApplyBlock(ctx, genBlock); err != nil {
		t.Fatalf("ApplyBlock (genesis-like block): decoded block failed Store's shape requirements: %v", err)
	}

	// Exact values survive.
	var p2pkValue int64
	var p2pkScriptType string
	if err := pool.QueryRow(ctx, `SELECT value_satoshis, script_type FROM transaction_outputs WHERE txid = $1 AND vout_index = 0`, genTxID).
		Scan(&p2pkValue, &p2pkScriptType); err != nil {
		t.Fatalf("read back P2PK output: %v", err)
	}
	if p2pkValue != 1_000_000_000 {
		t.Errorf("P2PK output value_satoshis = %d, want 1000000000 (10 QOGE)", p2pkValue)
	}
	if p2pkScriptType != "p2pk" {
		t.Errorf("P2PK output script_type = %s, want p2pk", p2pkScriptType)
	}

	// P2PK resolved address reaches output_addresses.
	var resolvedAddr string
	if err := pool.QueryRow(ctx, `SELECT address FROM output_addresses WHERE txid = $1 AND vout_index = 0`, genTxID).Scan(&resolvedAddr); err != nil {
		t.Fatalf("read back resolved P2PK output_addresses row: %v", err)
	}
	if resolvedAddr != "qPipelineP2PKAddress" {
		t.Errorf("output_addresses.address = %s, want qPipelineP2PKAddress", resolvedAddr)
	}

	// The P2PK output is an ordinary spendable coin at a non-genesis
	// height (unlike the OP_RETURN output below).
	p2pkUTXO, err := s.GetUTXO(ctx, genTxID, 0)
	if err != nil {
		t.Fatalf("GetUTXO (P2PK): %v", err)
	}
	if p2pkUTXO == nil {
		t.Error("expected a UTXO for the P2PK output, got nil")
	}

	// OP_RETURN survives transaction_outputs but not utxo_state.
	var opReturnValue int64
	var opReturnScriptType string
	if err := pool.QueryRow(ctx, `SELECT value_satoshis, script_type FROM transaction_outputs WHERE txid = $1 AND vout_index = 1`, genTxID).
		Scan(&opReturnValue, &opReturnScriptType); err != nil {
		t.Fatalf("read back OP_RETURN output: %v", err)
	}
	if opReturnValue != 0 || opReturnScriptType != "nulldata" {
		t.Errorf("OP_RETURN output = value %d type %s, want value 0 type nulldata", opReturnValue, opReturnScriptType)
	}
	opReturnUTXO, err := s.GetUTXO(ctx, genTxID, 1)
	if err != nil {
		t.Fatalf("GetUTXO (OP_RETURN): %v", err)
	}
	if opReturnUTXO != nil {
		t.Errorf("GetUTXO for the OP_RETURN output = %+v, want nil", opReturnUTXO)
	}

	// Present-but-empty scriptPubKey: transaction_outputs row persists
	// with a zero-length script, and — since an empty script is not
	// IsUnspendable (it doesn't start with OP_RETURN, and isn't
	// oversized) and this isn't the genesis height — it gets an ordinary
	// utxo_state row, exactly like any other spendable output. This is
	// Store's existing, unmodified UTXO semantics; the decoder changes
	// nothing here, it just correctly refrains from rejecting a
	// legitimately empty script as "missing."
	var emptyScript []byte
	var emptyScriptType string
	if err := pool.QueryRow(ctx, `SELECT script_pubkey, script_type FROM transaction_outputs WHERE txid = $1 AND vout_index = 2`, genTxID).
		Scan(&emptyScript, &emptyScriptType); err != nil {
		t.Fatalf("read back empty-scriptPubKey output: %v", err)
	}
	if len(emptyScript) != 0 {
		t.Errorf("empty-scriptPubKey output script_pubkey = %x, want zero-length", emptyScript)
	}
	if emptyScriptType != "unknown" {
		t.Errorf("empty-scriptPubKey output script_type = %s, want unknown", emptyScriptType)
	}
	emptyScriptUTXO, err := s.GetUTXO(ctx, genTxID, 2)
	if err != nil {
		t.Fatalf("GetUTXO (empty scriptPubKey): %v", err)
	}
	if emptyScriptUTXO == nil {
		t.Error("expected a UTXO for the present-but-empty-script output (not genesis, not IsUnspendable), got nil")
	}

	// ─── Block 2 (height 2): spends the P2PK output with a 17,088-byte +
	// 32-byte P2QPK-shaped witness. ───

	childCoinbaseTxID := rawHash("pipelineChildCB")
	spendTxID := rawHash("pipelineSpendTxID")
	spendWTxID := rawHash("pipelineSpendWTxID") // hash != txid: this transaction carries witness data
	sigHex := strings.Repeat("ab", 17088)
	pubHex := strings.Repeat("ef", 32)

	rawChild := rawBlockFixture(rawHash("pipelineChild"), 2, *rawGen.Hash,
		rpc.RawTransaction{
			TxID: strPtr(childCoinbaseTxID), Hash: strPtr(childCoinbaseTxID),
			Version: uint32Ptr(2), Size: intPtr(100), VSize: intPtr(100), Weight: intPtr(400), LockTime: uint32Ptr(0),
			Vin:  []rpc.RawVin{{Coinbase: strPtr("52"), Sequence: uint32Ptr(4294967295)}},
			Vout: []rpc.RawVout{rawP2PKHVout(0, "1", "qPipelineMiner")},
		},
		rpc.RawTransaction{
			TxID: strPtr(spendTxID), Hash: strPtr(spendWTxID),
			Version: uint32Ptr(2), Size: intPtr(17300), VSize: intPtr(4400), Weight: intPtr(17700), LockTime: uint32Ptr(0),
			Vin: []rpc.RawVin{{
				TxID: strPtr(genTxID), Vout: uint32Ptr(0),
				ScriptSig:   &rpc.RawScriptSig{Hex: strPtr("")},
				Sequence:    uint32Ptr(4294967295),
				TxInWitness: []string{sigHex, pubHex},
			}},
			Vout: []rpc.RawVout{rawP2PKHVout(0, "9.99", "qPipelineSpendDest")},
		},
	)

	childBlock, err := DecodeBlock(ctx, rawChild, resolver)
	if err != nil {
		t.Fatalf("DecodeBlock (child block): %v", err)
	}
	if err := s.ApplyBlock(ctx, childBlock); err != nil {
		t.Fatalf("ApplyBlock (child block): %v", err)
	}

	// P2QPK-shaped (17,088-byte + 32-byte) witness reaches
	// transaction_input_witness exactly.
	var sigData, pubData []byte
	if err := pool.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = 0 AND item_index = 0`, spendWTxID).Scan(&sigData); err != nil {
		t.Fatalf("read back witness item 0: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = 0 AND item_index = 1`, spendWTxID).Scan(&pubData); err != nil {
		t.Fatalf("read back witness item 1: %v", err)
	}
	if len(sigData) != 17088 {
		t.Errorf("witness item 0 length = %d, want 17088", len(sigData))
	}
	if len(pubData) != 32 {
		t.Errorf("witness item 1 length = %d, want 32", len(pubData))
	}
	wantSig := mustHexDecode(t, sigHex)
	wantPub := mustHexDecode(t, pubHex)
	if string(sigData) != string(wantSig) {
		t.Error("witness item 0 did not survive the full RPC->decode->Store pipeline byte-exact")
	}
	if string(pubData) != string(wantPub) {
		t.Error("witness item 1 did not survive the full RPC->decode->Store pipeline byte-exact")
	}

	spentUTXO, err := s.GetUTXO(ctx, genTxID, 0)
	if err != nil {
		t.Fatalf("GetUTXO after spend: %v", err)
	}
	if spentUTXO == nil || !spentUTXO.Spent || spentUTXO.SpendingTxID != spendTxID {
		t.Errorf("P2PK output should now be spent by %s, got %+v", spendTxID, spentUTXO)
	}

	tip, err := s.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip.Hash != *rawChild.Hash || tip.Height != 2 {
		t.Errorf("Tip = %+v, want hash=%s height=2", tip, *rawChild.Hash)
	}
}
