package mempool

import (
	"context"
	"fmt"
	"time"

	"github.com/QOGE/qoge-explorer/internal/script"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists mempool snapshots into the dedicated mempool_* tables
// (migrations/0002_mempool_cache.up.sql). It never reads or writes any
// confirmed-chain table (docs/ARCHITECTURE.md §22) and knows nothing
// about Core RPC — see sync.go for snapshot acquisition.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an already-connected, already-migrated pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// State is a read-only view of the mempool_state('main') singleton row.
// Initialized distinguishes "never successfully synchronized" (false, the
// zero value of every other field) from "successfully synchronized"
// (true) — including a successfully-synchronized-and-currently-empty
// mempool, which is Initialized=true, TxCount=0.
type State struct {
	Initialized      bool
	Generation       int64
	CoreTipHeight    *int64
	CoreTipHash      *string
	TxCount          int
	TotalVSize       int64
	TotalFeeSatoshis int64
	ObservedAt       *time.Time
}

// State returns the current mempool_state('main') row.
func (s *Store) State(ctx context.Context) (State, error) {
	var st State
	err := s.pool.QueryRow(ctx, `
		SELECT initialized, generation, core_tip_height, core_tip_hash,
		       tx_count, total_vsize, total_fee_satoshis, observed_at
		FROM mempool_state WHERE name = 'main'
	`).Scan(&st.Initialized, &st.Generation, &st.CoreTipHeight, &st.CoreTipHash,
		&st.TxCount, &st.TotalVSize, &st.TotalFeeSatoshis, &st.ObservedAt)
	if err != nil {
		return State{}, fmt.Errorf("mempool: read state: %w", err)
	}
	return st, nil
}

// ReplaceSnapshot atomically replaces the entire mempool cache with
// candidate: one PostgreSQL transaction acquires the mempool_state row
// lock (serializing concurrent writers), removes every row of the
// previous snapshot, inserts the complete new one, updates mempool_state
// LAST, and commits. If ANY step fails, the whole transaction rolls back
// and the previous complete snapshot remains exactly as it was — a
// half-replaced mempool is never observable (docs/ARCHITECTURE.md §22,
// spec items 22/26).
//
// generation is incremented on every successful commit, including a
// non-empty -> empty transition (an observed empty mempool is real
// synchronized state, not "nothing happened") — never on a failed or
// skipped refresh.
func (s *Store) ReplaceSnapshot(ctx context.Context, candidate Candidate) (generation int64, err error) {
	if verr := candidate.validate(); verr != nil {
		return 0, verr
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("mempool: begin replace snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// Serialize concurrent writers on the singleton state row before
	// touching anything else — the same row-lock-first pattern
	// internal/store.lockCheckpoint uses for canonical mutations.
	if _, lockErr := tx.Exec(ctx, `SELECT 1 FROM mempool_state WHERE name = 'main' FOR UPDATE`); lockErr != nil {
		err = fmt.Errorf("mempool: lock state row: %w", lockErr)
		return 0, err
	}

	// Full replacement, not additive deltas: clear the entire previous
	// snapshot. Every mempool_* child table cascades from
	// mempool_transactions (ON DELETE CASCADE), so this single statement
	// atomically clears inputs/witness/outputs/addresses/participants/
	// dependencies too.
	if _, delErr := tx.Exec(ctx, `DELETE FROM mempool_transactions`); delErr != nil {
		err = fmt.Errorf("mempool: clear previous snapshot: %w", delErr)
		return 0, err
	}

	var totalVSize int64
	var totalFee int64

	// Two-pass insertion, deliberately NOT one pass per candidate
	// transaction: mempool_dependencies.txid AND .depends_on_txid are both
	// immediate foreign keys into mempool_transactions(txid). Candidate
	// transactions are not topologically ordered by the caller (sync.go
	// lists them in lexicographic txid order, which has no dependency
	// meaning at all), so a single-pass "insert transaction, then its
	// dependencies, then the next transaction" loop fails the FK check
	// whenever any child transaction's txid happens to sort before its
	// parent's. Pass 1 inserts every mempool_transactions row (and every
	// non-dependency child row) for the WHOLE candidate; only once every
	// parent txid is guaranteed to exist does pass 2 insert every
	// dependency row. Store therefore accepts a valid candidate in ANY
	// slice order — see TestReplaceSnapshot_DependencyOrderIndependent.
	for _, ctxn := range candidate.Transactions {
		if insErr := insertCandidateTransactionBody(ctx, tx, ctxn); insErr != nil {
			err = insErr
			return 0, err
		}
		totalVSize += int64(ctxn.VSize)
		totalFee += ctxn.FeeSatoshis
	}
	for _, ctxn := range candidate.Transactions {
		if insErr := insertCandidateDependencies(ctx, tx, ctxn); insErr != nil {
			err = insErr
			return 0, err
		}
	}

	var newGeneration int64
	scanErr := tx.QueryRow(ctx, `
		UPDATE mempool_state
		SET initialized = TRUE,
		    generation = generation + 1,
		    core_tip_height = $1,
		    core_tip_hash = $2,
		    tx_count = $3,
		    total_vsize = $4,
		    total_fee_satoshis = $5,
		    observed_at = now()
		WHERE name = 'main'
		RETURNING generation
	`, candidate.CoreTipHeight, candidate.CoreTipHash, len(candidate.Transactions), totalVSize, totalFee,
	).Scan(&newGeneration)
	if scanErr != nil {
		err = fmt.Errorf("mempool: update state: %w", scanErr)
		return 0, err
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		err = fmt.Errorf("mempool: commit replace snapshot: %w", commitErr)
		return 0, err
	}

	return newGeneration, nil
}

// insertCandidateTransactionBody inserts one candidate transaction and
// every child row EXCEPT mempool_dependencies (inputs, witness, outputs,
// output_addresses OR output_participants) within the caller's
// already-open transaction. Dependencies are inserted separately, in a
// later pass over the whole candidate — see ReplaceSnapshot's two-pass
// comment.
func insertCandidateTransactionBody(ctx context.Context, tx pgx.Tx, ctxn CandidateTransaction) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO mempool_transactions
			(txid, wtxid, version, locktime, size, vsize, weight, fee_satoshis, entry_time, entry_height, replaceable)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, ctxn.TxID, ctxn.WTxID, int64(ctxn.Version), int64(ctxn.LockTime), ctxn.Size, ctxn.VSize, ctxn.Weight,
		ctxn.FeeSatoshis, ctxn.EntryTime, ctxn.EntryHeight, ctxn.Replaceable); err != nil {
		return fmt.Errorf("mempool: insert transaction %s: %w", ctxn.TxID, err)
	}

	for _, in := range ctxn.Inputs {
		// candidate.validate() already required every input on every
		// candidate transaction to have a non-nil PreviousOut (no
		// coinbase-shaped input is ever reachable here).
		if _, err := tx.Exec(ctx, `
			INSERT INTO mempool_inputs (txid, vin_index, prev_txid, prev_vout_index, script_sig, sequence)
			VALUES ($1,$2,$3,$4,$5,$6)
		`, ctxn.TxID, int64(in.Index), in.PreviousOut.TxID, int64(in.PreviousOut.Index), in.ScriptSig, int64(in.Sequence)); err != nil {
			return fmt.Errorf("mempool: insert input %s:%d: %w", ctxn.TxID, in.Index, err)
		}

		for itemIndex, item := range in.Witness {
			if _, err := tx.Exec(ctx, `
				INSERT INTO mempool_input_witness (txid, vin_index, item_index, data)
				VALUES ($1,$2,$3,$4)
			`, ctxn.TxID, int64(in.Index), itemIndex, item); err != nil {
				return fmt.Errorf("mempool: insert witness %s:%d[%d]: %w", ctxn.TxID, in.Index, itemIndex, err)
			}
		}
	}

	for _, out := range ctxn.Outputs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO mempool_outputs (txid, vout_index, value_satoshis, script_pubkey, script_type, witness_version, witness_program)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, ctxn.TxID, int64(out.Index), out.Value.Satoshis(), out.ScriptPubKey, string(out.ScriptType), out.WitnessVersion, out.WitnessProgram); err != nil {
			return fmt.Errorf("mempool: insert output %s:%d: %w", ctxn.TxID, out.Index, err)
		}

		switch out.ScriptType {
		case script.TypeMultisig:
			for i, addr := range out.ParticipantAddresses {
				if _, err := tx.Exec(ctx, `
					INSERT INTO mempool_output_participants (txid, vout_index, address, pubkey)
					VALUES ($1,$2,$3,$4)
				`, ctxn.TxID, int64(out.Index), addr, out.PubKeys[i]); err != nil {
					return fmt.Errorf("mempool: insert participant %s:%d: %w", ctxn.TxID, out.Index, err)
				}
			}
		default:
			if out.Address != "" {
				if _, err := tx.Exec(ctx, `
					INSERT INTO mempool_output_addresses (txid, vout_index, address)
					VALUES ($1,$2,$3)
				`, ctxn.TxID, int64(out.Index), out.Address); err != nil {
					return fmt.Errorf("mempool: insert address %s:%d: %w", ctxn.TxID, out.Index, err)
				}
			}
		}
	}

	return nil
}

// insertCandidateDependencies inserts every mempool_dependencies row for
// one candidate transaction. Called only after insertCandidateTransactionBody
// has already run for EVERY transaction in the candidate (ReplaceSnapshot's
// pass 2), so every depends_on_txid this candidate could legally reference
// (validate() already required it be part of the same candidate) is
// guaranteed to already exist as a mempool_transactions row — the FK can
// never spuriously fail due to insertion order.
func insertCandidateDependencies(ctx context.Context, tx pgx.Tx, ctxn CandidateTransaction) error {
	for _, dep := range ctxn.Depends {
		if _, err := tx.Exec(ctx, `
			INSERT INTO mempool_dependencies (txid, depends_on_txid) VALUES ($1,$2)
		`, ctxn.TxID, dep); err != nil {
			return fmt.Errorf("mempool: insert dependency %s -> %s: %w", ctxn.TxID, dep, err)
		}
	}
	return nil
}
