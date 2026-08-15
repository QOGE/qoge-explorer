package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestSupplyEndpoint_Uninitialized is spec item 41: a freshly migrated
// database with no blocks is a valid HTTP 200 response, not "chain supply
// of zero" — every total is zero and indexed_height is -1.
func TestSupplyEndpoint_Uninitialized(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "GET", "/api/v1/supply")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		IndexedHeight            int64   `json:"indexed_height"`
		IndexedHash              *string `json:"indexed_block_hash"`
		ScheduledSubsidySatoshis int64   `json:"scheduled_subsidy_sats"`
		ScheduledSubsidyQOGE     string  `json:"scheduled_subsidy_qoge"`
		UTXOSetValueSatoshis     int64   `json:"utxo_set_value_sats"`
	}
	decodeBody(t, rec, &body)
	if body.IndexedHeight != -1 {
		t.Fatalf("indexed_height = %d, want -1", body.IndexedHeight)
	}
	if body.IndexedHash != nil {
		t.Fatalf("indexed_block_hash = %v, want null", body.IndexedHash)
	}
	if body.ScheduledSubsidySatoshis != 0 || body.UTXOSetValueSatoshis != 0 {
		t.Fatalf("totals = %+v, want all zero", body)
	}
	if body.ScheduledSubsidyQOGE != "0.00000000" {
		t.Fatalf("scheduled_subsidy_qoge = %q, want 0.00000000", body.ScheduledSubsidyQOGE)
	}
}

// TestSupplyEndpoint_GoldenPath applies one genesis block and confirms the
// endpoint reports its exact monetary facts, in both integer satoshis and
// the formatted QOGE string — never a float, never scientific notation
// (spec item 54).
func TestSupplyEndpoint_GoldenPath(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestServerWithPool(t)

	g := block("api-supply-g", 0, "", coinbaseTx("api-supply-g", 100_00000000, "qApiSupplyG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/supply")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		IndexedHeight            int64  `json:"indexed_height"`
		ScheduledSubsidySatoshis int64  `json:"scheduled_subsidy_sats"`
		ScheduledSubsidyQOGE     string `json:"scheduled_subsidy_qoge"`
		CoinbaseOutputsSatoshis  int64  `json:"coinbase_outputs_sats"`
		UnclaimedRewardSatoshis  int64  `json:"unclaimed_reward_sats"`
	}
	decodeBody(t, rec, &body)
	if body.IndexedHeight != 0 {
		t.Fatalf("indexed_height = %d, want 0", body.IndexedHeight)
	}
	if body.ScheduledSubsidySatoshis != 100_00000000 {
		t.Fatalf("scheduled_subsidy_sats = %d, want %d", body.ScheduledSubsidySatoshis, 100_00000000)
	}
	if body.ScheduledSubsidyQOGE != "100.00000000" {
		t.Fatalf("scheduled_subsidy_qoge = %q, want 100.00000000", body.ScheduledSubsidyQOGE)
	}
	if body.CoinbaseOutputsSatoshis != 100_00000000 {
		t.Fatalf("coinbase_outputs_sats = %d, want %d", body.CoinbaseOutputsSatoshis, 100_00000000)
	}
	if body.UnclaimedRewardSatoshis != 0 {
		t.Fatalf("unclaimed_reward_sats = %d, want 0", body.UnclaimedRewardSatoshis)
	}
}

// TestSupplyEndpoint_ExactJSONContract is spec item 4: the golden-path JSON
// response must contain EXACTLY the twelve documented keys — sats fields as
// JSON integer numbers, qoge fields as JSON strings — and must never leak
// cumulative_excluded_output_satoshis or any circulating/issued/minted
// supply field, regardless of naming.
func TestSupplyEndpoint_ExactJSONContract(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestServerWithPool(t)

	g := block("api-supply-contract-g", 0, "", coinbaseTx("api-supply-contract-g", 100_00000000, "qApiSupplyContractG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/supply")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response as map: %v (body=%s)", err, rec.Body.String())
	}

	wantSatsKeys := []string{
		"indexed_height",
		"scheduled_subsidy_sats",
		"transaction_fees_sats",
		"coinbase_outputs_sats",
		"unclaimed_reward_sats",
		"utxo_set_value_sats",
	}
	wantQOGEKeys := []string{
		"scheduled_subsidy_qoge",
		"transaction_fees_qoge",
		"coinbase_outputs_qoge",
		"unclaimed_reward_qoge",
		"utxo_set_value_qoge",
	}
	wantOtherKeys := []string{"indexed_block_hash"}

	wantKeys := map[string]bool{}
	for _, k := range wantSatsKeys {
		wantKeys[k] = true
	}
	for _, k := range wantQOGEKeys {
		wantKeys[k] = true
	}
	for _, k := range wantOtherKeys {
		wantKeys[k] = true
	}

	if len(raw) != len(wantKeys) {
		t.Fatalf("got %d keys, want %d: got=%v", len(raw), len(wantKeys), raw)
	}
	for k := range raw {
		if !wantKeys[k] {
			t.Fatalf("unexpected key %q in response: %v", k, raw)
		}
	}
	for k := range wantKeys {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing expected key %q in response: %v", k, raw)
		}
	}

	forbidden := []string{
		"cumulative_excluded_output_satoshis",
		"excluded_output_sats",
		"circulating_supply",
		"circulating_supply_sats",
		"issued_supply",
		"minted_supply",
	}
	for _, k := range forbidden {
		if _, ok := raw[k]; ok {
			t.Fatalf("forbidden key %q present in response: %v", k, raw)
		}
	}

	for _, k := range wantSatsKeys {
		v := string(raw[k])
		if len(v) == 0 || v[0] == '"' {
			t.Fatalf("%s = %s, want a JSON integer number, not a string", k, v)
		}
	}
	for _, k := range wantQOGEKeys {
		v := string(raw[k])
		if len(v) == 0 || v[0] != '"' {
			t.Fatalf("%s = %s, want a JSON string, not a bare number", k, v)
		}
	}
}

// TestSupplyEndpoint_RollupUnavailable is spec item 21: an initialized
// chain whose indexed tip has no block_supply_rollup row must return HTTP
// 503 "supply_rollup_unavailable", never a partial/understated 200.
func TestSupplyEndpoint_RollupUnavailable(t *testing.T) {
	ctx := context.Background()
	s, st, pool := newTestServerWithPool(t)

	g := block("api-supply-unavail-g", 0, "", coinbaseTx("api-supply-unavail-g", 100_00000000, "qApiSupplyUnavailG"))
	if err := st.ApplyBlock(ctx, g); err != nil {
		t.Fatalf("apply genesis: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM block_supply_rollup WHERE block_hash = $1", g.Hash); err != nil {
		t.Fatalf("delete block_supply_rollup: %v", err)
	}

	rec := doRequest(t, s, "GET", "/api/v1/supply")
	assertJSONError(t, rec, http.StatusServiceUnavailable, "supply_rollup_unavailable")
}

// TestSupplyEndpoint_MethodNotAllowed mirrors
// TestDeploymentsEndpoint_MethodNotAllowed for the new supply route.
func TestSupplyEndpoint_MethodNotAllowed(t *testing.T) {
	s, _, _ := newTestServerWithPool(t)

	rec := doRequest(t, s, "POST", "/api/v1/supply")
	assertJSONError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); allow == "" {
		t.Fatalf("missing Allow header")
	}
}
