package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/QOGE/qoge-explorer/internal/chain"
	"github.com/jackc/pgx/v5"
)

// AddressSummary is an address's canonical, derived balance-accounting
// state — read directly from the `addresses` cache internal/store
// maintains (recomputeAddress), never independently recomputed here.
// Multisig participant identities (output_participants) never contribute
// to any of these fields — see docs/ARCHITECTURE.md §7/§13.A.
type AddressSummary struct {
	Address               string `json:"address"`
	BalanceSatoshis       int64  `json:"balance_sats"`
	BalanceQOGE           string `json:"balance_qoge"`
	TotalReceivedSatoshis int64  `json:"total_received_sats"`
	TotalReceivedQOGE     string `json:"total_received_qoge"`
	TotalSentSatoshis     int64  `json:"total_sent_sats"`
	TotalSentQOGE         string `json:"total_sent_qoge"`
	TxCount               int    `json:"tx_count"`
	FirstSeenHeight       *int64 `json:"first_seen_height,omitempty"`
	LastSeenHeight        *int64 `json:"last_seen_height,omitempty"`
}

// AddressSummary returns address's current canonical balance. An address
// with no `addresses` row (never received a canonical output, or every
// output that ever named it was rolled back — see recomputeAddress's
// "phantom all-zero entry" deletion in internal/store/apply.go) is not an
// error: it returns a zero-value summary (balance/received/sent/tx_count
// all zero) rather than ErrNotFound, because the schema has no concept of
// address validity to distinguish "malformed address" from "syntactically
// fine address with no on-chain history" — see docs/ARCHITECTURE.md §19.
func (s *Store) AddressSummary(ctx context.Context, address string) (AddressSummary, error) {
	sum := AddressSummary{Address: address}
	var received, sent, balance int64
	var txCount int
	var firstSeen, lastSeen *int64

	err := s.pool.QueryRow(ctx, `
		SELECT total_received_satoshis, total_sent_satoshis, balance_satoshis, tx_count, first_seen_height, last_seen_height
		FROM addresses WHERE address = $1
	`, address).Scan(&received, &sent, &balance, &txCount, &firstSeen, &lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		sum.BalanceQOGE = chain.Amount(0).String()
		sum.TotalReceivedQOGE = chain.Amount(0).String()
		sum.TotalSentQOGE = chain.Amount(0).String()
		return sum, nil
	}
	if err != nil {
		return AddressSummary{}, fmt.Errorf("query: address %s: %w", address, err)
	}

	sum.TotalReceivedSatoshis = received
	sum.TotalSentSatoshis = sent
	sum.BalanceSatoshis = balance
	sum.TxCount = txCount
	sum.FirstSeenHeight = firstSeen
	sum.LastSeenHeight = lastSeen
	sum.TotalReceivedQOGE = chain.Amount(received).String()
	sum.TotalSentQOGE = chain.Amount(sent).String()
	sum.BalanceQOGE = chain.Amount(balance).String()
	return sum, nil
}

// AddressHistoryEntry is one transaction that touched an address, either as
// a real monetary destination (received an output) or as the spender of a
// previously-received output.
type AddressHistoryEntry struct {
	TxID        string `json:"txid"`
	BlockHash   string `json:"block_hash"`
	BlockHeight int64  `json:"block_height"`
}

// AddressHistoryPage is one bounded, keyset-paginated page of an address's
// transaction history, newest first.
type AddressHistoryPage struct {
	Transactions []AddressHistoryEntry `json:"transactions"`
	// NextBeforeHeight/NextBeforeTxID, when both non-nil, are the cursor a
	// caller should pass back in to fetch the next page.
	NextBeforeHeight *int64  `json:"next_before_height,omitempty"`
	NextBeforeTxID   *string `json:"next_before_txid,omitempty"`
}

// AddressHistory returns address's canonical transaction history,
// newest-first, keyset-paginated by (block height, txid). beforeHeight/
// beforeTxID must both be nil (first page) or both non-nil (a cursor
// returned by a previous call). Built entirely from utxo_state
// (creation/spending occurrences), which internal/store already keeps
// canonical-only via RollbackTo — an orphaned output's creation or spend
// never appears here. Multisig participant identities never appear in this
// history — searching by participant address, if ever exposed, must remain
// a structurally separate query (docs/ARCHITECTURE.md §13.A).
func (s *Store) AddressHistory(ctx context.Context, address string, beforeHeight *int64, beforeTxID *string, pageSize int) (AddressHistoryPage, error) {
	pageSize = clampPageSize(pageSize)

	rows, err := s.pool.Query(ctx, `
		WITH addr_utxos AS (
			SELECT o.txid AS created_txid, u.creation_block_hash, u.creation_block_height,
			       u.spent, u.spending_txid, u.spending_block_hash, u.spending_block_height
			FROM output_addresses oa
			JOIN transaction_outputs o ON o.txid = oa.txid AND o.vout_index = oa.vout_index
			JOIN utxo_state u ON u.txid = oa.txid AND u.vout_index = oa.vout_index
			WHERE oa.address = $1
		),
		touches AS (
			SELECT created_txid AS txid, creation_block_hash AS block_hash, creation_block_height AS height
			FROM addr_utxos
			UNION
			SELECT spending_txid AS txid, spending_block_hash AS block_hash, spending_block_height AS height
			FROM addr_utxos WHERE spent
		)
		SELECT DISTINCT txid, block_hash, height
		FROM touches
		WHERE $2::bigint IS NULL OR height < $2 OR (height = $2 AND txid < $3)
		ORDER BY height DESC, txid DESC
		LIMIT $4
	`, address, beforeHeight, beforeTxID, pageSize)
	if err != nil {
		return AddressHistoryPage{}, fmt.Errorf("query: address history for %s: %w", address, err)
	}
	defer rows.Close()

	var page AddressHistoryPage
	for rows.Next() {
		var e AddressHistoryEntry
		if err := rows.Scan(&e.TxID, &e.BlockHash, &e.BlockHeight); err != nil {
			return AddressHistoryPage{}, fmt.Errorf("query: address history for %s: scan: %w", address, err)
		}
		page.Transactions = append(page.Transactions, e)
	}
	if err := rows.Err(); err != nil {
		return AddressHistoryPage{}, fmt.Errorf("query: address history for %s: %w", address, err)
	}

	if len(page.Transactions) == pageSize {
		last := page.Transactions[len(page.Transactions)-1]
		h, t := last.BlockHeight, last.TxID
		page.NextBeforeHeight = &h
		page.NextBeforeTxID = &t
	}
	return page, nil
}
