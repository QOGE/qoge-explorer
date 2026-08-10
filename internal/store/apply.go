package store

import (
	"bytes"
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
// # Canonical tip continuity
//
// The very first statement ApplyBlock issues is lockCheckpoint: a
// "SELECT ... FOR UPDATE" against sync_state('main'). This is the
// canonical-mutation lock — it serializes every ApplyBlock/RollbackTo call
// against every other one, across goroutines AND across separate Store
// instances/processes sharing the database, for as long as this
// transaction is open.
//
// Once locked, block must have an explicit, provable relationship to the
// checkpoint it just read — for an INITIALIZED store, exactly one of:
//
//   - exact tip replay: block.Hash == checkpoint.Hash AND
//     block.Height == checkpoint.Height, or
//   - immediate canonical append: block.Height == checkpoint.Height + 1
//     AND block.PreviousHash == checkpoint.Hash.
//
// Anything else — a height jump, a mismatched PreviousHash, or any block
// (canonical or orphaned) below the current tip — is ErrNonSequentialBlock
// and leaves the database completely unchanged: a lower historical block
// can never mutate canonical UTXO/address state merely because ApplyBlock
// was called on it.
//
// For an UNINITIALIZED store (sync_state still at its bootstrap -1/NULL
// row), ApplyBlock accepts any block unconditionally as this store's
// bootstrap point — there is no existing tip to compare against yet.
// Production historical sync always starts from genesis (height 0, no
// PreviousHash); this codebase's own tests use arbitrary synthetic
// "genesis" blocks at arbitrary heights, which this policy deliberately
// keeps working. Every block ApplyBlock accepts AFTER that first one is
// still bound by the continuity rule above, relative to whatever became
// the checkpoint.
//
// # Safe orphan re-promotion
//
// If block.Hash refers to an ALREADY-PERSISTED block that is currently
// orphaned (canonical = false, from an earlier RollbackTo) and it passes
// the continuity check above as the immediate child of the current tip,
// ApplyBlock promotes it: canonical = true, orphaned_at = NULL, and its
// canonical UTXO/address state is rebuilt exactly as it would be for any
// other immediate-append block (its transactions/inputs/outputs/witness
// rows were never deleted by RollbackTo in the first place — only its
// utxo_state rows were — so this "rebuild" is just the ordinary
// applyTransaction path re-creating what RollbackTo removed). An orphan
// that is NOT the immediate child of the current tip is never promoted;
// it is rejected by the continuity check like any other non-sequential
// block, before ApplyBlock even looks at whether it's orphaned.
//
// # Idempotency and immutable conflicts
//
// ApplyBlock is idempotent: reapplying the exact same already-indexed
// block succeeds as a no-op beyond re-verifying already-correct state and
// recomputing (to the same values) any touched address caches. Applying a
// block whose data contradicts already-persisted immutable records for the
// same identity (same block hash/txid/wtxid/input/output/witness item,
// different body, or a different-shaped set of children for the same
// parent identity — an added, omitted, or moved input/output/witness
// item) returns an error wrapping ErrImmutableConflict rather than
// silently overwriting it. See insertOrVerifyIdempotent and
// verifyExactCount (store.go).
//
// ApplyBlock assumes block.Transactions is already in a valid order — each
// transaction's inputs may only reference outputs created earlier in this
// same block or in an already-applied block, never later in this block.
// This is guaranteed by any block Qogecoin Core itself considers valid.
//
// # Core UTXO semantics
//
// transaction_outputs preserves every output that ever existed on-chain,
// 1:1, forever — including outputs Core itself never treats as spendable
// coins. utxo_state is NOT a mirror of transaction_outputs; it is the
// Core-equivalent canonical coin set, and is deliberately missing a row for
// two categories applyOutput excludes on purpose (mirroring
// CCoinsViewCache::AddCoin's "if (coin.out.scriptPubKey.IsUnspendable())
// return;", and ConnectBlock's genesis special case):
//
//   - every output of block height 0 (the genesis coinbase is documented,
//     in both Core and QOGE's own chainparams, as never having existed in
//     the coins database at all), and
//   - any output whose scriptPubKey is script.IsUnspendable (OP_RETURN, or
//     larger than script.MaxScriptSize), at any height.
//
// Neither exclusion drops or alters the immutable transaction_outputs row,
// its destination/participant metadata, or its presence in query results —
// only its presence in utxo_state (and therefore its eligibility to ever be
// spent, and its contribution to any address's cached balance, which is
// derived exclusively from utxo_state via recomputeAddress).
func (s *Store) ApplyBlock(ctx context.Context, block chain.Block) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin apply block %s: %w", block.Hash, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	checkpoint, err := lockCheckpoint(ctx, tx)
	if err != nil {
		return fmt.Errorf("store: apply block %s: %w", block.Hash, err)
	}

	if err := validateBlockShape(block); err != nil {
		return fmt.Errorf("store: apply block %s: %w", block.Hash, err)
	}

	if err := checkCanonicalContinuity(block, checkpoint); err != nil {
		return fmt.Errorf("store: apply block %s: %w", block.Hash, err)
	}

	blockFresh, err := applyBlockHeader(ctx, tx, block)
	if err != nil {
		return fmt.Errorf("store: apply block %s: %w", block.Hash, err)
	}
	if !blockFresh {
		if err := verifyExactCount(ctx, tx, "block_transactions for "+block.Hash,
			`SELECT count(*) FROM block_transactions WHERE block_hash = $1`,
			[]any{block.Hash}, len(block.Transactions),
		); err != nil {
			return fmt.Errorf("store: apply block %s: %w", block.Hash, err)
		}
	}

	// Height 0 is Core's genesis special case: ConnectBlock skips connecting
	// the genesis block's transactions at all ("its coinbase is unspendable"
	// — src/validation.cpp), and QOGE's chainparams document the same for
	// the genesis output specifically. See applyOutput.
	isGenesis := block.Height == 0

	touched := map[string]struct{}{}
	for i, txn := range block.Transactions {
		if err := applyTransaction(ctx, tx, block.Hash, i, txn, isGenesis, touched); err != nil {
			return fmt.Errorf("store: apply block %s tx %d (txid %s): %w", block.Hash, i, txn.TxID, err)
		}
	}

	for addr := range touched {
		if err := recomputeAddress(ctx, tx, addr); err != nil {
			return fmt.Errorf("store: apply block %s: recompute address %s: %w", block.Hash, addr, err)
		}
	}

	// The checkpoint is the FINAL logical write (task item 10). Continuity
	// has already proven block is either an exact tip replay (a harmless
	// no-op set here) or the immediate child of the previous checkpoint, so
	// this unconditionally advances (or re-affirms) it — no separate
	// height comparison is needed here anymore; continuity already did
	// the only comparison that matters. sync_state_validate_checkpoint_
	// trigger independently re-derives indexed_height from blocks.height
	// and verifies block.Hash is canonical (true by construction: either
	// it always was, or applyBlockHeader just promoted it), so this
	// doubles as a final correctness gate before commit.
	if _, err := tx.Exec(ctx, `
		UPDATE sync_state SET indexed_block_hash = $1, updated_at = now() WHERE name = 'main'
	`, block.Hash); err != nil {
		return fmt.Errorf("store: apply block %s: checkpoint: %w", block.Hash, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit block %s: %w", block.Hash, err)
	}
	return nil
}

// validateBlockShape checks block is internally self-consistent, before
// any database access: block.TxCount must match len(block.Transactions)
// exactly (ApplyBlock represents a fully indexed block, never a
// header-only one — see ErrIncompleteBlock), every transaction must have
// at least one input and one output (ApplyBlock represents a fully decoded
// transaction, never a possibly-partial RPC translation), every
// transaction's inputs/outputs must use the canonical positional index
// Core's own vin/vout array semantics guarantee
// (chain.Input.Index/chain.Output.Index documented as "this input/output's
// position within its transaction's vin/vout list" —
// internal/chain/input.go, output.go), every input's
// PreviousOut/Coinbase/ScriptSig fields must be mutually consistent (see
// below), and the transaction list must have Core-valid coinbase shape/
// positioning (see below). Enforcing the positional-index rule up front
// makes an "index moved to another slot" attack structurally impossible to
// even express, which is what lets the completeness checks in
// applyInput/applyOutput/applyTransaction rely on a simple count
// comparison rather than a full index-set comparison.
func validateBlockShape(block chain.Block) error {
	if len(block.Transactions) != block.TxCount {
		return fmt.Errorf("%w: block %s has TxCount=%d but %d transactions supplied",
			ErrIncompleteBlock, block.Hash, block.TxCount, len(block.Transactions))
	}
	if len(block.Transactions) == 0 {
		return fmt.Errorf("%w: block %s has no transactions", ErrInvalidTransactionShape, block.Hash)
	}
	for idx, txn := range block.Transactions {
		// Completeness, not consensus: ApplyBlock claims to persist a fully
		// decoded transaction, so an empty vin or vout must never be
		// accepted as a possibly-partial RPC translation — mirrors the
		// shape (not the reachability) of Core's CheckTransaction
		// bad-txns-vin-empty/bad-txns-vout-empty checks.
		if len(txn.Inputs) == 0 {
			return fmt.Errorf("%w: tx %s has no inputs", ErrInvalidTransactionShape, txn.TxID)
		}
		if len(txn.Outputs) == 0 {
			return fmt.Errorf("%w: tx %s has no outputs", ErrInvalidTransactionShape, txn.TxID)
		}

		for i, in := range txn.Inputs {
			if in.Index != uint32(i) {
				return fmt.Errorf("%w: tx %s input %d has Index=%d, want %d (canonical positional index)",
					ErrImmutableConflict, txn.TxID, i, in.Index, i)
			}
			// chain.Input documents PreviousOut/Coinbase/ScriptSig as
			// mutually exclusive by construction (internal/chain/input.go):
			// Coinbase is set only when PreviousOut is nil, ScriptSig is
			// empty for coinbase. Enforced here, not silently tolerated —
			// a future RPC decoder that ever constructs an inconsistent
			// model must not have its Coinbase bytes silently discarded
			// (applyInput never even looks at Coinbase when PreviousOut !=
			// nil). A non-coinbase input's ScriptSig is legitimately empty
			// for a pure witness spend, so that alone is never rejected.
			if in.PreviousOut == nil {
				if len(in.Coinbase) == 0 {
					return fmt.Errorf("%w: tx %s input %d is coinbase (nil PreviousOut) but has no Coinbase script bytes",
						ErrInvalidTransactionShape, txn.TxID, i)
				}
				if len(in.ScriptSig) != 0 {
					return fmt.Errorf("%w: tx %s input %d is coinbase (nil PreviousOut) but has a non-empty ScriptSig",
						ErrInvalidTransactionShape, txn.TxID, i)
				}
			} else if len(in.Coinbase) != 0 {
				return fmt.Errorf("%w: tx %s input %d is non-coinbase (PreviousOut set) but has Coinbase script bytes populated",
					ErrInvalidTransactionShape, txn.TxID, i)
			}
		}
		for i, out := range txn.Outputs {
			if out.Index != uint32(i) {
				return fmt.Errorf("%w: tx %s output %d has Index=%d, want %d (canonical positional index)",
					ErrImmutableConflict, txn.TxID, i, out.Index, i)
			}
		}

		// Mirrors Core's IsCoinBase() (src/primitives/transaction.h):
		// vin.size() == 1 && vin[0].prevout.IsNull(). txn.IsCoinbase is only
		// a flag on the already-parsed model — Store uses it to decide
		// whether to compute a fee and mark a prevout spent, so it must be
		// proven to agree with the actual input shape before either of
		// those monetary decisions is made, rather than trusted
		// independently (task item 3: a mismatched flag could otherwise
		// corrupt monetary state — e.g. IsCoinbase=true with a real
		// prevout would silently skip that prevout's spend marking).
		structurallyCoinbase := len(txn.Inputs) == 1 && txn.Inputs[0].PreviousOut == nil
		if txn.IsCoinbase != structurallyCoinbase {
			return fmt.Errorf("%w: tx %s IsCoinbase=%t but structurally %t (%d input(s))",
				ErrInvalidTransactionShape, txn.TxID, txn.IsCoinbase, structurallyCoinbase, len(txn.Inputs))
		}
		// Every real Core block has exactly one coinbase transaction, and
		// it is always transaction 0 — this is canonical block shape, not
		// merely a per-transaction property.
		if idx == 0 && !txn.IsCoinbase {
			return fmt.Errorf("%w: block %s: transaction 0 (%s) is not coinbase", ErrInvalidTransactionShape, block.Hash, txn.TxID)
		}
		if idx > 0 && txn.IsCoinbase {
			return fmt.Errorf("%w: block %s: transaction %d (%s) is coinbase but not transaction 0", ErrInvalidTransactionShape, block.Hash, idx, txn.TxID)
		}
	}
	return nil
}

// checkCanonicalContinuity enforces the relationship documented on
// ApplyBlock: for an uninitialized checkpoint, anything is accepted as
// this store's bootstrap point; otherwise block must be either an exact
// replay of the checkpoint or its immediate canonical child.
func checkCanonicalContinuity(block chain.Block, checkpoint Checkpoint) error {
	if checkpoint.Hash == "" {
		return nil
	}
	if block.Hash == checkpoint.Hash && block.Height == checkpoint.Height {
		return nil
	}
	if block.Height == checkpoint.Height+1 && block.PreviousHash == checkpoint.Hash {
		return nil
	}
	return fmt.Errorf("%w: block %s (height %d, prevHash %q) does not extend or replay checkpoint %s (height %d)",
		ErrNonSequentialBlock, block.Hash, block.Height, block.PreviousHash, checkpoint.Hash, checkpoint.Height)
}

func applyBlockHeader(ctx context.Context, tx pgx.Tx, block chain.Block) (bool, error) {
	var prevHash *string
	if block.PreviousHash != "" {
		prevHash = &block.PreviousHash
	}
	fresh, err := insertOrVerifyIdempotent(ctx, tx, "block "+block.Hash,
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
	if err != nil {
		return false, err
	}
	if fresh {
		return true, nil
	}

	// Safe orphan re-promotion (see ApplyBlock's doc comment): by the time
	// this runs, checkCanonicalContinuity has already proven block is
	// either an exact tip replay (always already canonical — the branch
	// below is a no-op for it) or the immediate child of the current tip.
	// If the existing row is currently orphaned, that means it's exactly
	// the "a previously orphaned branch becomes canonical again" case, and
	// it is now provably safe to promote it: the unique canonical-height
	// index remains a final DB guard against ever having two canonical
	// blocks at the same height regardless.
	var canonical bool
	if err := tx.QueryRow(ctx, `SELECT canonical FROM blocks WHERE hash = $1`, block.Hash).Scan(&canonical); err != nil {
		return false, fmt.Errorf("store: read canonical status of block %s: %w", block.Hash, err)
	}
	if !canonical {
		if _, err := tx.Exec(ctx, `UPDATE blocks SET canonical = true, orphaned_at = NULL WHERE hash = $1`, block.Hash); err != nil {
			return false, fmt.Errorf("store: promote orphaned block %s to canonical: %w", block.Hash, err)
		}
	}
	return false, nil
}

// addChecked adds b to a, returning ok=false instead of silently wrapping
// on int64 overflow (task item 6). Real QOGE mainnet values are far below
// this range, but the possibility is kept structurally impossible rather
// than merely improbable.
func addChecked(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// applyTransaction persists one transaction's body/variant/occurrence,
// every input (+ witness), every output (+ destination/participants +
// UTXO creation), and marks every prevout it spends as spent — in that
// order, so every foreign key the schema requires is already satisfied by
// the time each statement runs. Addresses this transaction created an
// output for or spent a UTXO from are added to touched. isGenesis is
// ApplyBlock's block.Height == 0 — see "Core UTXO semantics" on ApplyBlock.
func applyTransaction(ctx context.Context, tx pgx.Tx, blockHash string, txIndex int, txn chain.Transaction, isGenesis bool, touched map[string]struct{}) error {
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
			sum, ok := addChecked(inputSum, value)
			if !ok {
				return fmt.Errorf("%w: input value sum for tx %s", ErrAmountOverflow, txn.TxID)
			}
			inputSum = sum
		}
		var outputSum int64
		for _, out := range txn.Outputs {
			sum, ok := addChecked(outputSum, out.Value.Satoshis())
			if !ok {
				return fmt.Errorf("%w: output value sum for tx %s", ErrAmountOverflow, txn.TxID)
			}
			outputSum = sum
		}
		f := inputSum - outputSum
		if f < 0 {
			return fmt.Errorf("%w: inputs=%d outputs=%d", ErrNegativeFee, inputSum, outputSum)
		}
		fee = &f
	}

	transactionFresh, err := insertOrVerifyIdempotent(ctx, tx, "transaction "+txn.TxID,
		`INSERT INTO transactions (txid, version, locktime, is_coinbase, fee_satoshis)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (txid) DO NOTHING`,
		`SELECT version = $2 AND locktime = $3 AND is_coinbase = $4 AND fee_satoshis IS NOT DISTINCT FROM $5
		 FROM transactions WHERE txid = $1`,
		txn.TxID, int64(txn.Version), int64(txn.LockTime), txn.IsCoinbase, fee,
	)
	if err != nil {
		return err
	}

	variantFresh, err := insertOrVerifyIdempotent(ctx, tx, "transaction_variant "+txn.WTxID,
		`INSERT INTO transaction_variants (wtxid, txid, size, vsize, weight)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (wtxid) DO NOTHING`,
		`SELECT txid = $2 AND size = $3 AND vsize = $4 AND weight = $5
		 FROM transaction_variants WHERE wtxid = $1`,
		txn.WTxID, txn.TxID, txn.Size, txn.VSize, txn.Weight,
	)
	if err != nil {
		return err
	}

	if _, err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("block_transaction %s:%d", blockHash, txIndex),
		`INSERT INTO block_transactions (block_hash, tx_index, txid, wtxid)
		 VALUES ($1,$2,$3,$4) ON CONFLICT (block_hash, tx_index) DO NOTHING`,
		`SELECT txid = $3 AND wtxid = $4 FROM block_transactions WHERE block_hash = $1 AND tx_index = $2`,
		blockHash, txIndex, txn.TxID, txn.WTxID,
	); err != nil {
		return err
	}

	if !transactionFresh {
		if err := verifyExactCount(ctx, tx, "transaction_inputs for "+txn.TxID,
			`SELECT count(*) FROM transaction_inputs WHERE txid = $1`, []any{txn.TxID}, len(txn.Inputs),
		); err != nil {
			return err
		}
		if err := verifyExactCount(ctx, tx, "transaction_outputs for "+txn.TxID,
			`SELECT count(*) FROM transaction_outputs WHERE txid = $1`, []any{txn.TxID}, len(txn.Outputs),
		); err != nil {
			return err
		}
	}

	for _, in := range txn.Inputs {
		if err := applyInput(ctx, tx, txn.TxID, txn.WTxID, in, variantFresh); err != nil {
			return err
		}
	}

	for _, out := range txn.Outputs {
		addr, err := applyOutput(ctx, tx, blockHash, txn.TxID, out, isGenesis)
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
// (spenderTxid, spenderVin) in spendingBlockHash. It never assumes the
// UPDATE succeeded just because checkPrevout earlier saw spent = false:
// RowsAffected is inspected directly, and a zero-row result triggers a
// re-read to distinguish "already spent by exactly this input"
// (idempotent replay — a safe no-op) from a genuine conflict
// (ErrDoubleSpend) rather than silently treating zero rows as success.
// Returns the spent output's destination address, if any, for
// address-cache touch-tracking.
func markSpent(ctx context.Context, tx pgx.Tx, prevTxid string, prevVout int, spenderTxid string, spenderVin int, spendingBlockHash string) (string, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE utxo_state
		SET spent = true, spending_txid = $1, spending_vin_index = $2, spending_block_hash = $3
		WHERE txid = $4 AND vout_index = $5 AND spent = false
	`, spenderTxid, spenderVin, spendingBlockHash, prevTxid, prevVout)
	if err != nil {
		return "", fmt.Errorf("store: mark spent %s:%d: %w", prevTxid, prevVout, err)
	}
	if tag.RowsAffected() == 0 {
		var spendingTxid *string
		var spendingVinIndex *int64
		err := tx.QueryRow(ctx, `SELECT spending_txid, spending_vin_index FROM utxo_state WHERE txid = $1 AND vout_index = $2`, prevTxid, prevVout).
			Scan(&spendingTxid, &spendingVinIndex)
		if err != nil {
			return "", fmt.Errorf("store: re-read spend state %s:%d: %w", prevTxid, prevVout, err)
		}
		idempotent := spendingTxid != nil && *spendingTxid == spenderTxid && spendingVinIndex != nil && int(*spendingVinIndex) == spenderVin
		if !idempotent {
			return "", fmt.Errorf("%w: %s:%d", ErrDoubleSpend, prevTxid, prevVout)
		}
	}

	var addr string
	err = tx.QueryRow(ctx, `SELECT address FROM output_addresses WHERE txid = $1 AND vout_index = $2`, prevTxid, prevVout).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: lookup spent output address %s:%d: %w", prevTxid, prevVout, err)
	}
	return addr, nil
}

func applyInput(ctx context.Context, tx pgx.Tx, txid, wtxid string, in chain.Input, variantFresh bool) error {
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

	if _, err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("transaction_input %s:%d", txid, in.Index),
		`INSERT INTO transaction_inputs (txid, vin_index, prev_txid, prev_vout_index, coinbase, script_sig, sequence)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (txid, vin_index) DO NOTHING`,
		`SELECT prev_txid IS NOT DISTINCT FROM $3 AND prev_vout_index IS NOT DISTINCT FROM $4
		    AND coinbase IS NOT DISTINCT FROM $5 AND script_sig IS NOT DISTINCT FROM $6 AND sequence = $7
		 FROM transaction_inputs WHERE txid = $1 AND vin_index = $2`,
		txid, int64(in.Index), prevTxid, prevVout, coinbase, in.ScriptSig, int64(in.Sequence),
	); err != nil {
		return err
	}

	// Witness completeness is checked per (wtxid, vin_index) — not
	// aggregated across the whole wtxid — so a witness stack "moved" from
	// one input to another (same total item count, different
	// distribution) is caught here rather than only at an aggregate level:
	// each vin's own count must match exactly if this wtxid was already
	// observed before this call (variantFresh = false).
	if !variantFresh {
		if err := verifyExactCount(ctx, tx, fmt.Sprintf("transaction_input_witness for %s vin %d", wtxid, in.Index),
			`SELECT count(*) FROM transaction_input_witness WHERE wtxid = $1 AND vin_index = $2`,
			[]any{wtxid, int64(in.Index)}, len(in.Witness),
		); err != nil {
			return err
		}
	}

	for itemIndex, item := range in.Witness {
		if _, err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("transaction_input_witness %s:%d:%d", wtxid, in.Index, itemIndex),
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
// any), its bare-multisig participants (if any), and — unless isGenesis or
// the output is script.IsUnspendable, see ApplyBlock's "Core UTXO
// semantics" — creates its utxo_state row. Returns the destination
// address, if any, for address-cache touch-tracking.
func applyOutput(ctx context.Context, tx pgx.Tx, blockHash, txid string, out chain.Output, isGenesis bool) (string, error) {
	outputFresh, err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("transaction_output %s:%d", txid, out.Index),
		`INSERT INTO transaction_outputs (txid, vout_index, value_satoshis, script_pubkey, script_type, witness_version, witness_program)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (txid, vout_index) DO NOTHING`,
		`SELECT value_satoshis = $3 AND script_pubkey = $4 AND script_type = $5
		    AND witness_version IS NOT DISTINCT FROM $6 AND witness_program IS NOT DISTINCT FROM $7
		 FROM transaction_outputs WHERE txid = $1 AND vout_index = $2`,
		txid, int64(out.Index), out.Value.Satoshis(), out.ScriptPubKey, string(out.ScriptType), out.WitnessVersion, out.WitnessProgram,
	)
	if err != nil {
		return "", err
	}

	// output_addresses: balance-accounting destination ONLY, at most one
	// row per output. Never written for a multisig output (see
	// docs/ARCHITECTURE.md §7/§13.A) — the
	// output_addresses_reject_multisig_trigger would reject it anyway, but
	// we simply never attempt it, since Address is empty for the multisig
	// outputs this codebase currently produces. On a replay
	// (!outputFresh) that supplies NO address, a previously-persisted
	// address row must not be silently left in place uncompared — that
	// would be exactly the "one destination row iff the canonical Output
	// supplies Address" completeness gap, just via omission rather than a
	// content mismatch.
	if out.Address != "" {
		if _, err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("output_address %s:%d", txid, out.Index),
			`INSERT INTO output_addresses (txid, vout_index, address) VALUES ($1,$2,$3) ON CONFLICT (txid, vout_index) DO NOTHING`,
			`SELECT address = $3 FROM output_addresses WHERE txid = $1 AND vout_index = $2`,
			txid, int64(out.Index), out.Address,
		); err != nil {
			return "", err
		}
	} else if !outputFresh {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM output_addresses WHERE txid = $1 AND vout_index = $2)`, txid, int64(out.Index)).Scan(&exists); err != nil {
			return "", fmt.Errorf("store: check existing output_address %s:%d: %w", txid, out.Index, err)
		}
		if exists {
			return "", fmt.Errorf("%w: output %s:%d previously had a destination address, now supplies none", ErrImmutableConflict, txid, out.Index)
		}
	}

	// output_participants: MULTISIG search/display identities ONLY, never
	// used for balance accounting (see recomputeAddress, which only ever
	// reads output_addresses). ParticipantAddresses is documented as
	// parallel to PubKeys (chain.Output) — that relationship is required
	// explicitly here, not silently tolerated: a shape mismatch, or any
	// empty address, is rejected rather than quietly skipping the
	// offending entries.
	//
	// output_participants is a SET of (address, pubkey) participant
	// identities, keyed by (txid, vout_index, address) — a bare multisig
	// script can structurally list the same pubkey more than once (its raw
	// scriptPubKey bytes preserve that duplication exactly, unaffected by
	// any of this); two identical (address, pubkey) entries are the SAME
	// participant identity, not two, and are deduplicated here before
	// persistence/completeness counting so an exact replay of a
	// duplicate-participant output remains idempotent. The same address
	// claimed with two DIFFERENT pubkeys is a genuine identity conflict,
	// not a duplicate, and is rejected.
	if out.ScriptType == script.TypeMultisig {
		if len(out.ParticipantAddresses) != len(out.PubKeys) {
			return "", fmt.Errorf("%w: output %s:%d is multisig with %d pubkeys but %d participant addresses",
				ErrImmutableConflict, txid, out.Index, len(out.PubKeys), len(out.ParticipantAddresses))
		}
		participants := make(map[string][]byte, len(out.PubKeys))
		order := make([]string, 0, len(out.PubKeys))
		for i, addr := range out.ParticipantAddresses {
			if addr == "" {
				return "", fmt.Errorf("%w: output %s:%d participant %d has an empty address", ErrImmutableConflict, txid, out.Index, i)
			}
			pubkey := out.PubKeys[i]
			if existing, ok := participants[addr]; ok {
				if !bytes.Equal(existing, pubkey) {
					return "", fmt.Errorf("%w: output %s:%d address %s claimed with two different pubkeys",
						ErrImmutableConflict, txid, out.Index, addr)
				}
				continue // exact duplicate identity: same participant, not a new one
			}
			participants[addr] = pubkey
			order = append(order, addr)
		}
		if !outputFresh {
			if err := verifyExactCount(ctx, tx, fmt.Sprintf("output_participants for %s:%d", txid, out.Index),
				`SELECT count(*) FROM output_participants WHERE txid = $1 AND vout_index = $2`,
				[]any{txid, int64(out.Index)}, len(order),
			); err != nil {
				return "", err
			}
		}
		for _, addr := range order {
			pubkey := participants[addr]
			if _, err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("output_participant %s:%d:%s", txid, out.Index, addr),
				`INSERT INTO output_participants (txid, vout_index, address, pubkey) VALUES ($1,$2,$3,$4) ON CONFLICT (txid, vout_index, address) DO NOTHING`,
				`SELECT pubkey = $4 FROM output_participants WHERE txid = $1 AND vout_index = $2 AND address = $3`,
				txid, int64(out.Index), addr, pubkey,
			); err != nil {
				return "", err
			}
		}
	}

	// Canonical UTXO creation — mirrors Core's CCoinsViewCache::AddCoin,
	// which never adds an unspendable output to the coins view at all, and
	// ConnectBlock's genesis special case, which skips connecting the
	// genesis block's transactions entirely (see ApplyBlock's "Core UTXO
	// semantics"). transaction_outputs above is written unconditionally
	// either way — this only controls whether a canonical, spendable coin
	// exists for it. spent defaults false; the idempotent-verify SQL only
	// compares creation_block_hash, never spent/spending_* — a conflict
	// check here must never disturb a row's current mutable spend status
	// (see docs/ARCHITECTURE.md §16).
	if !isGenesis && !script.IsUnspendable(out.ScriptPubKey) {
		if _, err := insertOrVerifyIdempotent(ctx, tx, fmt.Sprintf("utxo_state creation %s:%d", txid, out.Index),
			`INSERT INTO utxo_state (txid, vout_index, creation_block_hash) VALUES ($1,$2,$3) ON CONFLICT (txid, vout_index) DO NOTHING`,
			`SELECT creation_block_hash = $3 FROM utxo_state WHERE txid = $1 AND vout_index = $2`,
			txid, int64(out.Index), blockHash,
		); err != nil {
			return "", err
		}
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
