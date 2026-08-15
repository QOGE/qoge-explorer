package query

import (
	"context"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
)

// RichListLimit is Phase 2H.3's hard bound on the ranked address balance
// list: exactly the top 100 positive addresses.balance_satoshis rows. No
// pagination, no OFFSET, no unlimited endpoint — see
// docs/ARCHITECTURE.md §29.
const RichListLimit = 100

// RichListEntry is one ranked ADDRESS balance — not a holder, wallet, or
// entity (see docs/ARCHITECTURE.md §29). Rank is the entry's 1-based
// POSITION in the list, not a statistical dense rank: because ties are
// broken by a deterministic address ordering, two equal-balance addresses
// still receive distinct, stable ranks.
type RichListEntry struct {
	Rank            int    `json:"rank"`
	Address         string `json:"address"`
	BalanceSatoshis int64  `json:"balance_sats"`
	BalanceQOGE     string `json:"balance_qoge"`
}

// RichListOverview is Phase 2H.3's public ranked-address-balance read
// surface: the top RichListLimit positive addresses.balance_satoshis
// rows, read from one read-only REPEATABLE READ snapshot alongside the
// sync_state anchor (readTx) — same snapshot-consistency property
// SupplyOverview/ExplorerOverview/DeploymentOverview already give their
// own composite responses.
//
// SUM(Entries[i].BalanceSatoshis) is NOT necessarily equal to the current
// UTXO-set value (query.SupplyOverview.UTXOSetValueSatoshis): the
// addresses cache deliberately does not attribute every UTXO to an
// address — bare-multisig participant identities (output_participants)
// are never balance-accounting destinations, and a spendable output with
// no recognized destination address contributes to UTXO-set value but to
// no addresses row at all (docs/ARCHITECTURE.md §29).
type RichListOverview struct {
	IndexedHeight int64   `json:"indexed_height"`
	IndexedHash   *string `json:"indexed_block_hash"`

	Entries []RichListEntry `json:"entries"`
}

// richListEntriesFrom is RichListOverview's ONLY monetary data source: the
// existing addresses.balance_satoshis cache internal/store already
// maintains (recomputeAddress) — never a request-time SUM over
// transaction_outputs/output_addresses/transaction_inputs/utxo_state (see
// docs/ARCHITECTURE.md §29). Ordered by balance_satoshis DESC, then a
// deterministic address tie-break (address COLLATE "C" ASC, matching
// migrations/0006_addresses_richlist_index.up.sql's own key order exactly
// so PostgreSQL can walk that index directly), so equal-balance addresses
// still receive a stable, repeatable order across calls.
func richListEntriesFrom(ctx context.Context, q querier) ([]RichListEntry, error) {
	rows, err := q.Query(ctx, `
		SELECT address, balance_satoshis
		FROM addresses
		WHERE balance_satoshis > 0
		ORDER BY balance_satoshis DESC, address COLLATE "C" ASC
		LIMIT $1
	`, RichListLimit)
	if err != nil {
		return nil, fmt.Errorf("query: richlist entries: %w", err)
	}
	defer rows.Close()

	entries := make([]RichListEntry, 0, RichListLimit)
	for rows.Next() {
		var address string
		var balance int64
		if err := rows.Scan(&address, &balance); err != nil {
			return nil, fmt.Errorf("query: richlist entries: scan: %w", err)
		}
		entries = append(entries, RichListEntry{
			Rank:            len(entries) + 1,
			Address:         address,
			BalanceSatoshis: balance,
			BalanceQOGE:     chain.Amount(balance).String(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: richlist entries: %w", err)
	}
	return entries, nil
}

// RichListOverview reads Phase 2H.3's ranked address balance list from one
// snapshot: the sync_state anchor, then the top RichListLimit positive
// addresses.balance_satoshis rows — a concurrent ApplyBlock/RollbackTo
// committing between these two reads can therefore never produce a
// response mixing height/hash from one canonical state with a ranking
// from another (same REPEATABLE-READ guarantee SupplyOverview already
// gives its own composite response). An uninitialized explorer
// (sync_state.indexed_height == -1) and a genesis-only explorer (genesis
// outputs are never placed in the addresses cache the same way they're
// never placed in utxo_state) both legitimately return an empty entries
// list with HTTP 200 — never an error.
func (s *Store) RichListOverview(ctx context.Context) (RichListOverview, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return RichListOverview{}, err
	}
	defer done()

	status, err := statusFrom(ctx, tx)
	if err != nil {
		return RichListOverview{}, err
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first statement

	entries, err := richListEntriesFrom(ctx, tx)
	if err != nil {
		return RichListOverview{}, err
	}

	return RichListOverview{
		IndexedHeight: status.IndexedHeight,
		IndexedHash:   status.IndexedHash,
		Entries:       entries,
	}, nil
}
