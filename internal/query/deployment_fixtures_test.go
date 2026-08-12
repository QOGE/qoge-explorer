package query

import (
	"encoding/json"
	"time"

	"github.com/QOGE/qoge-explorer/internal/deployments"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestDeploymentsStore wraps pool (the SAME pool newTestQueryStore
// already migrated, which applies every migration under migrations/
// including 0003_deployment_state) in a real internal/deployments.Store,
// so this package's own tests can build chain_deployments/deployment_state
// fixtures through the real, already-reviewed writer (Store.ReplaceSnapshot)
// — never ad-hoc SQL for VALID fixtures — exactly mirroring
// newTestMempoolStore. Only the integrity/drift tests use raw SQL
// afterward, deliberately, to simulate corruption a real writer could never
// produce (see deployments_test.go).
func newTestDeploymentsStore(pool *pgxpool.Pool) *deployments.Store {
	return deployments.NewStore(pool)
}

// The JSON fixture types below construct REALISTIC (but synthetic — never
// real Qogecoin mainnet consensus constants) getdeploymentinfo per
// -deployment JSON objects, mirroring the exact field shape
// internal/rpc.RawDeployment/RawBIP9Deployment/RawBIP9Statistics decode
// from and internal/deployments/deployment_fixtures_test.go already builds
// — duplicated here per this repo's established cross-package
// test-fixture convention (test-only files aren't importable across
// packages; see mempool_fixtures_test.go's doc comment).
type deploymentJSONFixture struct {
	Type   string           `json:"type"`
	Active bool             `json:"active"`
	Height *int64           `json:"height,omitempty"`
	BIP9   *bip9JSONFixture `json:"bip9,omitempty"`
}

type bip9JSONFixture struct {
	Bit                 *int                  `json:"bit,omitempty"`
	StartTime           int64                 `json:"start_time"`
	Timeout             int64                 `json:"timeout"`
	MinActivationHeight int64                 `json:"min_activation_height"`
	Status              string                `json:"status"`
	StatusNext          string                `json:"status_next"`
	Since               int64                 `json:"since"`
	Statistics          *bip9StatsJSONFixture `json:"statistics,omitempty"`
	Signalling          *string               `json:"signalling,omitempty"`
}

type bip9StatsJSONFixture struct {
	Period    int64  `json:"period"`
	Threshold *int64 `json:"threshold,omitempty"`
	Elapsed   int64  `json:"elapsed"`
	Count     int64  `json:"count"`
	Possible  *bool  `json:"possible,omitempty"`
}

// bip9Fixture builds a realistic "type":"bip9" deployment object.
// activationHeight mirrors Core's top-level "height", only ever populated
// once status_next is ACTIVE, per Core's actual field optionality.
// Core's top-level "active" reflects next_state == ACTIVE (SoftForkDescPushBack
// in src/rpc/blockchain.cpp), NOT current_state — so it is derived from
// statusNext here, never from status. A deployment can be current_state
// LOCKED_IN with next_state ACTIVE (active=true) in the very block that
// activates it.
func bip9Fixture(status, statusNext string, since int64, bit *int, activationHeight *int64, statistics *bip9StatsJSONFixture, signalling *string) json.RawMessage {
	raw, err := json.Marshal(deploymentJSONFixture{
		Type:   "bip9",
		Active: statusNext == "active",
		Height: activationHeight,
		BIP9: &bip9JSONFixture{
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

// intPtr/i64Ptr/boolPtr/strPtr are defined in rpcfixtures_test.go and
// mempool_fixtures_test.go respectively — reused here rather than
// redeclared.

// p2qpkDefinedFixture/p2qpkStartedFixture/p2qpkLockedInFixture/
// p2qpkActiveFixture/p2qpkFailedFixture build a realistic "p2qpk"
// deployment object at each BIP9 status, matching QOGE Core stable's
// src/rpc/blockchain.cpp SoftForkDescPushBack exactly: has_signal is true
// only for current_state STARTED or LOCKED_IN, and that gates bit,
// statistics, AND signalling together (not statistics alone) — so
// DEFINED/ACTIVE/FAILED carry none of the three, and LOCKED_IN carries
// all three (with statistics.threshold/possible additionally omitted
// only for LOCKED_IN). These are TEST FIXTURES with synthetic constants,
// never asserted real Qogecoin mainnet values.
func p2qpkDefinedFixture() json.RawMessage {
	return bip9Fixture("defined", "defined", 0, nil, nil, nil, nil)
}

func p2qpkStartedFixture() json.RawMessage {
	signalling := "##--##------------"
	return bip9Fixture("started", "started", 100_000, intPtr(21), nil, &bip9StatsJSONFixture{
		Period: 2016, Threshold: i64Ptr(1815), Elapsed: 500, Count: 480, Possible: boolPtr(true),
	}, &signalling)
}

func p2qpkLockedInFixture() json.RawMessage {
	// has_signal is true for LOCKED_IN, so bit and signalling are still
	// present; only statistics.threshold/possible are additionally omitted
	// for this state (current_state != LOCKED_IN gate in blockchain.cpp).
	signalling := "####################"
	return bip9Fixture("locked_in", "locked_in", 102_016, intPtr(21), nil, &bip9StatsJSONFixture{
		Period: 2016, Elapsed: 2016, Count: 2000,
	}, &signalling)
}

func p2qpkActiveFixture() json.RawMessage {
	// ACTIVE: has_signal is false, so bit/statistics/signalling are all
	// legitimately absent; Core now reports the block height the
	// deployment activated at.
	return bip9Fixture("active", "active", 104_032, nil, i64Ptr(104_032), nil, nil)
}

func p2qpkFailedFixture() json.RawMessage {
	// FAILED: has_signal is false, so bit/statistics/signalling are all
	// legitimately absent.
	return bip9Fixture("failed", "failed", 102_016, nil, nil, nil, nil)
}

// p2qpkLockedInActivatingFixture is the LOCKED_IN -> ACTIVE transition
// boundary: current_state is still LOCKED_IN (so bit/statistics/
// signalling are all present, with threshold/possible omitted per
// LOCKED_IN semantics), but next_state is ACTIVE, so the top-level
// "active" is true and "height" (the activation height) is populated.
// This proves query/api/web never assume active == (status == "active"):
// here status is still "locked_in" while active is true.
func p2qpkLockedInActivatingFixture() json.RawMessage {
	signalling := "####################"
	return bip9Fixture("locked_in", "active", 102_016, intPtr(21), i64Ptr(104_032), &bip9StatsJSONFixture{
		Period: 2016, Elapsed: 2016, Count: 2016,
	}, &signalling)
}

// deploymentCandidateOf extracts the (status, since) internal/deployments
// requires alongside the raw bytes for a CandidateDeployment — since/
// status must be supplied independently of raw (mirroring exactly what
// internal/deployments.DecodeDeploymentInfo itself extracts at
// acquisition time), never parsed back out of raw here.
func deploymentCandidateOf(name, status string, since int64, raw json.RawMessage) deployments.CandidateDeployment {
	return deployments.CandidateDeployment{Name: name, Status: status, SinceHeight: since, RawJSON: raw}
}

// deploymentSnapshot assembles a deployments.Candidate anchored to
// (coreTipHeight, coreTipHash), observed at observedAt.
func deploymentSnapshot(coreTipHeight int64, coreTipHash string, observedAt time.Time, deps ...deployments.CandidateDeployment) deployments.Candidate {
	return deployments.Candidate{
		CoreTipHeight: coreTipHeight,
		CoreTipHash:   coreTipHash,
		ObservedAt:    observedAt,
		Deployments:   deps,
	}
}
