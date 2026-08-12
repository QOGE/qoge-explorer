package deployments

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists deployment snapshots into chain_deployments and
// deployment_state (migrations/0001_initial.up.sql,
// 0003_deployment_state.up.sql). It never reads or writes any
// confirmed-chain or mempool_* table (docs/ARCHITECTURE.md §24) and knows
// nothing about Core RPC — see sync.go for snapshot acquisition.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an already-connected, already-migrated pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// State is a read-only view of the deployment_state('main') singleton
// row. Initialized distinguishes "never successfully observed" (false,
// the zero value of every other field) from "successfully observed"
// (true) — including a successfully-observed, currently-zero-BIP9-
// deployments snapshot, which is Initialized=true, DeploymentCount=0.
type State struct {
	Initialized     bool
	Generation      int64
	CoreTipHeight   *int64
	CoreTipHash     *string
	DeploymentCount int
	ObservedAt      *time.Time
}

// State returns the current deployment_state('main') row.
func (s *Store) State(ctx context.Context) (State, error) {
	var st State
	err := s.pool.QueryRow(ctx, `
		SELECT initialized, generation, core_tip_height, core_tip_hash,
		       deployment_count, observed_at
		FROM deployment_state WHERE name = 'main'
	`).Scan(&st.Initialized, &st.Generation, &st.CoreTipHeight, &st.CoreTipHash,
		&st.DeploymentCount, &st.ObservedAt)
	if err != nil {
		return State{}, fmt.Errorf("deployments: read state: %w", err)
	}
	return st, nil
}

// ReplaceSnapshot atomically replaces the entire deployment cache with
// candidate: one PostgreSQL transaction acquires the deployment_state row
// lock (serializing concurrent writers), removes every row of the
// previous snapshot, inserts the complete new one, updates
// deployment_state LAST, and commits. If ANY step fails, the whole
// transaction rolls back and the previous complete snapshot remains
// exactly as it was — a half-replaced deployment cache is never
// observable (docs/ARCHITECTURE.md §24, spec item 17).
//
// generation is incremented on every successful commit, including a
// snapshot whose deployment values happen to be identical to the
// previous one, or a non-empty -> empty (or empty -> non-empty)
// transition — never on a failed or skipped observation (spec items
// 18/19).
func (s *Store) ReplaceSnapshot(ctx context.Context, candidate Candidate) (generation int64, err error) {
	if verr := candidate.validate(); verr != nil {
		return 0, verr
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("deployments: begin replace snapshot: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// Serialize concurrent writers on the singleton state row before
	// touching anything else — the same row-lock-first pattern
	// internal/mempool.Store.ReplaceSnapshot and internal/store's
	// lockCheckpoint use for their own singleton/checkpoint mutations.
	if _, lockErr := tx.Exec(ctx, `SELECT 1 FROM deployment_state WHERE name = 'main' FOR UPDATE`); lockErr != nil {
		err = fmt.Errorf("deployments: lock state row: %w", lockErr)
		return 0, err
	}

	// Full replacement, not additive deltas: clear the entire previous
	// snapshot before inserting the new one.
	if _, delErr := tx.Exec(ctx, `DELETE FROM chain_deployments`); delErr != nil {
		err = fmt.Errorf("deployments: clear previous snapshot: %w", delErr)
		return 0, err
	}

	for _, d := range candidate.Deployments {
		if _, insErr := tx.Exec(ctx, `
			INSERT INTO chain_deployments (name, status, since_height, raw_json, checked_at)
			VALUES ($1, $2, $3, $4, $5)
		`, d.Name, d.Status, d.SinceHeight, d.RawJSON, candidate.ObservedAt); insErr != nil {
			err = fmt.Errorf("deployments: insert deployment %s: %w", d.Name, insErr)
			return 0, err
		}
	}

	var newGeneration int64
	scanErr := tx.QueryRow(ctx, `
		UPDATE deployment_state
		SET initialized = TRUE,
		    generation = generation + 1,
		    core_tip_height = $1,
		    core_tip_hash = $2,
		    deployment_count = $3,
		    observed_at = $4
		WHERE name = 'main'
		RETURNING generation
	`, candidate.CoreTipHeight, candidate.CoreTipHash, len(candidate.Deployments), candidate.ObservedAt,
	).Scan(&newGeneration)
	if scanErr != nil {
		err = fmt.Errorf("deployments: update state: %w", scanErr)
		return 0, err
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		err = fmt.Errorf("deployments: commit replace snapshot: %w", commitErr)
		return 0, err
	}

	return newGeneration, nil
}
