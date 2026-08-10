package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Phase 2D.1 read-only query engine. It wraps the same
// *pgxpool.Pool internal/store.Store writes through, but never issues
// anything other than SELECT — see doc.go and
// docs/ARCHITECTURE.md §19.
type Store struct {
	pool *pgxpool.Pool
}

// querier is the minimal read surface *pgxpool.Pool and pgx.Tx share.
// Private helpers below take a querier rather than being hardcoded to
// s.pool, so the exact same SQL runs identically whether it's issued as an
// ordinary pool statement (single-statement methods, where PostgreSQL's
// own per-statement snapshot is already sufficient) or inside an explicit
// read-only REPEATABLE READ transaction (multi-statement detail
// responses, where every read must come from one consistent snapshot —
// see readTx below and docs/ARCHITECTURE.md §19 "Multi-statement read
// consistency"). This is deliberately just an interface, not a query
// builder/ORM.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readTx begins a READ ONLY, REPEATABLE READ transaction: PostgreSQL fixes
// its snapshot at that transaction's FIRST statement and holds it for
// every subsequent statement, so a multi-statement detail response (e.g.
// TransactionDetail's occurrences + witness + inputs + outputs/utxo_state)
// can never mix rows committed before the snapshot with rows committed by
// a concurrent writer after it — the exact property a concurrent reorg
// would otherwise be able to violate. It never acquires
// internal/store's canonical-mutation row lock (lockCheckpoint) and never
// blocks ApplyBlock/RollbackTo beyond PostgreSQL's ordinary MVCC behavior:
// a read-only transaction takes no locks a writer needs to wait on.
//
// The returned commit func must be deferred by the caller; it rolls the
// (read-only, so trivially abandonable) transaction back rather than
// committing — there is nothing to persist either way.
func (s *Store) readTx(ctx context.Context) (pgx.Tx, func(), error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("query: begin read snapshot: %w", err)
	}
	return tx, func() { _ = tx.Rollback(ctx) }, nil
}

// snapshotTestHook, when non-nil, is invoked synchronously immediately
// after a multi-statement query's read-only REPEATABLE READ transaction
// has executed its FIRST statement — the point at which PostgreSQL fixes
// that transaction's snapshot — and before any subsequent statement in the
// same transaction. It exists solely so this package's own tests can
// deterministically inject a concurrent mutation at that precise moment
// (never via sleep-based timing); always nil outside those tests.
var snapshotTestHook func()

func fireSnapshotTestHook() {
	if snapshotTestHook != nil {
		snapshotTestHook()
	}
}

// New wraps an already-connected pool (see internal/store.Connect) in a
// query Store. The same pool may safely be shared with a write
// internal/store.Store — read/write separation here is a Go-level API
// boundary, not a separate database connection or credential.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ErrNotFound means the requested object (block, transaction, address
// activity) does not exist in the indexed database. internal/api maps this
// to HTTP 404.
var ErrNotFound = errors.New("query: not found")

// MaxPageSize is the hard upper bound on any list endpoint's page size —
// callers requesting more get MaxPageSize, never an unbounded scan.
const MaxPageSize = 100

// DefaultPageSize is used when a caller does not specify a page size.
const DefaultPageSize = 25

// clampPageSize normalizes a caller-requested page size: <=0 becomes
// DefaultPageSize, and anything above MaxPageSize is clamped down to it.
func clampPageSize(n int) int {
	if n <= 0 {
		return DefaultPageSize
	}
	if n > MaxPageSize {
		return MaxPageSize
	}
	return n
}
