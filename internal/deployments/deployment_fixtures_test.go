package deployments

import (
	"encoding/json"

	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// The types/builders in this file construct REALISTIC (but synthetic —
// never real Qogecoin mainnet consensus constants asserted from chat
// text, per spec item 31) getdeploymentinfo JSON payloads for tests, by
// marshaling through the same field shape internal/rpc.RawDeployment/
// RawBIP9Deployment/RawBIP9Statistics decode from, so the exact bytes
// exercised here are the same shape a real Core node would send.

type bip9StatisticsFixture struct {
	Period    int64  `json:"period"`
	Threshold *int64 `json:"threshold,omitempty"`
	Elapsed   int64  `json:"elapsed"`
	Count     int64  `json:"count"`
	Possible  *bool  `json:"possible,omitempty"`
}

type bip9ObjectFixture struct {
	Bit                 *int                   `json:"bit,omitempty"`
	StartTime           int64                  `json:"start_time"`
	Timeout             int64                  `json:"timeout"`
	MinActivationHeight int64                  `json:"min_activation_height"`
	Status              string                 `json:"status"`
	StatusNext          string                 `json:"status_next"`
	Since               int64                  `json:"since"`
	Statistics          *bip9StatisticsFixture `json:"statistics,omitempty"`
	Signalling          *string                `json:"signalling,omitempty"`
}

type deploymentFixture struct {
	Type   string             `json:"type"`
	Active bool               `json:"active"`
	Height *int64             `json:"height,omitempty"`
	BIP9   *bip9ObjectFixture `json:"bip9,omitempty"`
}

// buriedFixture builds a realistic "type":"buried" deployment object —
// no "bip9" object, an optional activation height, per Core's actual
// buried-deployment shape (e.g. bip34/bip65/bip66/csv/segwit).
func buriedFixture(active bool, height *int64) json.RawMessage {
	raw, err := json.Marshal(deploymentFixture{Type: "buried", Active: active, Height: height})
	if err != nil {
		panic(err)
	}
	return raw
}

// bip9Fixture builds a realistic "type":"bip9" deployment object with the
// given status/status_next and optional statistics/signalling — the
// building block every status-transition/raw-JSON-round-trip/P2QPK
// fixture test in this package composes from (spec items 29-31).
func bip9Fixture(status, statusNext string, since int64, bit *int, statistics *bip9StatisticsFixture, signalling *string) json.RawMessage {
	raw, err := json.Marshal(deploymentFixture{
		Type:   "bip9",
		Active: status == "active",
		BIP9: &bip9ObjectFixture{
			Bit:                 bit,
			StartTime:           1_700_000_000,
			Timeout:             1_800_000_000,
			MinActivationHeight: 0,
			Status:              status,
			StatusNext:          statusNext,
			Since:               since,
			Statistics:          statistics,
			Signalling:          signalling,
		},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func intPtr(i int) *int       { return &i }
func i64Ptr(i int64) *int64   { return &i }
func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// p2qpkDefinedFixture, p2qpkStartedFixture, etc. build a realistic
// "p2qpk" deployment object at each BIP9 status, respecting Core's actual
// per-status field optionality (spec item 10/31): DEFINED carries no
// signalling statistics; STARTED/LOCKED_IN normally carry statistics;
// ACTIVE may omit bit/statistics/signalling entirely. These are TEST
// FIXTURES with synthetic constants, not asserted real Qogecoin mainnet
// values — see this file's top doc comment.
func p2qpkDefinedFixture() json.RawMessage {
	return bip9Fixture("defined", "defined", 0, intPtr(21), nil, nil)
}

func p2qpkStartedFixture() json.RawMessage {
	signalling := "##--##------------"
	return bip9Fixture("started", "started", 100_000, intPtr(21), &bip9StatisticsFixture{
		Period: 2016, Threshold: i64Ptr(1815), Elapsed: 500, Count: 480, Possible: boolPtr(true),
	}, &signalling)
}

func p2qpkLockedInFixture() json.RawMessage {
	// Core output semantics: threshold/possible/signalling may be absent
	// once LOCKED_IN — represented here by a statistics object with only
	// period/elapsed/count (no threshold, no possible), per spec item 10.
	return bip9Fixture("locked_in", "locked_in", 102_016, intPtr(21), &bip9StatisticsFixture{
		Period: 2016, Elapsed: 2016, Count: 2000,
	}, nil)
}

func p2qpkActiveFixture() json.RawMessage {
	// ACTIVE: bit/statistics/signalling all legitimately absent per spec
	// item 10 — Core no longer needs to report ongoing signalling data
	// once a deployment has activated.
	return bip9Fixture("active", "active", 104_032, nil, nil, nil)
}

func p2qpkFailedFixture() json.RawMessage {
	return bip9Fixture("failed", "failed", 102_016, intPtr(21), &bip9StatisticsFixture{
		Period: 2016, Threshold: i64Ptr(1815), Elapsed: 2016, Count: 1200, Possible: boolPtr(false),
	}, nil)
}

// deploymentInfoResponse builds a realistic top-level getdeploymentinfo
// rpc.RawDeploymentInfo response.
func deploymentInfoResponse(hash string, height int64, deployments map[string]json.RawMessage) rpc.RawDeploymentInfo {
	return rpc.RawDeploymentInfo{Hash: hash, Height: i64Ptr(height), Deployments: deployments}
}
