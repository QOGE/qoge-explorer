package query

import (
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Phase 2D.1 read-only query engine. It wraps the same
// *pgxpool.Pool internal/store.Store writes through, but never issues
// anything other than SELECT — see doc.go and
// docs/ARCHITECTURE.md §19.
type Store struct {
	pool *pgxpool.Pool
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
