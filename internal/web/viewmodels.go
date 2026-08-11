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

// blocksView backs templates/blocks.tmpl.
type blocksView struct {
	Blocks     []query.BlockSummary
	Pagination pagination
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
