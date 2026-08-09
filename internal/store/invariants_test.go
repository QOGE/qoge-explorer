package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
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

// fixtureBlockTransaction records that txid occurred in blockHash at
// tx_index — the occurrence link the utxo_state relational-integrity FKs
// (task item 3) validate against.
func fixtureBlockTransaction(t *testing.T, ctx context.Context, tx pgx.Tx, blockHash, txid string, txIndex int) {
	t.Helper()
	mustExec(t, ctx, tx, `INSERT INTO block_transactions (block_hash, tx_index, txid) VALUES ($1, $2, $3)`,
		blockHash, txIndex, txid)
}

// fixtureTransactionInput inserts a minimal non-coinbase input, so
// utxo_state's spending-input FK has something real to reference.
func fixtureTransactionInput(t *testing.T, ctx context.Context, tx pgx.Tx, txid string, vinIndex int, prevTxid string, prevVout int) {
	t.Helper()
	mustExec(t, ctx, tx, `
		INSERT INTO transaction_inputs (txid, vin_index, prev_txid, prev_vout_index, script_sig, sequence)
		VALUES ($1, $2, $3, $4, ''::bytea, 4294967295)
	`, txid, vinIndex, prevTxid, prevVout)
}

// fixtureTransactionOutput inserts a minimal, legacy (non-witness) output —
// the common case most invariant tests below just need to exist so other
// FKs (output_addresses, utxo_state) have something to reference.
func fixtureTransactionOutput(t *testing.T, ctx context.Context, tx pgx.Tx, txid string, voutIndex int, scriptType string, value int64) {
	t.Helper()
	mustExec(t, ctx, tx, `
		INSERT INTO transaction_outputs (txid, vout_index, value_satoshis, script_pubkey, script_type)
		VALUES ($1, $2, $3, $4, $5)
	`, txid, voutIndex, value, []byte{0x00}, scriptType)
}

func intPtr(v int) *int { return &v }

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

// ─── PR #2 review fixes ─────────────────────────────────────────────────

// TestInvariant_WitnessMetadataConsistency is item 1: the P2QPK CHECK NULL
// loophole (script_type='p2qpk' with witness_version/witness_program both
// NULL previously passed, because a CHECK expression that evaluates to
// NULL is treated as satisfied by Postgres) plus structural
// version/length consistency for every known witness script_type.
func TestInvariant_WitnessMetadataConsistency(t *testing.T) {
	pool := migratedPool(t)

	tests := []struct {
		name           string
		scriptType     string
		witnessVersion *int
		witnessProgram []byte
		wantErr        bool
	}{
		// The exact NULL-loophole scenario named in the review, plus the
		// three other required near-misses, plus the one case that must
		// succeed.
		{"p2qpk + NULL/NULL rejected", "p2qpk", nil, nil, true},
		{"p2qpk + v1/32 rejected", "p2qpk", intPtr(1), make([]byte, 32), true},
		{"p2qpk + v2/31 rejected", "p2qpk", intPtr(2), make([]byte, 31), true},
		{"p2qpk + v2/33 rejected", "p2qpk", intPtr(2), make([]byte, 33), true},
		{"p2qpk + v2/32 accepted", "p2qpk", intPtr(2), make([]byte, 32), false},

		// Structural consistency for every other known witness type.
		{"p2wpkh + v0/20 accepted", "p2wpkh", intPtr(0), make([]byte, 20), false},
		{"p2wpkh + v0/32 rejected (wrong length)", "p2wpkh", intPtr(0), make([]byte, 32), true},
		{"p2wpkh + NULL/NULL rejected (missing metadata)", "p2wpkh", nil, nil, true},
		{"p2wsh + v0/32 accepted", "p2wsh", intPtr(0), make([]byte, 32), false},
		{"p2wsh + v0/20 rejected (wrong length)", "p2wsh", intPtr(0), make([]byte, 20), true},
		{"p2tr + v1/32 accepted", "p2tr", intPtr(1), make([]byte, 32), false},
		{"p2tr + v0/32 rejected (wrong version)", "p2tr", intPtr(0), make([]byte, 32), true},
		{"unknown_witness + v3/25 accepted", "unknown_witness", intPtr(3), make([]byte, 25), false},
		{"unknown_witness + NULL/NULL rejected (must carry nonzero version)", "unknown_witness", nil, nil, true},
		{"unknown_witness + v0/25 rejected (version must be >0)", "unknown_witness", intPtr(0), make([]byte, 25), true},

		// Legacy (non-witness) types must carry no witness metadata at all.
		{"p2pkh + NULL/NULL accepted", "p2pkh", nil, nil, false},
		{"p2pkh + v0/20 rejected (legacy type must not carry witness metadata)", "p2pkh", intPtr(0), make([]byte, 20), true},
		{"p2pk + NULL/NULL accepted", "p2pk", nil, nil, false},
		{"nulldata + NULL/NULL accepted", "nulldata", nil, nil, false},
		{"multisig + NULL/NULL accepted", "multisig", nil, nil, false},

		// unknown: exactly two legitimate shapes (no witness program at
		// all, or an off-length witness-v0 program) — anything else,
		// including a length/version combo that actually belongs to a
		// named type, is rejected.
		{"unknown + NULL/NULL accepted", "unknown", nil, nil, false},
		{"unknown + v0/25 accepted (off-length v0)", "unknown", intPtr(0), make([]byte, 25), false},
		{"unknown + v0/20 rejected (that length is always p2wpkh)", "unknown", intPtr(0), make([]byte, 20), true},
		{"unknown + v0/32 rejected (that length is always p2wsh)", "unknown", intPtr(0), make([]byte, 32), true},
		{"unknown + v1/32 rejected (nonzero version belongs to p2tr/unknown_witness)", "unknown", intPtr(1), make([]byte, 32), true},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)

			blockHash := hash64(fmt.Sprintf("wmblock%d", i))
			txid := hash64(fmt.Sprintf("wmtx%d", i))
			fixtureBlock(t, ctx, tx, blockHash, int64(1000+i), nil)
			fixtureTransaction(t, ctx, tx, txid, false)

			_, err = tx.Exec(ctx, `
				INSERT INTO transaction_outputs (txid, vout_index, value_satoshis, script_pubkey, script_type, witness_version, witness_program)
				VALUES ($1, 0, 1000, $2, $3, $4, $5)
			`, txid, []byte{0x00}, tt.scriptType, tt.witnessVersion, tt.witnessProgram)

			if tt.wantErr && err == nil {
				t.Errorf("expected rejection, got success")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected success, got error: %v", err)
			}
		})
	}
}

// TestInvariant_OneDestinationAddressPerOutput is item 2: output_addresses
// permits at most one row per (txid, vout_index) — PRIMARY KEY (txid,
// vout_index), not (txid, vout_index, address) — so even an ordinary,
// non-multisig output can't have its value multiply-credited across two
// different addresses.
func TestInvariant_OneDestinationAddressPerOutput(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txnormal"), false)
	fixtureTransactionOutput(t, ctx, tx, hash64("txnormal"), 0, "p2pkh", 5000)

	mustExec(t, ctx, tx, `INSERT INTO output_addresses (txid, vout_index, address) VALUES ($1, 0, 'bq1qfirstdestination')`,
		hash64("txnormal"))

	_, err := tx.Exec(ctx, `INSERT INTO output_addresses (txid, vout_index, address) VALUES ($1, 0, 'bq1qseconddestination')`,
		hash64("txnormal"))
	if err == nil {
		t.Fatal("expected a second, different destination address for the same output to be rejected, got nil error")
	}
}

// TestInvariant_UTXOCreationOccurrenceFKRejectsWrongBlock is item 3, case
// "wrong creation block for txid -> rejected": the output's transaction
// really occurred in blocka (via block_transactions), but the utxo_state
// row claims it was created in blockb, which never contained that txid.
func TestInvariant_UTXOCreationOccurrenceFKRejectsWrongBlock(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
	fixtureTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	_, err := tx.Exec(ctx, `
		INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height)
		VALUES ($1, 0, $2, 101)
	`, hash64("txcreate"), hash64("blockb"))
	if err == nil {
		t.Fatal("expected utxo_state claiming the wrong creation block to be rejected, got nil error")
	}
}

// TestInvariant_UTXOSpendingInputFKRejectsNonexistentInput is item 3, case
// "nonexistent spending input -> rejected": spending_txid/spending_vin_index
// claim a transaction_inputs row that was never inserted.
func TestInvariant_UTXOSpendingInputFKRejectsNonexistentInput(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
	fixtureTransaction(t, ctx, tx, hash64("txspend"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txspend"), 0)
	// Deliberately no transaction_inputs row for (txspend, vin_index=0).

	_, err := tx.Exec(ctx, `
		INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height,
			spent, spending_txid, spending_vin_index, spending_block_hash, spending_block_height)
		VALUES ($1, 0, $2, 100, true, $3, 0, $4, 101)
	`, hash64("txcreate"), hash64("blocka"), hash64("txspend"), hash64("blockb"))
	if err == nil {
		t.Fatal("expected utxo_state claiming a nonexistent spending input to be rejected, got nil error")
	}
}

// TestInvariant_UTXOSpendingBlockOccurrenceFKRejectsWrongBlock is item 3,
// case "spending tx not contained by claimed spending block -> rejected":
// txspend really occurred in blockb, but utxo_state claims blockc.
func TestInvariant_UTXOSpendingBlockOccurrenceFKRejectsWrongBlock(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
	fixtureBlock(t, ctx, tx, hash64("blockc"), 102, nil)
	fixtureTransaction(t, ctx, tx, hash64("txspend"), false)
	fixtureTransactionInput(t, ctx, tx, hash64("txspend"), 0, hash64("txcreate"), 0)
	fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txspend"), 0) // really in blockb

	_, err := tx.Exec(ctx, `
		INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height,
			spent, spending_txid, spending_vin_index, spending_block_hash, spending_block_height)
		VALUES ($1, 0, $2, 100, true, $3, 0, $4, 102)
	`, hash64("txcreate"), hash64("blocka"), hash64("txspend"), hash64("blockc")) // claims blockc
	if err == nil {
		t.Fatal("expected utxo_state claiming the wrong spending block to be rejected, got nil error")
	}
}

// TestInvariant_UTXOHeightsCannotPersistWrong is item 3, case "supplied
// wrong heights cannot persist": creation_block_height is trigger-derived
// from blocks.height, overwriting whatever the caller supplied.
func TestInvariant_UTXOHeightsCannotPersistWrong(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	mustExec(t, ctx, tx, `
		INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height)
		VALUES ($1, 0, $2, 999999)
	`, hash64("txcreate"), hash64("blocka")) // 999999 is deliberately wrong; blocka is really height 100

	var height int64
	if err := tx.QueryRow(ctx, `SELECT creation_block_height FROM utxo_state WHERE txid=$1 AND vout_index=0`, hash64("txcreate")).Scan(&height); err != nil {
		t.Fatalf("read back creation_block_height: %v", err)
	}
	if height != 100 {
		t.Errorf("creation_block_height = %d, want 100 (trigger-derived; the wrong supplied value must not persist)", height)
	}
}

// TestInvariant_UTXOValidSpendSucceeds is item 3, case "valid spend
// relationship succeeds" — the full happy path, also confirming both
// creation_block_height and spending_block_height are correctly
// trigger-derived even when (deliberately, here) wrong values are supplied
// for both.
func TestInvariant_UTXOValidSpendSucceeds(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
	fixtureTransaction(t, ctx, tx, hash64("txspend"), false)
	fixtureTransactionInput(t, ctx, tx, hash64("txspend"), 0, hash64("txcreate"), 0)
	fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txspend"), 0)

	mustExec(t, ctx, tx, `
		INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height,
			spent, spending_txid, spending_vin_index, spending_block_hash, spending_block_height)
		VALUES ($1, 0, $2, 1, true, $3, 0, $4, 999)
	`, hash64("txcreate"), hash64("blocka"), hash64("txspend"), hash64("blockb")) // 1, 999 deliberately wrong

	var creationHeight, spendingHeight int64
	if err := tx.QueryRow(ctx, `SELECT creation_block_height, spending_block_height FROM utxo_state WHERE txid=$1 AND vout_index=0`,
		hash64("txcreate")).Scan(&creationHeight, &spendingHeight); err != nil {
		t.Fatalf("read back heights: %v", err)
	}
	if creationHeight != 100 {
		t.Errorf("creation_block_height = %d, want 100", creationHeight)
	}
	if spendingHeight != 101 {
		t.Errorf("spending_block_height = %d, want 101", spendingHeight)
	}
}

// TestInvariant_Uint32RangeRejected is item 6: blocks.nonce,
// transactions.locktime, and transaction_inputs.sequence are all
// consensus-wire uint32 fields and must be rejected outside [0,4294967295].
func TestInvariant_Uint32RangeRejected(t *testing.T) {
	t.Run("nonce negative", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
			VALUES ($1, 500, NULL, $1, 1700000000, '1d00ffff', 1.0, -1, 100, 400, 1)
		`, hash64("badnonceneg"))
		if err == nil {
			t.Fatal("expected negative nonce to be rejected, got nil error")
		}
	})

	t.Run("nonce above uint32 max", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
			VALUES ($1, 500, NULL, $1, 1700000000, '1d00ffff', 1.0, 4294967296, 100, 400, 1)
		`, hash64("badnoncehi"))
		if err == nil {
			t.Fatal("expected nonce above uint32 max to be rejected, got nil error")
		}
	})

	t.Run("locktime negative", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, size, vsize, weight, is_coinbase)
			VALUES ($1, 2, -1, 100, 100, 400, false)
		`, hash64("badlocktimeneg"))
		if err == nil {
			t.Fatal("expected negative locktime to be rejected, got nil error")
		}
	})

	t.Run("locktime above uint32 max", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, size, vsize, weight, is_coinbase)
			VALUES ($1, 2, 4294967296, 100, 100, 400, false)
		`, hash64("badlocktimehi"))
		if err == nil {
			t.Fatal("expected locktime above uint32 max to be rejected, got nil error")
		}
	})

	t.Run("sequence above uint32 max", func(t *testing.T) {
		ctx, tx := txPool(t)
		fixtureTransaction(t, ctx, tx, hash64("txbadseq"), false)
		_, err := tx.Exec(ctx, `
			INSERT INTO transaction_inputs (txid, vin_index, prev_txid, prev_vout_index, script_sig, sequence)
			VALUES ($1, 0, $2, 0, ''::bytea, 4294967296)
		`, hash64("txbadseq"), hash64("someprevtx"))
		if err == nil {
			t.Fatal("expected sequence above uint32 max to be rejected, got nil error")
		}
	})

	t.Run("boundary values (0 and 4294967295) accepted", func(t *testing.T) {
		ctx, tx := txPool(t)
		mustExec(t, ctx, tx, `
			INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
			VALUES ($1, 500, NULL, $1, 1700000000, '1d00ffff', 1.0, 4294967295, 100, 400, 1)
		`, hash64("maxnonce"))
		mustExec(t, ctx, tx, `
			INSERT INTO transactions (txid, version, locktime, size, vsize, weight, is_coinbase)
			VALUES ($1, 2, 4294967295, 100, 100, 400, false)
		`, hash64("maxlocktimetx"))
		mustExec(t, ctx, tx, `
			INSERT INTO transaction_inputs (txid, vin_index, prev_txid, prev_vout_index, script_sig, sequence)
			VALUES ($1, 0, $2, 0, ''::bytea, 4294967295)
		`, hash64("maxlocktimetx"), hash64("someotherprevtx"))
	})
}

// TestInvariant_FeeConstraints is item 6: fee_satoshis must be NULL or
// non-negative, and a coinbase transaction must not carry a fee value at
// all (see docs/ARCHITECTURE.md §6 — coinbase value is subsidy + fees, not
// itself "a fee").
func TestInvariant_FeeConstraints(t *testing.T) {
	t.Run("negative fee rejected", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, size, vsize, weight, is_coinbase, fee_satoshis)
			VALUES ($1, 2, 0, 100, 100, 400, false, -1)
		`, hash64("negfee"))
		if err == nil {
			t.Fatal("expected negative fee_satoshis to be rejected, got nil error")
		}
	})

	t.Run("coinbase with a fee rejected", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, size, vsize, weight, is_coinbase, fee_satoshis)
			VALUES ($1, 2, 0, 100, 100, 400, true, 100)
		`, hash64("coinbasefee"))
		if err == nil {
			t.Fatal("expected a coinbase transaction with a non-NULL fee to be rejected, got nil error")
		}
	})

	t.Run("non-coinbase with a nonnegative fee accepted", func(t *testing.T) {
		ctx, tx := txPool(t)
		mustExec(t, ctx, tx, `
			INSERT INTO transactions (txid, version, locktime, size, vsize, weight, is_coinbase, fee_satoshis)
			VALUES ($1, 2, 0, 100, 100, 400, false, 0)
		`, hash64("zerofee"))
	})
}
