package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestRichListEndpoint_Uninitialized is spec item 48: an uninitialized
// database is a valid HTTP 200 with an empty entries list.
func TestRichListEndpoint_Uninitialized(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/api/v1/richlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		IndexedHeight int64   `json:"indexed_height"`
		IndexedHash   *string `json:"indexed_block_hash"`
		Entries       []any   `json:"entries"`
	}
	decodeBody(t, rec, &body)
	if body.IndexedHeight != -1 {
		t.Fatalf("indexed_height = %d, want -1", body.IndexedHeight)
	}
	if body.IndexedHash != nil {
		t.Fatalf("indexed_block_hash = %v, want null", body.IndexedHash)
	}
	if len(body.Entries) != 0 {
		t.Fatalf("entries = %v, want empty", body.Entries)
	}
}

// TestRichListEndpoint_GenesisOnly is spec item 48: genesis-only chain also
// reports an empty entries list.
func TestRichListEndpoint_GenesisOnly(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestServerWithPool(t)

	g := block("api-richlist-genesis-g", 0, "", coinbaseTx("api-richlist-genesis-g", 100_00000000, "qApiRichlistGenesisG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/richlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []any `json:"entries"`
	}
	decodeBody(t, rec, &body)
	if len(body.Entries) != 0 {
		t.Fatalf("entries = %v, want empty", body.Entries)
	}
}

// TestRichListEndpoint_GoldenPath confirms exact rank/address/sats/qoge
// formatting for a normal ranking.
func TestRichListEndpoint_GoldenPath(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestServerWithPool(t)

	g := block("api-richlist-golden-g", 0, "", coinbaseTx("api-richlist-golden-g", 100_00000000, "qApiRichlistGoldenG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("api-richlist-golden-h1", 1, g.Hash, coinbaseTx("api-richlist-golden-h1", 50_00000000, "qApiRichlistGoldenA"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/richlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		IndexedHeight int64  `json:"indexed_height"`
		IndexedHash   string `json:"indexed_block_hash"`
		Entries       []struct {
			Rank        int    `json:"rank"`
			Address     string `json:"address"`
			BalanceSats int64  `json:"balance_sats"`
			BalanceQOGE string `json:"balance_qoge"`
		} `json:"entries"`
	}
	decodeBody(t, rec, &body)
	if body.IndexedHeight != 1 || body.IndexedHash != h1.Hash {
		t.Fatalf("anchor = (%d, %s), want (1, %s)", body.IndexedHeight, body.IndexedHash, h1.Hash)
	}
	if len(body.Entries) != 1 {
		t.Fatalf("entries = %+v, want exactly 1", body.Entries)
	}
	e := body.Entries[0]
	if e.Rank != 1 || e.Address != "qApiRichlistGoldenA" || e.BalanceSats != 50_00000000 || e.BalanceQOGE != "50.00000000" {
		t.Fatalf("entries[0] = %+v, want rank=1 address=qApiRichlistGoldenA balance_sats=%d balance_qoge=50.00000000", e, int64(50_00000000))
	}
}

// TestRichListEndpoint_ExactJSONContract is spec item 52: the response must
// contain exactly the documented top-level and entry keys, and must never
// leak a holder/entity/supply-percentage field.
func TestRichListEndpoint_ExactJSONContract(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestServerWithPool(t)

	g := block("api-richlist-contract-g", 0, "", coinbaseTx("api-richlist-contract-g", 100_00000000, "qApiRichlistContractG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	h1 := block("api-richlist-contract-h1", 1, g.Hash, coinbaseTx("api-richlist-contract-h1", 50_00000000, "qApiRichlistContractA"))
	if err := st.ApplyBlock(ctx, h1); err != nil {
		t.Fatalf("apply h1: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/richlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response as map: %v (body=%s)", err, rec.Body.String())
	}
	wantTop := map[string]bool{"indexed_height": true, "indexed_block_hash": true, "entries": true}
	if len(raw) != len(wantTop) {
		t.Fatalf("got %d top-level keys, want %d: got=%v", len(raw), len(wantTop), raw)
	}
	for k := range raw {
		if !wantTop[k] {
			t.Fatalf("unexpected top-level key %q: %v", k, raw)
		}
	}

	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw["entries"], &entries); err != nil {
		t.Fatalf("decode entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want exactly 1", entries)
	}
	wantEntry := map[string]bool{"rank": true, "address": true, "balance_sats": true, "balance_qoge": true}
	if len(entries[0]) != len(wantEntry) {
		t.Fatalf("got %d entry keys, want %d: got=%v", len(entries[0]), len(wantEntry), entries[0])
	}
	for k := range entries[0] {
		if !wantEntry[k] {
			t.Fatalf("unexpected entry key %q: %v", k, entries[0])
		}
	}

	forbidden := []string{
		"total_supply", "percentage", "percentage_of_supply", "percent_supply",
		"supply_share", "ownership_share", "concentration", "top_10_share",
		"top_100_share", "holder", "owner", "entity",
		"excluded_output", "circulating_supply", "issued_supply",
	}
	body := rec.Body.String()
	for _, k := range forbidden {
		if _, ok := raw[k]; ok {
			t.Fatalf("forbidden top-level key %q present: %s", k, body)
		}
		if _, ok := entries[0][k]; ok {
			t.Fatalf("forbidden entry key %q present: %s", k, body)
		}
	}

	if v := string(raw["indexed_height"]); len(v) == 0 || v[0] == '"' {
		t.Fatalf("indexed_height = %s, want a JSON integer, not a string", v)
	}
	if v := string(entries[0]["rank"]); len(v) == 0 || v[0] == '"' {
		t.Fatalf("rank = %s, want a JSON integer, not a string", v)
	}
	if v := string(entries[0]["balance_sats"]); len(v) == 0 || v[0] == '"' {
		t.Fatalf("balance_sats = %s, want a JSON integer, not a string", v)
	}
	if v := string(entries[0]["balance_qoge"]); len(v) == 0 || v[0] != '"' {
		t.Fatalf("balance_qoge = %s, want a JSON string", v)
	}
}

// TestRichListEndpoint_MethodNotAllowed mirrors
// TestSupplyEndpoint_MethodNotAllowed for the new richlist route.
func TestRichListEndpoint_MethodNotAllowed(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "POST", "/api/v1/richlist")
	assertJSONError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow == "" {
		t.Fatalf("missing Allow header")
	}
}
