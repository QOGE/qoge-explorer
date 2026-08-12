package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/QOGE/qoge-explorer/internal/deployments"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testObservedAt returns a stable, microsecond-truncated observation
// timestamp — PostgreSQL's timestamptz column has microsecond precision,
// so truncating here keeps a later checked_at/observed_at comparison
// exact rather than losing sub-microsecond precision on write.
func testObservedAt() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// newTestDeploymentsStore/newDeploymentTestServer mirror
// newTestMempoolStore/newMempoolTestServer exactly, for the same reason:
// this package needs to seed chain_deployments/deployment_state fixtures
// through the real, already-reviewed writer (deployments.Store.
// ReplaceSnapshot), never ad-hoc SQL.
func newTestDeploymentsStore(pool *pgxpool.Pool) *deployments.Store {
	return deployments.NewStore(pool)
}

func newDeploymentTestServer(t *testing.T) (context.Context, *Server, *pgxpool.Pool, *deployments.Store) {
	t.Helper()
	s, _, pool := newTestServerWithPool(t)
	return context.Background(), s, pool, newTestDeploymentsStore(pool)
}

// The JSON fixture builders below duplicate
// internal/query/deployment_fixtures_test.go's shape — test-only files
// aren't importable across packages (established convention; see
// internal/api/mempool_fixtures_test.go's own doc comment).
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

func p2qpkStartedFixture() json.RawMessage {
	signalling := "##--##------------"
	return bip9Fixture("started", "started", 100_000, intPtr(21), nil, &bip9StatsJSONFixture{
		Period: 2016, Threshold: i64Ptr(1815), Elapsed: 500, Count: 480, Possible: boolPtr(true),
	}, &signalling)
}

func p2qpkActiveFixture() json.RawMessage {
	// ACTIVE: has_signal is false (current_state != STARTED/LOCKED_IN), so
	// bit/statistics/signalling are all legitimately absent.
	return bip9Fixture("active", "active", 104_032, nil, i64Ptr(104_032), nil, nil)
}

// p2qpkLockedInActivatingFixture is the LOCKED_IN -> ACTIVE transition
// boundary: current_state is still LOCKED_IN (so bit/statistics/
// signalling are all present, with threshold/possible omitted per
// LOCKED_IN semantics), but next_state is ACTIVE, so the top-level
// "active" is true and "height" (the activation height) is populated.
// This proves the API never derives active from status: here status is
// still "locked_in" while active is true.
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
