package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/jackc/pgx/v5"
)

// ApplyBlock persists one already-parsed, canonical block inside exactly
// one PostgreSQL transaction: the block header, every transaction's
// immutable body/variant/occurrence, every input/output/witness item,
// canonical UTXO state transitions, and every touched address's
// recomputed cache — finished by advancing sync_state('main') to this
// block as the FINAL logical write. If anything fails before commit,
// NOTHING from this block persists. See docs/ARCHITECTURE.md §16.
//
// ApplyBlock is idempotent: reapplying the exact same already-indexed
// block succeeds as a no-op beyond re-verifying already-correct state and
// recomputing (to the same values) any touched address caches. Applying a
// block whose data contradicts already-persisted immutable records for the
// same identity (same block hash/txid/wtxid/input/output, different body)
// returns an error wrapping ErrImmutableConflict rather than silently
// overwriting it.
//
// ApplyBlock assumes block.Transactions is already in a valid order — each
// transaction's inputs may only reference outputs created earlier in this
// same block or in an already-applied block, never later in this block.
// This is guaranteed by any block Qogecoin Core itself considers valid.
func (s *Store) ApplyBlock(ctx context.Context, block chain.Block) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin apply block %s: %w", block.Hash, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var currentCheckpointHeight int64
	if err := tx.QueryRow(ctx, `SELECT indexed_height FROM sync_state WHERE name = 'main'`).Scan(&currentCheckpointHeight); err != nil {
		return fmt.Errorf("store: apply block %s: read current checkpoint: %w", block.Hash, err)
	}

	if err := applyBlockHeader(ctx, tx, block); err != nil {
		return fmt.Errorf("store: apply block %s: %w", block.Hash, err)
	}

	touched := map[string]struct{}{}
	for i, txn := range block.Transactions {
		if err := applyTransaction(ctx, tx, block.Hash, i, txn, touched); err != nil {
			return fmt.Errorf("store: apply block %s tx %d (txid %s): %w", block.Hash, i, txn.TxID, err)
		}
	}

	for addr := range touched {
		if err := recomputeAddress(ctx, tx, addr); err != nil {
			return fmt.Errorf("store: apply block %s: recompute address %s: %w", block.Hash, addr, err)
		}
	}

	// The checkpoint is the FINAL logical write (task item 10) — and is
	// only ever advanced, never rewound, by a lower-or-equal-height block
	// (e.g. a deliberate historical re-index run). The
	// sync_state_validate_checkpoint_trigger independently re-derives
	// indexed_height from blocks.height and verifies block.Hash is
	// canonical, so this doubles as a final correctness gate before
	// commit.
	if block.Height >= currentCheckpointHeight {
		if _, err := tx.Exec(ctx, `
			UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'
		`, block.Hash); err != nil {
			return fmt.Errorf("store: apply block %s: checkpoint: %w", block.Hash, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit block %s: %w", block.Hash, err)
	}
	return nil
}

func applyBlockHeader(ctx context.Context, tx pgx.Tx, block chain.Block) error {
	var prevHash *string
	if block.PreviousHash != "" {
		prevHash = &block.PreviousHash
	}
	return insertOrVerifyIdempotent(ctx, tx, "block "+block.Hash,
		`INSERT INTO blocks (hash, height, prev_hash, merkle_root, "time", bits, difficulty, nonce, size, weight, tx_count)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (hash) DO NOTHING`,
		`SELECT height = $2 AND prev_hash IS NOT DISTINCT FROM $3 AND merkle_root = $4
		    AND "time" = $5 AND bits = $6 AND difficulty = $7 AND nonce = $8
		    AND size = $9 AND weight = $10 AND tx_count = $11
		 FROM blocks WHERE hash = $1`,
		block.Hash, block.Height, prevHash, block.MerkleRoot, block.Time, block.Bits,
		block.Difficulty, int64(block.Nonce), block.Size, block.Weight, block.TxCount,
	)
}

// applyTransaction persists one transaction's body/variant/occurrence,
// every input (+ witness), every output (+ destination/participants +
// UTXO creation), and marks every prevout it spends as spent — in that
// order, so every foreign key the schema requires is already satisfied by
// the time each statement runs. Addresses this transaction created an
// output for or spent a UTXO from are added to touched.
func applyTransaction(ctx context.Context, tx pgx.Tx, blockHash string, txIndex int, txn chain.Transaction, touched map[string]struct{}) error {
	hasWitness := false
	for _, in := range txn.Inputs {
		if !in.Witness.IsEmpty() {
			hasWitness = true
			break
		}
	}
	if (txn.WTxID == txn.TxID) == hasWitness {
		return fmt.Errorf("%w: txid=%s wtxid=%s hasWitness=%t", ErrWitnessIdentityMismatch, txn.TxID, txn.WTxID, hasWitness)
	}

	var fee *int64
	if !txn.IsCoinbase {
		var inputSum int64
		for _, in := range txn.Inputs {
			if in.PreviousOut == nil {
				return fmt.Errorf("store: non-coinbase input %d has no PreviousOut", in.Index)
			}
			value, err := checkPrevout(ctx, tx, in.PreviousOut.TxID, int(in.PreviousOut.Index), txn.TxID, int(in.Index))
			if err != nil {
				return err
			}
			inputSum += value
		}
		var outputSum int64
		for _, out := range txn.Outputs {
			outputSum += out.Value.Satoshis()
		}
		f := inputSum - outputSum
		if f < 0 {
			return fmt.Errorf("%w: inputs=%d outputs=%d", ErrNegativeFee, inputSum, outputSum)
		}
		fee = &f
	}

	if err := insertOrVerifyIdempotent(ctx, tx, "transaction "+txn.TxID,
		`INSERT INTO transactions (txid, version, locktime, is_coinbase, fee_satoshis)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (txid) DO NOTHING`,
		`SELECT version = $2 AND locktime = $3 AND is_coinbase = $4 AND fee_satoshis IS NOT DISTINCT FROM $5
		 FROM transactions WHERE txid = $1`,
		txn.TxID, int64(txn.Version), int64(txn.LockTime), txn.IsCoinbase, fee,
	); err != nil {
		return err
	}

	if err := insertOrVerifyIdempotent(ctx, tx, "transaction_variant "+txn.WTxID,
		`INSERT INTO transaction_variants (wtxid, txid, size, vsize, weight)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (wtxid) DO NOTHING`,
		`SELECT txid = $2 AND size = $3 AND vsize = $4 AND weight = $5
		 FROM transaction_variants WHERE wtxid = $1`,
		txn.WTxID, txn.TxID, txn.Size, txn.VSize, txn.Weight,
	); err != nil {
		return err
	}

	if err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("block_transaction %s:%d", blockHash, txIndex),
		`INSERT INTO block_transactions (block_hash, tx_index, txid, wtxid)
		 VALUES ($1,$2,$3,$4) ON CONFLICT (block_hash, tx_index) DO NOTHING`,
		`SELECT txid = $3 AND wtxid = $4 FROM block_transactions WHERE block_hash = $1 AND tx_index = $2`,
		blockHash, txIndex, txn.TxID, txn.WTxID,
	); err != nil {
		return err
	}

	for _, in := range txn.Inputs {
		if err := applyInput(ctx, tx, txn.TxID, txn.WTxID, in); err != nil {
			return err
		}
	}

	for _, out := range txn.Outputs {
		addr, err := applyOutput(ctx, tx, blockHash, txn.TxID, out)
		if err != nil {
			return err
		}
		if addr != "" {
			touched[addr] = struct{}{}
		}
	}

	if !txn.IsCoinbase {
		for _, in := range txn.Inputs {
			addr, err := markSpent(ctx, tx, in.PreviousOut.TxID, int(in.PreviousOut.Index), txn.TxID, int(in.Index), blockHash)
			if err != nil {
				return err
			}
			if addr != "" {
				touched[addr] = struct{}{}
			}
		}
	}

	return nil
}

// checkPrevout resolves a non-coinbase input's previous output: it must
// have a utxo_state row (ErrMissingPrevout otherwise — the store never
// invents a missing previous output) that is either currently unspent, or
// already spent by exactly this (spenderTxid, spenderVin) — the idempotent
// replay case. Any other spender is ErrDoubleSpend. Returns the prevout's
// value in satoshis for fee computation.
func checkPrevout(ctx context.Context, tx pgx.Tx, prevTxid string, prevVout int, spenderTxid string, spenderVin int) (int64, error) {
	var value int64
	var spent bool
	var spendingTxid *string
	var spendingVin *int64
	err := tx.QueryRow(ctx, `
		SELECT o.value_satoshis, u.spent, u.spending_txid, u.spending_vin_index
		FROM transaction_outputs o
		JOIN utxo_state u ON u.txid = o.txid AND u.vout_index = o.vout_index
		WHERE o.txid = $1 AND o.vout_index = $2
	`, prevTxid, prevVout).Scan(&value, &spent, &spendingTxid, &spendingVin)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s:%d", ErrMissingPrevout, prevTxid, prevVout)
	}
	if err != nil {
		return 0, fmt.Errorf("store: check prevout %s:%d: %w", prevTxid, prevVout, err)
	}
	if spent {
		if spendingTxid != nil && *spendingTxid == spenderTxid && spendingVin != nil && int(*spendingVin) == spenderVin {
			return value, nil // already spent by exactly this input: idempotent replay
		}
		return 0, fmt.Errorf("%w: %s:%d", ErrDoubleSpend, prevTxid, prevVout)
	}
	return value, nil
}

// markSpent records that (prevTxid, prevVout) was spent by
// (spenderTxid, spenderVin) in spendingBlockHash. The WHERE spent = false
// guard makes this a safe no-op when checkPrevout already confirmed the
// row is spent by exactly this input (idempotent replay) — by the time
// this runs, checkPrevout has already ruled out every other case. Returns
// the spent output's destination address, if any, for address-cache
// touch-tracking.
func markSpent(ctx context.Context, tx pgx.Tx, prevTxid string, prevVout int, spenderTxid string, spenderVin int, spendingBlockHash string) (string, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE utxo_state
		SET spent = true, spending_txid = $1, spending_vin_index = $2, spending_block_hash = $3
		WHERE txid = $4 AND vout_index = $5 AND spent = false
	`, spenderTxid, spenderVin, spendingBlockHash, prevTxid, prevVout); err != nil {
		return "", fmt.Errorf("store: mark spent %s:%d: %w", prevTxid, prevVout, err)
	}

	var addr string
	err := tx.QueryRow(ctx, `SELECT address FROM output_addresses WHERE txid = $1 AND vout_index = $2`, prevTxid, prevVout).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: lookup spent output address %s:%d: %w", prevTxid, prevVout, err)
	}
	return addr, nil
}

func applyInput(ctx context.Context, tx pgx.Tx, txid, wtxid string, in chain.Input) error {
	var prevTxid *string
	var prevVout *int64
	var coinbase []byte
	if in.PreviousOut != nil {
		t := in.PreviousOut.TxID
		prevTxid = &t
		v := int64(in.PreviousOut.Index)
		prevVout = &v
	} else {
		coinbase = in.Coinbase
	}

	if err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("transaction_input %s:%d", txid, in.Index),
		`INSERT INTO transaction_inputs (txid, vin_index, prev_txid, prev_vout_index, coinbase, script_sig, sequence)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (txid, vin_index) DO NOTHING`,
		`SELECT prev_txid IS NOT DISTINCT FROM $3 AND prev_vout_index IS NOT DISTINCT FROM $4
		    AND coinbase IS NOT DISTINCT FROM $5 AND script_sig IS NOT DISTINCT FROM $6 AND sequence = $7
		 FROM transaction_inputs WHERE txid = $1 AND vin_index = $2`,
		txid, int64(in.Index), prevTxid, prevVout, coinbase, in.ScriptSig, int64(in.Sequence),
	); err != nil {
		return err
	}

	for itemIndex, item := range in.Witness {
		if err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("transaction_input_witness %s:%d:%d", wtxid, in.Index, itemIndex),
			`INSERT INTO transaction_input_witness (wtxid, txid, vin_index, item_index, data)
			 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (wtxid, vin_index, item_index) DO NOTHING`,
			`SELECT txid = $2 AND data = $5
			 FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = $3 AND item_index = $4`,
			wtxid, txid, int64(in.Index), itemIndex, item,
		); err != nil {
			return err
		}
	}
	return nil
}

// applyOutput persists one output's body, its destination address (if
// any), its bare-multisig participants (if any), and creates its
// utxo_state row. Returns the destination address, if any, for
// address-cache touch-tracking.
func applyOutput(ctx context.Context, tx pgx.Tx, blockHash, txid string, out chain.Output) (string, error) {
	if err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("transaction_output %s:%d", txid, out.Index),
		`INSERT INTO transaction_outputs (txid, vout_index, value_satoshis, script_pubkey, script_type, witness_version, witness_program)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (txid, vout_index) DO NOTHING`,
		`SELECT value_satoshis = $3 AND script_pubkey = $4 AND script_type = $5
		    AND witness_version IS NOT DISTINCT FROM $6 AND witness_program IS NOT DISTINCT FROM $7
		 FROM transaction_outputs WHERE txid = $1 AND vout_index = $2`,
		txid, int64(out.Index), out.Value.Satoshis(), out.ScriptPubKey, string(out.ScriptType), out.WitnessVersion, out.WitnessProgram,
	); err != nil {
		return "", err
	}

	// output_addresses: balance-accounting destination ONLY. Never written
	// for a multisig output (see docs/ARCHITECTURE.md §7/§13.A) — the
	// output_addresses_reject_multisig_trigger would reject it anyway, but
	// we simply never attempt it, since Address is empty for the multisig
	// outputs this codebase currently produces.
	if out.Address != "" {
		if err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("output_address %s:%d", txid, out.Index),
			`INSERT INTO output_addresses (txid, vout_index, address) VALUES ($1,$2,$3) ON CONFLICT (txid, vout_index) DO NOTHING`,
			`SELECT address = $3 FROM output_addresses WHERE txid = $1 AND vout_index = $2`,
			txid, int64(out.Index), out.Address,
		); err != nil {
			return "", err
		}
	}

	// output_participants: MULTISIG search/display identities ONLY, never
	// used for balance accounting (see recomputeAddress, which only ever
	// reads output_addresses).
	if out.ScriptType == script.TypeMultisig {
		for i, pubkey := range out.PubKeys {
			if i >= len(out.ParticipantAddresses) || out.ParticipantAddresses[i] == "" {
				continue
			}
			addr := out.ParticipantAddresses[i]
			if err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("output_participant %s:%d:%s", txid, out.Index, addr),
				`INSERT INTO output_participants (txid, vout_index, address, pubkey) VALUES ($1,$2,$3,$4) ON CONFLICT (txid, vout_index, address) DO NOTHING`,
				`SELECT pubkey = $4 FROM output_participants WHERE txid = $1 AND vout_index = $2 AND address = $3`,
				txid, int64(out.Index), addr, pubkey,
			); err != nil {
				return "", err
			}
		}
	}

	// Canonical UTXO creation. spent defaults false; the idempotent-verify
	// SQL only compares creation_block_hash, never spent/spending_* — a
	// conflict check here must never disturb a row's current mutable spend
	// status (see docs/ARCHITECTURE.md §16).
	if err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("utxo_state creation %s:%d", txid, out.Index),
		`INSERT INTO utxo_state (txid, vout_index, creation_block_hash) VALUES ($1,$2,$3) ON CONFLICT (txid, vout_index) DO NOTHING`,
		`SELECT creation_block_hash = $3 FROM utxo_state WHERE txid = $1 AND vout_index = $2`,
		txid, int64(out.Index), blockHash,
	); err != nil {
		return "", err
	}

	return out.Address, nil
}

// recomputeAddress SETs (never increments) address's cached totals from
// current source tables — output_addresses joined against utxo_state and
// transaction_outputs — so it is safe to call any number of times, in any
// order, from ApplyBlock or RollbackTo alike. See docs/ARCHITECTURE.md §16
// "Address cache" for the exact semantics documented here:
//
//   - total_received_satoshis = SUM(value) over every output ever created
//     to this address (via output_addresses), spent or not.
//   - total_sent_satoshis = SUM(value) over this address's outputs that
//     are currently spent.
//   - balance_satoshis = SUM(value) over this address's currently unspent
//     outputs (equivalently total_received - total_sent).
//   - tx_count = count of DISTINCT transactions this address participated
//     in, as either a creation (received an output) or a spend (a
//     previously-received output was spent by some transaction).
//   - first_seen_height = MIN(creation_block_height) over this address's
//     outputs.
//   - last_seen_height = MAX(the higher of an output's creation height and
//     its spending height, if spent) — the highest block height at which
//     any activity touching this address occurred.
//
// If recomputation finds zero remaining canonical activity for address
// (e.g. every output that ever named it was rolled back by RollbackTo),
// its addresses row is deleted rather than left as a phantom all-zero
// entry.
func recomputeAddress(ctx context.Context, tx pgx.Tx, address string) error {
	var totalReceived, totalSent, balance, txCount int64
	var firstSeen, lastSeen *int64

	err := tx.QueryRow(ctx, `
		WITH addr_utxos AS (
			SELECT o.value_satoshis, u.spent, u.creation_block_height, u.spending_block_height,
			       u.txid, u.spending_txid
			FROM output_addresses oa
			JOIN transaction_outputs o ON o.txid = oa.txid AND o.vout_index = oa.vout_index
			JOIN utxo_state u ON u.txid = oa.txid AND u.vout_index = oa.vout_index
			WHERE oa.address = $1
		),
		tx_ids AS (
			SELECT txid AS tid FROM addr_utxos
			UNION
			SELECT spending_txid AS tid FROM addr_utxos WHERE spent
		)
		SELECT
			COALESCE((SELECT SUM(value_satoshis) FROM addr_utxos), 0),
			COALESCE((SELECT SUM(value_satoshis) FROM addr_utxos WHERE spent), 0),
			COALESCE((SELECT SUM(value_satoshis) FROM addr_utxos WHERE NOT spent), 0),
			(SELECT COUNT(*) FROM tx_ids),
			(SELECT MIN(creation_block_height) FROM addr_utxos),
			(SELECT MAX(GREATEST(creation_block_height, COALESCE(spending_block_height, creation_block_height))) FROM addr_utxos)
	`, address).Scan(&totalReceived, &totalSent, &balance, &txCount, &firstSeen, &lastSeen)
	if err != nil {
		return fmt.Errorf("store: recompute address %s: %w", address, err)
	}

	if txCount == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM addresses WHERE address = $1`, address); err != nil {
			return fmt.Errorf("store: remove empty address cache %s: %w", address, err)
		}
		return nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO addresses (address, total_received_satoshis, total_sent_satoshis, balance_satoshis, tx_count, first_seen_height, last_seen_height, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now())
		ON CONFLICT (address) DO UPDATE SET
			total_received_satoshis = EXCLUDED.total_received_satoshis,
			total_sent_satoshis = EXCLUDED.total_sent_satoshis,
			balance_satoshis = EXCLUDED.balance_satoshis,
			tx_count = EXCLUDED.tx_count,
			first_seen_height = EXCLUDED.first_seen_height,
			last_seen_height = EXCLUDED.last_seen_height,
			updated_at = now()
	`, address, totalReceived, totalSent, balance, txCount, firstSeen, lastSeen); err != nil {
		return fmt.Errorf("store: upsert address cache %s: %w", address, err)
	}
	return nil
}
