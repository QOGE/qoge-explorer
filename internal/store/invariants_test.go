package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/jackc/pgx/v5"
)

// txPool returns a fully-migrated database wrapped in a transaction that is
// always rolled back on test cleanup, so fixture rows never leak between
// tests or outlive the test run regardless of pass/fail.
func txPool(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	pool := migratedPool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	return ctx, tx
}

func mustExec(t *testing.T, ctx context.Context, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// fixtureBlock inserts a minimal valid canonical block, so invariant tests
// below aren't each responsible for satisfying every NOT NULL column by
// hand.
func fixtureBlock(t *testing.T, ctx context.Context, tx pgx.Tx, hash string, height int64, prevHash *string) {
	t.Helper()
	mustExec(t, ctx, tx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, $2, $3, $1, 1700000000, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, hash, height, prevHash)
}

func fixtureTransaction(t *testing.T, ctx context.Context, tx pgx.Tx, txid string, isCoinbase bool) {
	t.Helper()
	mustExec(t, ctx, tx, `
		INSERT INTO transactions (txid, version, locktime, size, vsize, weight, is_coinbase)
		VALUES ($1, 2, 0, 100, 100, 400, $2)
	`, txid, isCoinbase)
}

// hash64 turns a short readable label into a syntactically valid
// 64-lowercase-hex-char hash so each test's fixtures are easy to read while
// still satisfying the hash-format CHECK constraints (which require
// [0-9a-f]{64} — a plain English label like "blocka" contains letters
// outside a-f and would be rejected as malformed, not merely "a label").
func hash64(label string) string {
	h := hex.EncodeToString([]byte(label))
	if len(h) >= 64 {
		return h[:64]
	}
	return h + strings.Repeat("0", 64-len(h))
}

// TestInvariant_DuplicateCanonicalHeightRejected is invariant test D.
func TestInvariant_DuplicateCanonicalHeightRejected(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)

	_, err := tx.Exec(ctx, `
		INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		VALUES ($1, 100, NULL, $1, 1700000001, '1d00ffff', 1.0, 0, 100, 400, 1)
	`, hash64("blockb"))
	if err == nil {
		t.Fatal("expected an error inserting a second canonical block at the same height, got nil")
	}
}

// TestInvariant_SameTxidTwoBlocks is invariant test E: the same txid can be
// associated with two different block hashes through block_transactions —
// the whole point of separating transaction identity from transaction
// occurrence (docs/ARCHITECTURE.md).
func TestInvariant_SameTxidTwoBlocks(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil) // independent height; competing-fork detail is irrelevant to this invariant
	fixtureTransaction(t, ctx, tx, hash64("txshared"), false)

	mustExec(t, ctx, tx, `INSERT INTO block_transactions (block_hash, tx_index, txid) VALUES ($1, 0, $2)`,
		hash64("blocka"), hash64("txshared"))
	mustExec(t, ctx, tx, `INSERT INTO block_transactions (block_hash, tx_index, txid) VALUES ($1, 0, $2)`,
		hash64("blockb"), hash64("txshared"))

	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM block_transactions WHERE txid = $1`, hash64("txshared")).Scan(&count); err != nil {
		t.Fatalf("count block_transactions: %v", err)
	}
	if count != 2 {
		t.Errorf("expected the same txid linked to 2 blocks, got %d", count)
	}

	// The trigger-derived block_height must match each block's real height,
	// not just whatever was (not) supplied.
	var h int64
	if err := tx.QueryRow(ctx, `SELECT block_height FROM block_transactions WHERE block_hash = $1`, hash64("blocka")).Scan(&h); err != nil {
		t.Fatalf("query derived block_height: %v", err)
	}
	if h != 100 {
		t.Errorf("block_transactions.block_height for blocka = %d, want 100 (trigger-derived)", h)
	}
}

// TestInvariant_WitnessBYTEAPreservesLargeSignature is invariant test F:
// raw witness BYTEA preserves a 17,088-byte P2QPK signature exactly.
// This is a database fixture only — no transaction was broadcast or
// manufactured on any network; the byte content is synthetic filler.
func TestInvariant_WitnessBYTEAPreservesLargeSignature(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txqpk"), false)
	mustExec(t, ctx, tx, `
		INSERT INTO transaction_inputs (txid, vin_index, prev_txid, prev_vout_index, script_sig, sequence)
		VALUES ($1, 0, $2, 0, ''::bytea, 4294967295)
	`, hash64("txqpk"), hash64("txprev"))

	sig := make([]byte, script.P2QPKSignatureLength) // 17,088 bytes, per docs/ARCHITECTURE.md §9
	for i := range sig {
		sig[i] = byte(i % 251)
	}
	pubkey := make([]byte, script.P2QPKPublicKeyLength) // 32 bytes
	for i := range pubkey {
		pubkey[i] = byte(200 + i)
	}

	mustExec(t, ctx, tx, `INSERT INTO transaction_input_witness (txid, vin_index, item_index, data) VALUES ($1, 0, 0, $2)`,
		hash64("txqpk"), sig)
	mustExec(t, ctx, tx, `INSERT INTO transaction_input_witness (txid, vin_index, item_index, data) VALUES ($1, 0, 1, $2)`,
		hash64("txqpk"), pubkey)

	var gotSig, gotPubkey []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE txid=$1 AND vin_index=0 AND item_index=0`, hash64("txqpk")).Scan(&gotSig); err != nil {
		t.Fatalf("read back signature: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE txid=$1 AND vin_index=0 AND item_index=1`, hash64("txqpk")).Scan(&gotPubkey); err != nil {
		t.Fatalf("read back pubkey: %v", err)
	}

	if len(gotSig) != script.P2QPKSignatureLength {
		t.Fatalf("read-back signature length = %d, want %d", len(gotSig), script.P2QPKSignatureLength)
	}
	if !bytes.Equal(gotSig, sig) {
		t.Error("read-back signature bytes do not match exactly what was written")
	}
	if !bytes.Equal(gotPubkey, pubkey) {
		t.Error("read-back pubkey bytes do not match exactly what was written")
	}
}

// TestInvariant_ZeroValueNullDataOutputPreserved is invariant test G:
// transaction outputs preserve zero-value OP_RETURN outputs — the exact
// eIquidus behavior (dropping them) this schema deliberately avoids.
func TestInvariant_ZeroValueNullDataOutputPreserved(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txnulldata"), true)

	mustExec(t, ctx, tx, `
		INSERT INTO transaction_outputs (txid, vout_index, value_satoshis, script_pubkey, script_type)
		VALUES ($1, 0, 0, $2, 'nulldata')
	`, hash64("txnulldata"), []byte{0x6a, 0x04, 0xde, 0xad, 0xbe, 0xef})

	var value int64
	var scriptType string
	if err := tx.QueryRow(ctx, `SELECT value_satoshis, script_type FROM transaction_outputs WHERE txid=$1 AND vout_index=0`, hash64("txnulldata")).Scan(&value, &scriptType); err != nil {
		t.Fatalf("read back nulldata output: %v", err)
	}
	if value != 0 {
		t.Errorf("value_satoshis = %d, want 0", value)
	}
	if scriptType != string(script.TypeNullData) {
		t.Errorf("script_type = %s, want %s", scriptType, script.TypeNullData)
	}
}

// TestInvariant_MultisigParticipantsNeverCreateDestinationRows is
// invariant test H: bare multisig participants do not create
// destination-address balance rows. output_participants insertion must
// succeed; an output_addresses insertion for the same multisig output must
// be rejected by the trigger.
func TestInvariant_MultisigParticipantsNeverCreateDestinationRows(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txmultisig"), false)
	mustExec(t, ctx, tx, `
		INSERT INTO transaction_outputs (txid, vout_index, value_satoshis, script_pubkey, script_type)
		VALUES ($1, 0, 500000000, $2, 'multisig')
	`, hash64("txmultisig"), []byte{0x52, 0x21, 0x02})

	// Participant rows: allowed.
	mustExec(t, ctx, tx, `INSERT INTO output_participants (txid, vout_index, address, pubkey) VALUES ($1, 0, 'bqParticipant1', $2)`,
		hash64("txmultisig"), []byte{0x02, 0x01})
	mustExec(t, ctx, tx, `INSERT INTO output_participants (txid, vout_index, address, pubkey) VALUES ($1, 0, 'bqParticipant2', $2)`,
		hash64("txmultisig"), []byte{0x02, 0x02})

	var participantCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM output_participants WHERE txid=$1 AND vout_index=0`, hash64("txmultisig")).Scan(&participantCount); err != nil {
		t.Fatalf("count participants: %v", err)
	}
	if participantCount != 2 {
		t.Errorf("participant count = %d, want 2", participantCount)
	}

	// Destination row for the same multisig output: must be rejected.
	_, err := tx.Exec(ctx, `INSERT INTO output_addresses (txid, vout_index, address) VALUES ($1, 0, 'bqParticipant1')`,
		hash64("txmultisig"))
	if err == nil {
		t.Fatal("expected output_addresses insert for a multisig output to be rejected, got nil error")
	}
}

// TestInvariant_MalformedScriptTypeRejected is invariant test I.
func TestInvariant_MalformedScriptTypeRejected(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txbadtype"), false)

	_, err := tx.Exec(ctx, `
		INSERT INTO transaction_outputs (txid, vout_index, value_satoshis, script_pubkey, script_type)
		VALUES ($1, 0, 100, $2, 'not_a_real_script_type')
	`, hash64("txbadtype"), []byte{0x00})
	if err == nil {
		t.Fatal("expected an error inserting a malformed script_type, got nil")
	}
}

// TestInvariant_UninitializedSyncStateValid is invariant test J: the
// bootstrap sync_state row (height=-1, hash=NULL) inserted by the up
// migration is itself a valid, queryable row satisfying every constraint.
func TestInvariant_UninitializedSyncStateValid(t *testing.T) {
	ctx, tx := txPool(t)

	var height int64
	var hash *string
	if err := tx.QueryRow(ctx, `SELECT indexed_height, indexed_block_hash FROM sync_state WHERE name='main'`).Scan(&height, &hash); err != nil {
		t.Fatalf("read bootstrap sync_state row: %v", err)
	}
	if height != -1 {
		t.Errorf("bootstrap indexed_height = %d, want -1", height)
	}
	if hash != nil {
		t.Errorf("bootstrap indexed_block_hash = %v, want NULL", *hash)
	}

	// Also confirm a second, explicitly-constructed uninitialized row is
	// itself valid (not just the migration's own INSERT).
	mustExec(t, ctx, tx, `INSERT INTO sync_state (name, indexed_height, indexed_block_hash) VALUES ('other', -1, NULL)`)
}

// TestInvariant_ContradictorySyncStateRejected is invariant test K.
//
// Each contradictory case is checked in its own transaction: after a
// constraint violation, Postgres aborts the current transaction and
// refuses further statements on it ("current transaction is aborted")
// until rollback — reusing one transaction for both cases would make the
// second check fail for the wrong reason rather than actually re-testing
// the second half of the CHECK constraint.
func TestInvariant_ContradictorySyncStateRejected(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	t.Run("height=-1 with a non-NULL hash", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)

		_, err = tx.Exec(ctx, `UPDATE sync_state SET indexed_height = -1, indexed_block_hash = $1 WHERE name='main'`, hash64("bogus"))
		if err == nil {
			t.Fatal("expected height=-1 with a non-NULL hash to be rejected, got nil error")
		}
	})

	t.Run("height>=0 with a NULL hash", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)

		_, err = tx.Exec(ctx, `UPDATE sync_state SET indexed_height = 5, indexed_block_hash = NULL WHERE name='main'`)
		if err == nil {
			t.Fatal("expected height=5 with a NULL hash to be rejected, got nil error")
		}
	})
}
