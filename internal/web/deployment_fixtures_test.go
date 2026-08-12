package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/QOGE/qoge-explorer/internal/deployments"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestDeploymentsStore/newDeploymentTestServer mirror
// newTestMempoolStore/newMempoolTestServer exactly.
func newTestDeploymentsStore(pool *pgxpool.Pool) *deployments.Store {
	return deployments.NewStore(pool)
}

func newDeploymentTestServer(t *testing.T) (context.Context, *Server, *pgxpool.Pool, *deployments.Store) {
	t.Helper()
	s, _, pool := newTestServerWithPool(t)
	return context.Background(), s, pool, newTestDeploymentsStore(pool)
}

// testObservedAt returns a stable, microsecond-truncated observation
// timestamp matching PostgreSQL's timestamptz precision.
func testObservedAt() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// The JSON fixture builders below duplicate
// internal/query/deployment_fixtures_test.go's shape — test-only files
// aren't importable across packages (established convention).
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

// bip9Fixture's top-level "active" reflects next_state == ACTIVE
// (SoftForkDescPushBack in qogecoin's src/rpc/blockchain.cpp), NOT
// current_state — so it is derived from statusNext, never status. A
// deployment can be current_state LOCKED_IN with next_state ACTIVE
// (active=true) in the very block that activates it.
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

func p2qpkDefinedFixture() json.RawMessage {
	// DEFINED: has_signal is false, so bit/statistics/signalling are all
	// legitimately absent.
	return bip9Fixture("defined", "defined", 0, nil, nil, nil, nil)
}

func p2qpkStartedFixture() json.RawMessage {
	signalling := "##--##------------"
	return bip9Fixture("started", "started", 100_000, intPtr(21), nil, &bip9StatsJSONFixture{
		Period: 2016, Threshold: i64Ptr(1815), Elapsed: 500, Count: 480, Possible: boolPtr(true),
	}, &signalling)
}

// p2qpkStartedPossibleFalseFixture is spec item 13: possible=false must be
// rendered as "no", never omitted the way a nil Possible is for LOCKED_IN.
func p2qpkStartedPossibleFalseFixture() json.RawMessage {
	signalling := "----------------"
	return bip9Fixture("started", "started", 100_000, intPtr(21), nil, &bip9StatsJSONFixture{
		Period: 2016, Threshold: i64Ptr(1815), Elapsed: 500, Count: 10, Possible: boolPtr(false),
	}, &signalling)
}

func p2qpkLockedInFixture() json.RawMessage {
	// has_signal is true for LOCKED_IN, so bit and signalling are still
	// present; only statistics.threshold/possible are additionally omitted
	// for this state.
	signalling := "####################"
	return bip9Fixture("locked_in", "locked_in", 102_016, intPtr(21), nil, &bip9StatsJSONFixture{
		Period: 2016, Elapsed: 2016, Count: 2000,
	}, &signalling)
}

func p2qpkActiveFixture() json.RawMessage {
	// ACTIVE: has_signal is false, so bit/statistics/signalling are all
	// legitimately absent.
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
// This proves the templates never assume active == (status == "active"):
// here status is still "locked_in" while active is true.
func p2qpkLockedInActivatingFixture() json.RawMessage {
	signalling := "####################"
	return bip9Fixture("locked_in", "active", 102_016, intPtr(21), i64Ptr(104_032), &bip9StatsJSONFixture{
		Period: 2016, Elapsed: 2016, Count: 2016,
	}, &signalling)
}

func deploymentCandidateOf(name, status string, since int64, raw json.RawMessage) deployments.CandidateDeployment {
	return deployments.CandidateDeployment{Name: name, Status: status, SinceHeight: since, RawJSON: raw}
}

func deploymentSnapshotCandidate(coreTipHeight int64, coreTipHash string, deps ...deployments.CandidateDeployment) deployments.Candidate {
	return deployments.Candidate{
		CoreTipHeight: coreTipHeight,
		CoreTipHash:   coreTipHash,
		ObservedAt:    testObservedAt(),
		Deployments:   deps,
	}
}
