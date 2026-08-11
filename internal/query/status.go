package query

import (
	"context"
	"fmt"
)

// Status describes the INDEXED DATABASE's current canonical checkpoint —
// never Core's live tip (see docs/ARCHITECTURE.md §19: remote height/sync
// lag is out of scope for Phase 2D.1, since it requires composing the
// index and serve processes, which is deliberately not done yet).
type Status struct {
	IndexedHeight int64   `json:"indexed_height"`
	IndexedHash   *string `json:"indexed_block_hash"`
}

// Status reads sync_state('main') directly: the same durable checkpoint
// row internal/store.Store.Tip reads, without requiring a live
// internal/store.Store instance or its write-side dependencies.
// IndexedHeight is -1 and IndexedHash is nil for an explorer that has never
// completed a block (migration's bootstrap row).
func (s *Store) Status(ctx context.Context) (Status, error) {
	var st Status
	var hash *string
	err := s.pool.QueryRow(ctx,
		`SELECT indexed_height, indexed_block_hash FROM sync_state WHERE name = 'main'`,
	).Scan(&st.IndexedHeight, &hash)
	if err != nil {
		return Status{}, fmt.Errorf("query: status: %w", err)
	}
	st.IndexedHash = hash
	return st, nil
}
