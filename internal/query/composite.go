package query

import "context"

// ExplorerOverview is the home page's Status + RecentBlocks pair, read from
// ONE read-only REPEATABLE READ snapshot (readTx) — the same snapshot-
// consistency property BlockByHash/BlockByHeight/TransactionByTxID already
// give a single multi-statement detail response, extended here to a
// multi-CALL composite response. Without this, a concurrent ApplyBlock/
// RollbackTo committing between the two independent SELECTs could make one
// rendered page describe two different canonical states (e.g. Status
// reporting a tip that RecentBlocks's own first row disagrees with) — see
// docs/ARCHITECTURE.md §20 "Composite read snapshots".
type ExplorerOverview struct {
	Status       Status
	RecentBlocks BlockPage
}

// ExplorerOverview reads Status and the newest recentBlocksPageSize
// canonical blocks from one snapshot. internal/web's handleHome is the only
// caller; it never independently calls Status/RecentBlocks itself.
func (s *Store) ExplorerOverview(ctx context.Context, recentBlocksPageSize int) (ExplorerOverview, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return ExplorerOverview{}, err
	}
	defer done()

	status, err := statusFrom(ctx, tx)
	if err != nil {
		return ExplorerOverview{}, err
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first statement

	blocks, err := recentBlocksFrom(ctx, tx, nil, recentBlocksPageSize)
	if err != nil {
		return ExplorerOverview{}, err
	}

	return ExplorerOverview{Status: status, RecentBlocks: blocks}, nil
}

// AddressDetail is the address page's AddressSummary + AddressHistory pair,
// read from ONE read-only REPEATABLE READ snapshot — same rationale as
// ExplorerOverview. Without this, a concurrent reorg committing between the
// two independent SELECTs could render a balance that belongs to one
// canonical branch alongside a transaction history that belongs to
// another — a database state that never existed on either branch.
type AddressDetail struct {
	Summary AddressSummary
	History AddressHistoryPage
}

// AddressDetail reads address's AddressSummary and one page of
// AddressHistory from one snapshot. internal/web's handleAddress is the
// only caller; it never independently calls AddressSummary/AddressHistory
// itself. Parameters mirror AddressHistory's exactly.
func (s *Store) AddressDetail(ctx context.Context, address string, beforeHeight *int64, beforeTxID *string, pageSize int) (AddressDetail, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return AddressDetail{}, err
	}
	defer done()

	summary, err := addressSummaryFrom(ctx, tx, address)
	if err != nil {
		return AddressDetail{}, err
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first statement

	history, err := addressHistoryFrom(ctx, tx, address, beforeHeight, beforeTxID, pageSize)
	if err != nil {
		return AddressDetail{}, err
	}

	return AddressDetail{Summary: summary, History: history}, nil
}
