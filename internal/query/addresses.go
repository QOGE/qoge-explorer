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
	return addressSummaryFrom(ctx, s.pool, address)
}

// addressSummaryFrom runs AddressSummary's SELECT against any querier — see
// statusFrom's doc comment for why this shape exists.
func addressSummaryFrom(ctx context.Context, q querier, address string) (AddressSummary, error) {
	sum := AddressSummary{Address: address}
	var received, sent, balance int64
	var txCount int
	var firstSeen, lastSeen *int64

	err := q.QueryRow(ctx, `
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
// returned by a previous call).
//
// Built from IMMUTABLE destination/input relations
// (output_addresses/transaction_inputs) joined against canonical block
// OCCURRENCE state (block_transactions/blocks), never from utxo_state —
// utxo_state is deliberately missing a row for every genesis output and
// every script.IsUnspendable output (see Store.ApplyBlock's "Core UTXO
// semantics" doc comment in internal/store/apply.go), so gating history
// visibility on a utxo_state row existing would silently drop those
// outputs' real canonical transaction history even though
// transaction_outputs/output_addresses both persist them — see
// docs/ARCHITECTURE.md §19 "Address history vs. UTXO eligibility". Balance
// accounting (AddressSummary) is intentionally unaffected by this — it
// still reads the `addresses` cache exactly as before, so a genesis-only
// destination shows real history with zero spendable balance, not the
// other way around.
//
//   - RECEIVE side: output_addresses -> transaction_outputs (proves the
//     output really has this address, txid, vout) -> block_transactions
//     (that txid's occurrence) -> blocks WHERE canonical.
//   - SPEND side: output_addresses' (txid, vout_index) -> transaction_inputs
//     whose (prev_txid, prev_vout_index) reference exactly that output
//     (transaction_inputs is immutable per-transaction body data, kept
//     forever regardless of branch — see §3 "Reorg keeps an audit trail" —
//     so this alone would include orphaned spend attempts) -> the SPENDING
//     transaction's own block_transactions/blocks WHERE canonical, which is
//     what actually restricts this to canonical spends only.
//
// An orphaned output's creation or spend never appears here — both sides
// require a canonical block_transactions/blocks join, exactly mirroring
// how RollbackTo already keeps utxo_state/addresses canonical-only.
// Multisig participant identities (output_participants) never appear in
// this history — searching by participant address, if ever exposed, must
// remain a structurally separate query (docs/ARCHITECTURE.md §13.A).
func (s *Store) AddressHistory(ctx context.Context, address string, beforeHeight *int64, beforeTxID *string, pageSize int) (AddressHistoryPage, error) {
	return addressHistoryFrom(ctx, s.pool, address, beforeHeight, beforeTxID, pageSize)
}

// addressHistoryFrom runs AddressHistory's query against any querier — see
// statusFrom's doc comment for why this shape exists.
func addressHistoryFrom(ctx context.Context, q querier, address string, beforeHeight *int64, beforeTxID *string, pageSize int) (AddressHistoryPage, error) {
	pageSize = clampPageSize(pageSize)

	rows, err := q.Query(ctx, `
		WITH received AS (
			SELECT bt.txid, bt.block_hash, bt.block_height
			FROM output_addresses oa
			JOIN transaction_outputs o ON o.txid = oa.txid AND o.vout_index = oa.vout_index
			JOIN block_transactions bt ON bt.txid = oa.txid
			JOIN blocks b ON b.hash = bt.block_hash AND b.canonical
			WHERE oa.address = $1
		),
		spent AS (
			SELECT bt.txid, bt.block_hash, bt.block_height
			FROM output_addresses oa
			JOIN transaction_inputs ti ON ti.prev_txid = oa.txid AND ti.prev_vout_index = oa.vout_index
			JOIN block_transactions bt ON bt.txid = ti.txid
			JOIN blocks b ON b.hash = bt.block_hash AND b.canonical
			WHERE oa.address = $1
		),
		touches AS (
			SELECT txid, block_hash, block_height FROM received
			UNION
			SELECT txid, block_hash, block_height FROM spent
		)
		SELECT DISTINCT txid, block_hash, block_height
		FROM touches
		WHERE $2::bigint IS NULL OR block_height < $2 OR (block_height = $2 AND txid < $3)
		ORDER BY block_height DESC, txid DESC
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
