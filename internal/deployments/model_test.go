package deployments

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/QOGE/qoge-explorer/internal/rpc"
)

func fixedTime() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

// TestDecodeDeploymentInfo_StatusTransitions is spec item 30: strict
// decoding must succeed for every BIP9 status, without assuming every
// state carries identical optional fields (defined has no statistics;
// active may omit bit/statistics/signalling entirely).
func TestDecodeDeploymentInfo_StatusTransitions(t *testing.T) {
	cases := []struct {
		name    string
		fixture json.RawMessage
		status  string
	}{
		{"defined", p2qpkDefinedFixture(), "defined"},
		{"started", p2qpkStartedFixture(), "started"},
		{"locked_in", p2qpkLockedInFixture(), "locked_in"},
		{"active", p2qpkActiveFixture(), "active"},
		{"failed", p2qpkFailedFixture(), "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := deploymentInfoResponse(fakeHash("tip"), 100_000, map[string]json.RawMessage{
				"p2qpk": tc.fixture,
			})
			decoded, err := DecodeDeploymentInfo(info)
			if err != nil {
				t.Fatalf("DecodeDeploymentInfo: %v", err)
			}
			if len(decoded.Deployments) != 1 {
				t.Fatalf("got %d deployments, want 1", len(decoded.Deployments))
			}
			d := decoded.Deployments[0]
			if d.Name != "p2qpk" {
				t.Errorf("Name = %q, want p2qpk", d.Name)
			}
			if d.Status != tc.status {
				t.Errorf("Status = %q, want %q", d.Status, tc.status)
			}
		})
	}
}

// TestDecodeDeploymentInfo_P2QPKRealisticFixture is spec item 31: a
// truthful QOGE-style "p2qpk" fixture (synthetic constants — see
// deployment_fixtures_test.go's doc comment) decodes with every field
// preserved.
func TestDecodeDeploymentInfo_P2QPKRealisticFixture(t *testing.T) {
	info := deploymentInfoResponse(fakeHash("tip"), 100_000, map[string]json.RawMessage{
		"p2qpk": p2qpkStartedFixture(),
	})
	decoded, err := DecodeDeploymentInfo(info)
	if err != nil {
		t.Fatalf("DecodeDeploymentInfo: %v", err)
	}
	if len(decoded.Deployments) != 1 || decoded.Deployments[0].Name != "p2qpk" {
		t.Fatalf("expected exactly one p2qpk deployment, got %+v", decoded.Deployments)
	}
	if decoded.Deployments[0].Status != "started" {
		t.Errorf("Status = %q, want started", decoded.Deployments[0].Status)
	}
	if decoded.Deployments[0].SinceHeight != 100_000 {
		t.Errorf("SinceHeight = %d, want 100000", decoded.Deployments[0].SinceHeight)
	}
}

// TestDecodeDeploymentInfo_BuriedNotPersisted is spec item 33: a response
// containing both a buried deployment and a BIP9 deployment must decode
// without crashing, must NOT persist the buried entry, and must persist
// the BIP9 entry. Buried deployments are static historical consensus
// rules with no BIP9 status model (docs/ARCHITECTURE.md §24) — there is
// no chain_deployments row shape for them to occupy.
func TestDecodeDeploymentInfo_BuriedNotPersisted(t *testing.T) {
	h := int64(500)
	info := deploymentInfoResponse(fakeHash("tip"), 100_000, map[string]json.RawMessage{
		"segwit": buriedFixture(true, &h),
		"p2qpk":  p2qpkStartedFixture(),
	})
	decoded, err := DecodeDeploymentInfo(info)
	if err != nil {
		t.Fatalf("DecodeDeploymentInfo: %v", err)
	}
	if len(decoded.Deployments) != 1 {
		t.Fatalf("got %d deployments, want exactly 1 (buried must be dropped)", len(decoded.Deployments))
	}
	if decoded.Deployments[0].Name != "p2qpk" {
		t.Errorf("surviving deployment = %q, want p2qpk", decoded.Deployments[0].Name)
	}
}

// TestDecodeDeploymentInfo_RawJSONRoundTrip is spec item 29: persisting
// and reading back a realistic BIP9 object (every documented field
// present) must preserve semantic equality with the original.
func TestDecodeDeploymentInfo_RawJSONRoundTrip(t *testing.T) {
	original := p2qpkStartedFixture()
	info := deploymentInfoResponse(fakeHash("tip"), 100_000, map[string]json.RawMessage{
		"p2qpk": original,
	})
	decoded, err := DecodeDeploymentInfo(info)
	if err != nil {
		t.Fatalf("DecodeDeploymentInfo: %v", err)
	}

	var want, got map[string]interface{}
	if err := json.Unmarshal(original, &want); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(decoded.Deployments[0].RawJSON, &got); err != nil {
		t.Fatalf("unmarshal decoded RawJSON: %v", err)
	}

	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("raw_json not semantically equal:\n want %s\n got  %s", wantJSON, gotJSON)
	}

	// Spot-check every documented field survived by name (spec item 29's
	// explicit field list), not just gross byte equality.
	for _, field := range []string{"type", "active", "bip9"} {
		if _, ok := got[field]; !ok {
			t.Errorf("decoded raw_json missing top-level field %q", field)
		}
	}
	bip9, _ := got["bip9"].(map[string]interface{})
	for _, field := range []string{"bit", "start_time", "timeout", "min_activation_height", "status", "since", "status_next", "statistics", "signalling"} {
		if _, ok := bip9[field]; !ok {
			t.Errorf("decoded raw_json bip9 object missing field %q", field)
		}
	}
	stats, _ := bip9["statistics"].(map[string]interface{})
	for _, field := range []string{"period", "threshold", "elapsed", "count", "possible"} {
		if _, ok := stats[field]; !ok {
			t.Errorf("decoded raw_json bip9.statistics missing field %q", field)
		}
	}
}

func TestDecodeDeploymentInfo_RejectsMalformedResponse(t *testing.T) {
	validBIP9 := func() *bip9StatisticsFixture {
		return &bip9StatisticsFixture{Period: 2016, Threshold: i64Ptr(1815), Elapsed: 500, Count: 480}
	}

	cases := []struct {
		name string
		info rpc.RawDeploymentInfo
	}{
		{"bad response hash", deploymentInfoResponse("not-hex", 100, nil)},
		{"negative response height", deploymentInfoResponse(fakeHash("tip"), -1, nil)},
		{"empty deployment name", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"": p2qpkStartedFixture(),
		})},
		{"unrecognized type", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"mystery": json.RawMessage(`{"type":"unknown","active":false}`),
		})},
		{"bip9 missing bip9 object", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": json.RawMessage(`{"type":"bip9","active":false}`),
		})},
		{"invalid status", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("weird_status", "started", 0, nil, nil, nil),
		})},
		{"invalid status_next", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "weird_status", 0, nil, nil, nil),
		})},
		{"negative since", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", -1, nil, nil, nil),
		})},
		{"bit out of range low", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", 0, intPtr(-1), nil, nil),
		})},
		{"bit out of range high", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", 0, intPtr(29), nil, nil),
		})},
		{"statistics period zero", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", 0, nil, &bip9StatisticsFixture{Period: 0, Elapsed: 0, Count: 0}, nil),
		})},
		{"statistics elapsed exceeds period", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", 0, nil, &bip9StatisticsFixture{Period: 100, Elapsed: 200, Count: 0}, nil),
		})},
		{"statistics count exceeds elapsed", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", 0, nil, &bip9StatisticsFixture{Period: 100, Elapsed: 50, Count: 60}, nil),
		})},
		{"statistics threshold exceeds period", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", 0, nil, &bip9StatisticsFixture{Period: 100, Threshold: i64Ptr(200), Elapsed: 50, Count: 10}, nil),
		})},
		{"statistics threshold zero", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", 0, nil, &bip9StatisticsFixture{Period: 100, Threshold: i64Ptr(0), Elapsed: 50, Count: 10}, nil),
		})},
		{"signalling invalid character", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"p2qpk": bip9Fixture("started", "started", 0, nil, validBIP9(), strPtr("##xx--")),
		})},
		{"buried negative height", deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
			"segwit": buriedFixture(true, i64Ptr(-5)),
		})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeDeploymentInfo(tc.info); err == nil {
				t.Fatalf("DecodeDeploymentInfo: expected error, got nil")
			}
		})
	}
}

func TestDecodeDeploymentInfo_SignallingAllowsOnlyHashAndDash(t *testing.T) {
	signalling := "##--##--##--##--##--"
	info := deploymentInfoResponse(fakeHash("tip"), 100, map[string]json.RawMessage{
		"p2qpk": bip9Fixture("started", "started", 0, intPtr(1), &bip9StatisticsFixture{Period: 2016, Elapsed: 20, Count: 15}, &signalling),
	})
	if _, err := DecodeDeploymentInfo(info); err != nil {
		t.Fatalf("DecodeDeploymentInfo: %v", err)
	}
}

func TestCandidate_ValidateRejectsDuplicateNames(t *testing.T) {
	c := Candidate{
		CoreTipHeight: 0,
		CoreTipHash:   fakeHash("tip"),
		ObservedAt:    fixedTime(),
		Deployments: []CandidateDeployment{
			{Name: "p2qpk", Status: "started", SinceHeight: 0, RawJSON: json.RawMessage(`{}`)},
			{Name: "p2qpk", Status: "started", SinceHeight: 0, RawJSON: json.RawMessage(`{}`)},
		},
	}
	if err := c.validate(); err == nil {
		t.Fatal("validate: expected error for duplicate deployment name, got nil")
	}
}

func TestCandidate_ValidateRejectsBadAnchor(t *testing.T) {
	base := Candidate{
		CoreTipHeight: 0,
		CoreTipHash:   fakeHash("tip"),
		ObservedAt:    fixedTime(),
	}

	badHash := base
	badHash.CoreTipHash = "not-hex"
	if err := badHash.validate(); err == nil {
		t.Error("validate: expected error for malformed core tip hash")
	}

	badHeight := base
	badHeight.CoreTipHeight = -1
	if err := badHeight.validate(); err == nil {
		t.Error("validate: expected error for negative core tip height")
	}

	badTime := base
	badTime.ObservedAt = time.Time{}
	if err := badTime.validate(); err == nil {
		t.Error("validate: expected error for zero ObservedAt")
	}

	if err := base.validate(); err != nil {
		t.Errorf("validate: expected valid candidate with no deployments to succeed, got %v", err)
	}
}
