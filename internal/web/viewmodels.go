package web

import (
	"net/url"
	"strconv"

	"github.com/QOGE/qoge-explorer/internal/query"
)

// pagination backs templates/pagination.tmpl. It carries only a
// ready-to-use href — no cursor internals — so the template stays a dumb
// renderer.
type pagination struct {
	HasNext bool
	NextURL string
}

// blocksPagination builds the "next page" link for /blocks. limit is the
// caller's originally-requested "limit" query parameter (0 meaning
// unspecified/default — see parsePageSize) and is carried through to the
// next link so paging forward doesn't silently reset back to
// query.DefaultPageSize.
func blocksPagination(limit int, next *int64) pagination {
	if next == nil {
		return pagination{}
	}
	v := url.Values{}
	v.Set("before", strconv.FormatInt(*next, 10))
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	return pagination{HasNext: true, NextURL: "/blocks?" + v.Encode()}
}

// addressPagination builds the "next page" link for /address/{address}.
// limit is preserved the same way blocksPagination preserves it.
func addressPagination(address string, limit int, nextHeight *int64, nextTxID *string) pagination {
	if nextHeight == nil || nextTxID == nil {
		return pagination{}
	}
	v := url.Values{}
	v.Set("before", strconv.FormatInt(*nextHeight, 10))
	v.Set("before_txid", *nextTxID)
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	return pagination{HasNext: true, NextURL: "/address/" + url.PathEscape(address) + "?" + v.Encode()}
}

// homeView backs templates/home.tmpl.
type homeView struct {
	Status       query.Status
	RecentBlocks []query.BlockSummary
}

// blocksView backs templates/blocks.tmpl. FirstPage is true only when the
// request had no "before" cursor — i.e. this page's first row is the
// current canonical tip, not merely a historical page. Only that page is
// live-refresh eligible (see templates/blocks.tmpl's data-live-refresh
// attribute and docs/ARCHITECTURE.md §21): a historical/paginated
// /blocks?before=... page must never auto-reload just because the global
// tip advances.
type blocksView struct {
	Blocks     []query.BlockSummary
	Pagination pagination
	FirstPage  bool
}

// txView backs templates/tx.tmpl: query.TransactionDetail plus whether the
// caller opted into raw witness data, so the template can render the
// correct toggle link (see docs/ARCHITECTURE.md §20 "P2QPK large-witness
// default-hidden policy").
type txView struct {
	query.TransactionDetail
	IncludeWitness bool
}

// addressView backs templates/address.tmpl.
type addressView struct {
	Summary    query.AddressSummary
	History    []query.AddressHistoryEntry
	Pagination pagination
}

// searchView backs templates/search.tmpl — rendered only when q looked
// like a 64-char hash but matched none of BlockByHash/TransactionByTxID/
// TransactionByWTxID (a genuine "not found", not a malformed-input 400).
type searchView struct {
	Query string
}

// mempoolPagination builds the "next page" link for /mempool. Unlike
// blocksPagination/addressPagination (a single-value or two-value cursor),
// a mempool page must carry all three MempoolCursor fields — generation
// included — so a stale link (the mempool having since been replaced)
// fails loudly via ErrMempoolGenerationChanged rather than silently
// pointing at the wrong snapshot.
func mempoolPagination(limit int, next *query.MempoolCursor) pagination {
	if next == nil {
		return pagination{}
	}
	v := url.Values{}
	v.Set("generation", strconv.FormatInt(next.Generation, 10))
	v.Set("before_entry_time", strconv.FormatInt(next.EntryTime, 10))
	v.Set("before_txid", next.TxID)
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	return pagination{HasNext: true, NextURL: "/mempool?" + v.Encode()}
}

// mempoolView backs templates/mempool.tmpl. FirstPage is true only when the
// request had no cursor at all — mirrors blocksView.FirstPage's rationale,
// though mempool pages do not currently participate in live.js auto-refresh
// (deliberately deferred — see docs/ARCHITECTURE.md §23 "No browser mempool
// polling yet").
type mempoolView struct {
	Overview   query.MempoolOverview
	Pagination pagination
	FirstPage  bool
}

// mempoolTxView backs templates/mempooltx.tmpl: query.MempoolTransactionDetail
// plus whether the caller opted into raw witness data — mirrors txView
// exactly.
type mempoolTxView struct {
	query.MempoolTransactionDetail
	IncludeWitness bool
}
