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

// fixtureTransaction inserts the immutable, NON-WITNESS transaction body
// (task item 1: txid = Core GetHash(), excludes witness data — size/vsize/
// weight now live on transaction_variants, not here, because they depend
// on which witness serialization was actually observed).
func fixtureTransaction(t *testing.T, ctx context.Context, tx pgx.Tx, txid string, isCoinbase bool) {
	t.Helper()
	mustExec(t, ctx, tx, `
		INSERT INTO transactions (txid, version, locktime, is_coinbase)
		VALUES ($1, 2, 0, $2)
	`, txid, isCoinbase)
}

// fixtureTransactionVariant inserts one concrete witness serialization
// (wtxid = Core GetWitnessHash()) of an already-inserted txid. A
// transaction with no witness data at all still gets exactly one variant
// row, with wtxid == txid.
func fixtureTransactionVariant(t *testing.T, ctx context.Context, tx pgx.Tx, wtxid, txid string) {
	t.Helper()
	mustExec(t, ctx, tx, `
		INSERT INTO transaction_variants (wtxid, txid, size, vsize, weight)
		VALUES ($1, $2, 100, 100, 400)
	`, wtxid, txid)
}

// fixtureSimpleTransaction is the common-case convenience most invariant
// tests below actually want: a transaction body plus its single witness
// variant, with wtxid == txid. Tests that aren't specifically exercising
// wtxid/txid variance (see TestInvariant_WitnessVariantsDoNotOverwriteEachOther
// for the one that is) use this rather than calling fixtureTransaction and
// fixtureTransactionVariant separately.
func fixtureSimpleTransaction(t *testing.T, ctx context.Context, tx pgx.Tx, txid string, isCoinbase bool) {
	t.Helper()
	fixtureTransaction(t, ctx, tx, txid, isCoinbase)
	fixtureTransactionVariant(t, ctx, tx, txid, txid)
}

// fixtureBlockTransaction records that the (txid, wtxid) variant occurred
// in blockHash at tx_index — the occurrence link both the utxo_state
// relational-integrity FKs (task item 3, PR #2 second round) and the
// wtxid/txid variant model (task item 1, PR #2 third round) validate
// against.
func fixtureBlockTransaction(t *testing.T, ctx context.Context, tx pgx.Tx, blockHash, txid, wtxid string, txIndex int) {
	t.Helper()
	mustExec(t, ctx, tx, `INSERT INTO block_transactions (block_hash, tx_index, txid, wtxid) VALUES ($1, $2, $3, $4)`,
		blockHash, txIndex, txid, wtxid)
}

// fixtureTransactionInput inserts a minimal non-coinbase input, so
// utxo_state's spending-input FK has something real to reference. Inputs
// remain txid-scoped (not per-variant): scriptSig/prevout are part of the
// txid-determining serialization, shared by every witness variant of that
// txid.
func fixtureTransactionInput(t *testing.T, ctx context.Context, tx pgx.Tx, txid string, vinIndex int, prevTxid string, prevVout int) {
	t.Helper()
	mustExec(t, ctx, tx, `
		INSERT INTO transaction_inputs (txid, vin_index, prev_txid, prev_vout_index, script_sig, sequence)
		VALUES ($1, $2, $3, $4, ''::bytea, 4294967295)
	`, txid, vinIndex, prevTxid, prevVout)
}

// fixtureTransactionInputWitness inserts one witness stack item for a
// specific (wtxid, txid, vin_index, item_index) — variant-scoped per task
// item 1, so two different witness variants of the same txid never
// overwrite each other's witness data.
func fixtureTransactionInputWitness(t *testing.T, ctx context.Context, tx pgx.Tx, wtxid, txid string, vinIndex, itemIndex int, data []byte) {
	t.Helper()
	mustExec(t, ctx, tx, `
		INSERT INTO transaction_input_witness (wtxid, txid, vin_index, item_index, data)
		VALUES ($1, $2, $3, $4, $5)
	`, wtxid, txid, vinIndex, itemIndex, data)
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
// occurrence (docs/ARCHITECTURE.md). This test uses wtxid == txid
// throughout (it isn't specifically about witness variance — see
// TestInvariant_WitnessVariantsDoNotOverwriteEachOther for that).
func TestInvariant_SameTxidTwoBlocks(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil) // independent height; competing-fork detail is irrelevant to this invariant
	fixtureSimpleTransaction(t, ctx, tx, hash64("txshared"), false)

	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txshared"), hash64("txshared"), 0)
	fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txshared"), hash64("txshared"), 0)

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
	fixtureSimpleTransaction(t, ctx, tx, hash64("txqpk"), false)
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

	fixtureTransactionInputWitness(t, ctx, tx, hash64("txqpk"), hash64("txqpk"), 0, 0, sig)
	fixtureTransactionInputWitness(t, ctx, tx, hash64("txqpk"), hash64("txqpk"), 0, 1, pubkey)

	var gotSig, gotPubkey []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid=$1 AND vin_index=0 AND item_index=0`, hash64("txqpk")).Scan(&gotSig); err != nil {
		t.Fatalf("read back signature: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid=$1 AND vin_index=0 AND item_index=1`, hash64("txqpk")).Scan(&gotPubkey); err != nil {
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

// ─── PR #2 review round 2 fixes ─────────────────────────────────────────

// TestInvariant_WitnessMetadataConsistency is item 1 (round 2): the P2QPK
// CHECK NULL loophole (script_type='p2qpk' with witness_version/
// witness_program both NULL previously passed, because a CHECK expression
// that evaluates to NULL is treated as satisfied by Postgres) plus
// structural version/length consistency for every known witness
// script_type. Extended in round 3 (item 3) with tighter unknown/
// unknown_witness boundary cases: the 2..40 structural witness-program
// length range, and explicit exclusion of unknown_witness from any
// version/length combination that actually belongs to a named type.
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

		// unknown_witness: round-2 cases plus round-3's tighter boundary
		// requirements (2..40 length range, explicit exclusion of v1/32
		// and v2/32 — those belong to P2TR/P2QPK, never unknown_witness).
		{"unknown_witness + v3/25 accepted", "unknown_witness", intPtr(3), make([]byte, 25), false},
		{"unknown_witness + NULL/NULL rejected (must carry nonzero version)", "unknown_witness", nil, nil, true},
		{"unknown_witness + v0/25 rejected (version must be >0)", "unknown_witness", intPtr(0), make([]byte, 25), true},
		{"unknown_witness + v1/32 rejected (that's P2TR)", "unknown_witness", intPtr(1), make([]byte, 32), true},
		{"unknown_witness + v2/32 rejected (that's P2QPK)", "unknown_witness", intPtr(2), make([]byte, 32), true},
		{"unknown_witness + v2/31 accepted", "unknown_witness", intPtr(2), make([]byte, 31), false},
		{"unknown_witness + v3/32 accepted", "unknown_witness", intPtr(3), make([]byte, 32), false},
		{"unknown_witness + program length 1 rejected (below structural minimum 2)", "unknown_witness", intPtr(5), make([]byte, 1), true},
		{"unknown_witness + program length 41 rejected (above structural maximum 40)", "unknown_witness", intPtr(5), make([]byte, 41), true},

		// Legacy (non-witness) types must carry no witness metadata at all.
		{"p2pkh + NULL/NULL accepted", "p2pkh", nil, nil, false},
		{"p2pkh + v0/20 rejected (legacy type must not carry witness metadata)", "p2pkh", intPtr(0), make([]byte, 20), true},
		{"p2pk + NULL/NULL accepted", "p2pk", nil, nil, false},
		{"nulldata + NULL/NULL accepted", "nulldata", nil, nil, false},
		{"multisig + NULL/NULL accepted", "multisig", nil, nil, false},

		// unknown: exactly two legitimate shapes (no witness program at
		// all, or a structurally valid off-length witness-v0 program) —
		// anything else, including a length/version combo that actually
		// belongs to a named type, or a length outside the 2..40
		// structural range, is rejected.
		{"unknown + NULL/NULL accepted", "unknown", nil, nil, false},
		{"unknown + v0/19 accepted", "unknown", intPtr(0), make([]byte, 19), false},
		{"unknown + v0/25 accepted (off-length v0)", "unknown", intPtr(0), make([]byte, 25), false},
		{"unknown + v0/20 rejected (that length is always p2wpkh)", "unknown", intPtr(0), make([]byte, 20), true},
		{"unknown + v0/32 rejected (that length is always p2wsh)", "unknown", intPtr(0), make([]byte, 32), true},
		{"unknown + v0/1 rejected (below structural minimum 2)", "unknown", intPtr(0), make([]byte, 1), true},
		{"unknown + v0/41 rejected (above structural maximum 40)", "unknown", intPtr(0), make([]byte, 41), true},
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

// TestInvariant_OneDestinationAddressPerOutput is item 2 (round 2):
// output_addresses permits at most one row per (txid, vout_index) —
// PRIMARY KEY (txid, vout_index), not (txid, vout_index, address) — so
// even an ordinary, non-multisig output can't have its value
// multiply-credited across two different addresses.
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

// TestInvariant_UTXOCreationOccurrenceFKRejectsWrongBlock is item 3
// (round 2), case "wrong creation block for txid -> rejected": the
// output's transaction really occurred in blocka (via block_transactions),
// but the utxo_state row claims it was created in blockb, which never
// contained that txid.
func TestInvariant_UTXOCreationOccurrenceFKRejectsWrongBlock(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
	fixtureSimpleTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	_, err := tx.Exec(ctx, `
		INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height)
		VALUES ($1, 0, $2, 101)
	`, hash64("txcreate"), hash64("blockb"))
	if err == nil {
		t.Fatal("expected utxo_state claiming the wrong creation block to be rejected, got nil error")
	}
}

// TestInvariant_UTXOSpendingInputFKRejectsNonexistentInput is item 3
// (round 2), case "nonexistent spending input -> rejected":
// spending_txid/spending_vin_index claim a transaction_inputs row that was
// never inserted.
func TestInvariant_UTXOSpendingInputFKRejectsNonexistentInput(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureSimpleTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
	fixtureSimpleTransaction(t, ctx, tx, hash64("txspend"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txspend"), hash64("txspend"), 0)
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

// TestInvariant_UTXOSpendingBlockOccurrenceFKRejectsWrongBlock is item 3
// (round 2), case "spending tx not contained by claimed spending block ->
// rejected": txspend really occurred in blockb, but utxo_state claims
// blockc.
func TestInvariant_UTXOSpendingBlockOccurrenceFKRejectsWrongBlock(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureSimpleTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
	fixtureBlock(t, ctx, tx, hash64("blockc"), 102, nil)
	fixtureSimpleTransaction(t, ctx, tx, hash64("txspend"), false)
	fixtureTransactionInput(t, ctx, tx, hash64("txspend"), 0, hash64("txcreate"), 0)
	fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txspend"), hash64("txspend"), 0) // really in blockb

	_, err := tx.Exec(ctx, `
		INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height,
			spent, spending_txid, spending_vin_index, spending_block_hash, spending_block_height)
		VALUES ($1, 0, $2, 100, true, $3, 0, $4, 102)
	`, hash64("txcreate"), hash64("blocka"), hash64("txspend"), hash64("blockc")) // claims blockc
	if err == nil {
		t.Fatal("expected utxo_state claiming the wrong spending block to be rejected, got nil error")
	}
}

// TestInvariant_UTXOHeightsCannotPersistWrong is item 3 (round 2), case
// "supplied wrong heights cannot persist": creation_block_height is
// trigger-derived from blocks.height, overwriting whatever the caller
// supplied.
func TestInvariant_UTXOHeightsCannotPersistWrong(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureSimpleTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), hash64("txcreate"), 0)
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

// TestInvariant_UTXOValidSpendSucceeds is item 3 (round 2), case "valid
// spend relationship succeeds" — the full happy path, also confirming both
// creation_block_height and spending_block_height are correctly
// trigger-derived even when (deliberately, here) wrong values are supplied
// for both. Also doubles as item 2 (round 3)'s "spending input exists and
// points to this exact output -> PASS" case, now that the spend FK
// requires the exact prevout match.
func TestInvariant_UTXOValidSpendSucceeds(t *testing.T) {
	ctx, tx := txPool(t)

	fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
	fixtureSimpleTransaction(t, ctx, tx, hash64("txcreate"), false)
	fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), hash64("txcreate"), 0)
	fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

	fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
	fixtureSimpleTransaction(t, ctx, tx, hash64("txspend"), false)
	fixtureTransactionInput(t, ctx, tx, hash64("txspend"), 0, hash64("txcreate"), 0)
	fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txspend"), hash64("txspend"), 0)

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

// TestInvariant_Uint32RangeRejected is item 6 (round 2): blocks.nonce,
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
			INSERT INTO transactions (txid, version, locktime, is_coinbase)
			VALUES ($1, 2, -1, false)
		`, hash64("badlocktimeneg"))
		if err == nil {
			t.Fatal("expected negative locktime to be rejected, got nil error")
		}
	})

	t.Run("locktime above uint32 max", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, is_coinbase)
			VALUES ($1, 2, 4294967296, false)
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

	t.Run("version negative", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, is_coinbase)
			VALUES ($1, -1, 0, false)
		`, hash64("badversionneg"))
		if err == nil {
			t.Fatal("expected negative version to be rejected, got nil error")
		}
	})

	t.Run("version above uint32 max", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, is_coinbase)
			VALUES ($1, 4294967296, 0, false)
		`, hash64("badversionhi"))
		if err == nil {
			t.Fatal("expected version above uint32 max to be rejected, got nil error")
		}
	})

	t.Run("version boundary and int32-crossing values accepted", func(t *testing.T) {
		ctx, tx := txPool(t)
		// Confirms transactions.version isn't silently clamped to Core's
		// in-memory int32_t range — 2147483648 (2^31, the smallest value
		// that overflows a signed 32-bit int) and 4294967295 (uint32 max)
		// must both be representable, per docs/ARCHITECTURE.md §3's
		// RPC-uint32-not-C++-int32 distinction.
		for i, v := range []int64{0, 2147483647, 2147483648, 4294967295} {
			mustExec(t, ctx, tx, `
				INSERT INTO transactions (txid, version, locktime, is_coinbase)
				VALUES ($1, $2, 0, false)
			`, hash64(fmt.Sprintf("versionok%d", i)), v)
		}
	})

	t.Run("boundary values (0 and 4294967295) accepted", func(t *testing.T) {
		ctx, tx := txPool(t)
		mustExec(t, ctx, tx, `
			INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
			VALUES ($1, 500, NULL, $1, 1700000000, '1d00ffff', 1.0, 4294967295, 100, 400, 1)
		`, hash64("maxnonce"))
		mustExec(t, ctx, tx, `
			INSERT INTO transactions (txid, version, locktime, is_coinbase)
			VALUES ($1, 2, 4294967295, false)
		`, hash64("maxlocktimetx"))
		mustExec(t, ctx, tx, `
			INSERT INTO transaction_inputs (txid, vin_index, prev_txid, prev_vout_index, script_sig, sequence)
			VALUES ($1, 0, $2, 0, ''::bytea, 4294967295)
		`, hash64("maxlocktimetx"), hash64("someotherprevtx"))
	})
}

// TestInvariant_FeeConstraints is item 6 (round 2): fee_satoshis must be
// NULL or non-negative, and a coinbase transaction must not carry a fee
// value at all (see docs/ARCHITECTURE.md §6 — coinbase value is subsidy +
// fees, not itself "a fee").
func TestInvariant_FeeConstraints(t *testing.T) {
	t.Run("negative fee rejected", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, is_coinbase, fee_satoshis)
			VALUES ($1, 2, 0, false, -1)
		`, hash64("negfee"))
		if err == nil {
			t.Fatal("expected negative fee_satoshis to be rejected, got nil error")
		}
	})

	t.Run("coinbase with a fee rejected", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `
			INSERT INTO transactions (txid, version, locktime, is_coinbase, fee_satoshis)
			VALUES ($1, 2, 0, true, 100)
		`, hash64("coinbasefee"))
		if err == nil {
			t.Fatal("expected a coinbase transaction with a non-NULL fee to be rejected, got nil error")
		}
	})

	t.Run("non-coinbase with a nonnegative fee accepted", func(t *testing.T) {
		ctx, tx := txPool(t)
		mustExec(t, ctx, tx, `
			INSERT INTO transactions (txid, version, locktime, is_coinbase, fee_satoshis)
			VALUES ($1, 2, 0, false, 0)
		`, hash64("zerofee"))
	})
}

// ─── PR #2 review round 3 fixes ─────────────────────────────────────────

// TestInvariant_WitnessVariantsDoNotOverwriteEachOther is the decisive test
// for item 1 (round 3, the wtxid/txid split): one txid T, two witness
// variants W1 and W2 with distinct witness bytes and distinct
// size/vsize/weight, each occurring in a different block. Proves both
// block occurrences exist, both share txid T, the wtxids differ, each
// variant's witness stack is preserved exactly and independently, and
// neither variant's witness data or metrics overwrite the other's — the
// exact scenario a txid-only-keyed schema could not represent. W2's
// witness includes a real 17,088-byte P2QPK-sized item. This is a database
// fixture only; no transaction was broadcast or manufactured on any
// network.
func TestInvariant_WitnessVariantsDoNotOverwriteEachOther(t *testing.T) {
	ctx, tx := txPool(t)

	txidT := hash64("sharedtxid")
	wtxid1 := hash64("variantw1")
	wtxid2 := hash64("variantw2")
	if wtxid1 == wtxid2 {
		t.Fatal("test bug: wtxid1 and wtxid2 must differ")
	}

	fixtureTransaction(t, ctx, tx, txidT, false)
	fixtureTransactionInput(t, ctx, tx, txidT, 0, hash64("someprevtx"), 0)

	// Two variants of the SAME txid, with distinct size/vsize/weight.
	mustExec(t, ctx, tx, `INSERT INTO transaction_variants (wtxid, txid, size, vsize, weight) VALUES ($1, $2, 500, 300, 1200)`, wtxid1, txidT)
	mustExec(t, ctx, tx, `INSERT INTO transaction_variants (wtxid, txid, size, vsize, weight) VALUES ($1, $2, 17600, 500, 2000)`, wtxid2, txidT)

	// Distinct witness content per variant — W2's item is a real
	// 17,088-byte P2QPK-sized signature.
	witnessW1 := []byte{0xde, 0xad, 0xbe, 0xef}
	witnessW2Sig := make([]byte, script.P2QPKSignatureLength)
	for i := range witnessW2Sig {
		witnessW2Sig[i] = byte(i % 233)
	}

	fixtureTransactionInputWitness(t, ctx, tx, wtxid1, txidT, 0, 0, witnessW1)
	fixtureTransactionInputWitness(t, ctx, tx, wtxid2, txidT, 0, 0, witnessW2Sig)

	fixtureBlock(t, ctx, tx, hash64("varblocka"), 200, nil)
	fixtureBlock(t, ctx, tx, hash64("varblockb"), 201, nil)
	fixtureBlockTransaction(t, ctx, tx, hash64("varblocka"), txidT, wtxid1, 0) // block A -> T/W1
	fixtureBlockTransaction(t, ctx, tx, hash64("varblockb"), txidT, wtxid2, 0) // block B -> T/W2

	// Both block occurrences exist, both share txid T, wtxids differ.
	rows, err := tx.Query(ctx, `SELECT block_hash, txid, wtxid FROM block_transactions WHERE txid = $1 ORDER BY block_hash`, txidT)
	if err != nil {
		t.Fatalf("query block_transactions: %v", err)
	}
	type occurrence struct{ blockHash, txid, wtxid string }
	var occurrences []occurrence
	for rows.Next() {
		var o occurrence
		if err := rows.Scan(&o.blockHash, &o.txid, &o.wtxid); err != nil {
			rows.Close()
			t.Fatalf("scan occurrence: %v", err)
		}
		occurrences = append(occurrences, o)
	}
	closeErr := rows.Err()
	rows.Close()
	if closeErr != nil {
		t.Fatalf("iterate occurrences: %v", closeErr)
	}
	if len(occurrences) != 2 {
		t.Fatalf("expected 2 block occurrences for txid %s, got %d", txidT, len(occurrences))
	}
	seenWtxids := map[string]bool{}
	for _, o := range occurrences {
		if o.txid != txidT {
			t.Errorf("occurrence txid = %s, want %s", o.txid, txidT)
		}
		seenWtxids[o.wtxid] = true
	}
	if !seenWtxids[wtxid1] || !seenWtxids[wtxid2] {
		t.Fatalf("expected both wtxids %s and %s among occurrences, got %v", wtxid1, wtxid2, seenWtxids)
	}

	// Each variant's witness stack is preserved exactly and independently
	// — neither overwrites the other.
	var gotW1, gotW2 []byte
	if err := tx.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid=$1 AND vin_index=0 AND item_index=0`, wtxid1).Scan(&gotW1); err != nil {
		t.Fatalf("read back W1 witness: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT data FROM transaction_input_witness WHERE wtxid=$1 AND vin_index=0 AND item_index=0`, wtxid2).Scan(&gotW2); err != nil {
		t.Fatalf("read back W2 witness: %v", err)
	}
	if !bytes.Equal(gotW1, witnessW1) {
		t.Errorf("W1 witness = %x, want %x (must not have been overwritten by W2)", gotW1, witnessW1)
	}
	if len(gotW2) != script.P2QPKSignatureLength || !bytes.Equal(gotW2, witnessW2Sig) {
		t.Errorf("W2 witness mismatch or wrong length (got %d bytes, want %d)", len(gotW2), script.P2QPKSignatureLength)
	}

	// Variant-specific size/vsize/weight are preserved independently too —
	// neither variant's metrics overwrite the other's.
	var size1, vsize1, weight1, size2, vsize2, weight2 int
	if err := tx.QueryRow(ctx, `SELECT size, vsize, weight FROM transaction_variants WHERE wtxid=$1`, wtxid1).Scan(&size1, &vsize1, &weight1); err != nil {
		t.Fatalf("read back W1 metrics: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT size, vsize, weight FROM transaction_variants WHERE wtxid=$1`, wtxid2).Scan(&size2, &vsize2, &weight2); err != nil {
		t.Fatalf("read back W2 metrics: %v", err)
	}
	if size1 != 500 || vsize1 != 300 || weight1 != 1200 {
		t.Errorf("W1 metrics = (%d,%d,%d), want (500,300,1200)", size1, vsize1, weight1)
	}
	if size2 != 17600 || vsize2 != 500 || weight2 != 2000 {
		t.Errorf("W2 metrics = (%d,%d,%d), want (17600,500,2000) — must not have been overwritten by W1", size2, vsize2, weight2)
	}
}

// TestInvariant_UTXOSpendMustMatchExactPrevout is item 2 (round 3): an
// input that really exists (and really occurred in the claimed spending
// block) but spends a DIFFERENT output — wrong txid, or the right txid but
// the wrong vout — must not be usable to mark some other output "spent".
func TestInvariant_UTXOSpendMustMatchExactPrevout(t *testing.T) {
	t.Run("input exists but points to a different txid", func(t *testing.T) {
		ctx, tx := txPool(t)

		fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
		fixtureSimpleTransaction(t, ctx, tx, hash64("txcreate"), false)
		fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), hash64("txcreate"), 0)
		fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

		fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
		fixtureSimpleTransaction(t, ctx, tx, hash64("txspend"), false)
		// txspend's real input spends a DIFFERENT txid's output (txother:0), not txcreate:0.
		fixtureTransactionInput(t, ctx, tx, hash64("txspend"), 0, hash64("txother"), 0)
		fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txspend"), hash64("txspend"), 0)

		// Claim txspend's vin 0 spends txcreate:0 anyway — it doesn't.
		_, err := tx.Exec(ctx, `
			INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height,
				spent, spending_txid, spending_vin_index, spending_block_hash, spending_block_height)
			VALUES ($1, 0, $2, 100, true, $3, 0, $4, 101)
		`, hash64("txcreate"), hash64("blocka"), hash64("txspend"), hash64("blockb"))
		if err == nil {
			t.Fatal("expected utxo_state to reject a spending input that actually points to a different txid, got nil error")
		}
	})

	t.Run("input exists but points to a different vout", func(t *testing.T) {
		ctx, tx := txPool(t)

		fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
		fixtureSimpleTransaction(t, ctx, tx, hash64("txcreate"), false)
		fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), hash64("txcreate"), 0)
		fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)
		fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 1, "p2pkh", 2000)

		fixtureBlock(t, ctx, tx, hash64("blockb"), 101, nil)
		fixtureSimpleTransaction(t, ctx, tx, hash64("txspend"), false)
		// txspend's real input spends txcreate:1, not txcreate:0.
		fixtureTransactionInput(t, ctx, tx, hash64("txspend"), 0, hash64("txcreate"), 1)
		fixtureBlockTransaction(t, ctx, tx, hash64("blockb"), hash64("txspend"), hash64("txspend"), 0)

		// Claim txspend's vin 0 spends txcreate:0 anyway — it actually spends txcreate:1.
		_, err := tx.Exec(ctx, `
			INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height,
				spent, spending_txid, spending_vin_index, spending_block_hash, spending_block_height)
			VALUES ($1, 0, $2, 100, true, $3, 0, $4, 101)
		`, hash64("txcreate"), hash64("blocka"), hash64("txspend"), hash64("blockb"))
		if err == nil {
			t.Fatal("expected utxo_state to reject a spending input that actually points to a different vout, got nil error")
		}
	})

	t.Run("unspent row still works", func(t *testing.T) {
		ctx, tx := txPool(t)

		fixtureBlock(t, ctx, tx, hash64("blocka"), 100, nil)
		fixtureSimpleTransaction(t, ctx, tx, hash64("txcreate"), false)
		fixtureBlockTransaction(t, ctx, tx, hash64("blocka"), hash64("txcreate"), hash64("txcreate"), 0)
		fixtureTransactionOutput(t, ctx, tx, hash64("txcreate"), 0, "p2pkh", 1000)

		mustExec(t, ctx, tx, `
			INSERT INTO utxo_state (txid, vout_index, creation_block_hash, creation_block_height)
			VALUES ($1, 0, $2, 100)
		`, hash64("txcreate"), hash64("blocka"))

		var spent bool
		if err := tx.QueryRow(ctx, `SELECT spent FROM utxo_state WHERE txid=$1 AND vout_index=0`, hash64("txcreate")).Scan(&spent); err != nil {
			t.Fatalf("read back unspent row: %v", err)
		}
		if spent {
			t.Error("expected spent=false for a freshly created, never-spent output")
		}
	})
}

// ─── PR #2 review round 4 fixes ────────────────────────────────────────

// TestInvariant_SyncStateCheckpointMustMatchCanonicalBlock is the decisive
// test for round 4 item 2: sync_state_validate_checkpoint_trigger must
// prove an initialized checkpoint's indexed_block_hash is a real, canonical
// block, and must derive indexed_height from blocks.height itself rather
// than trust whatever the caller supplied.
func TestInvariant_SyncStateCheckpointMustMatchCanonicalBlock(t *testing.T) {
	t.Run("bootstrap -1/NULL remains valid", func(t *testing.T) {
		ctx, tx := txPool(t)
		mustExec(t, ctx, tx, `INSERT INTO sync_state (name, indexed_height, indexed_block_hash) VALUES ('checkpoint_bootstrap', -1, NULL)`)
	})

	t.Run("initialized checkpoint referencing a canonical block succeeds", func(t *testing.T) {
		ctx, tx := txPool(t)
		fixtureBlock(t, ctx, tx, hash64("syncblockvalid"), 42, nil)
		mustExec(t, ctx, tx, `INSERT INTO sync_state (name, indexed_height, indexed_block_hash) VALUES ('checkpoint_valid', 42, $1)`, hash64("syncblockvalid"))

		var height int64
		if err := tx.QueryRow(ctx, `SELECT indexed_height FROM sync_state WHERE name='checkpoint_valid'`).Scan(&height); err != nil {
			t.Fatalf("read back checkpoint: %v", err)
		}
		if height != 42 {
			t.Errorf("indexed_height = %d, want 42", height)
		}
	})

	t.Run("caller-supplied wrong height is mechanically corrected to blocks.height", func(t *testing.T) {
		ctx, tx := txPool(t)
		fixtureBlock(t, ctx, tx, hash64("syncblockcorrect"), 99, nil)
		// Deliberately claim height=1 for a block that's actually height 99
		// — the trigger must overwrite this, not trust it.
		mustExec(t, ctx, tx, `INSERT INTO sync_state (name, indexed_height, indexed_block_hash) VALUES ('checkpoint_corrected', 1, $1)`, hash64("syncblockcorrect"))

		var height int64
		if err := tx.QueryRow(ctx, `SELECT indexed_height FROM sync_state WHERE name='checkpoint_corrected'`).Scan(&height); err != nil {
			t.Fatalf("read back checkpoint: %v", err)
		}
		if height != 99 {
			t.Errorf("indexed_height = %d, want 99 (caller-supplied 1 should have been overridden by blocks.height)", height)
		}
	})

	t.Run("nonexistent block hash rejected", func(t *testing.T) {
		ctx, tx := txPool(t)
		_, err := tx.Exec(ctx, `INSERT INTO sync_state (name, indexed_height, indexed_block_hash) VALUES ('checkpoint_missing', 5, $1)`, hash64("nosuchsyncblock"))
		if err == nil {
			t.Fatal("expected a checkpoint referencing a nonexistent block hash to be rejected, got nil error")
		}
	})

	t.Run("orphaned (non-canonical) block hash rejected", func(t *testing.T) {
		ctx, tx := txPool(t)
		fixtureBlock(t, ctx, tx, hash64("syncblockorphan"), 7, nil)
		mustExec(t, ctx, tx, `UPDATE blocks SET canonical = false, orphaned_at = now() WHERE hash = $1`, hash64("syncblockorphan"))

		_, err := tx.Exec(ctx, `INSERT INTO sync_state (name, indexed_height, indexed_block_hash) VALUES ('checkpoint_orphan', 7, $1)`, hash64("syncblockorphan"))
		if err == nil {
			t.Fatal("expected a checkpoint referencing an orphaned (canonical=false) block to be rejected, got nil error")
		}
	})
}
