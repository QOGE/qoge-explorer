// Package store implements PostgreSQL persistence for the QOGE explorer.
//
// Phase 2B.1 scope is deliberately narrow: a connection helper and a
// minimal, dependency-light migration runner, plus the schema itself
// (migrations/*.sql at the repository root). The block-indexing write API
// (INSERT ... ON CONFLICT per block, UTXO/address maintenance, reorg
// rollback) is Phase 2B.2 — see docs/ARCHITECTURE.md §4/§5.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a PostgreSQL connection pool using pgx (a PostgreSQL-native
// driver, not database/sql — see docs/ARCHITECTURE.md §12 for why pgx v5
// was chosen). Callers are responsible for calling Close on the returned
// pool.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("store: empty database URL")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return pool, nil
}
