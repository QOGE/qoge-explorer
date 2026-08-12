package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrDeploymentCacheUninitialized means the deployment observer
// (internal/deployments) has never successfully published a snapshot.
// internal/api maps this to HTTP 503 with the stable code
// "deployment_cache_uninitialized" for a name lookup — an uninitialized
// cache cannot truthfully claim a deployment does not exist, so this is
// deliberately distinct from ErrNotFound (docs/ARCHITECTURE.md §25).
var ErrDeploymentCacheUninitialized = errors.New("query: deployment cache is uninitialized")

// ErrDeploymentCacheIntegrity means a row in chain_deployments disagreed
// with itself or with deployment_state in a way that must never be
// silently resolved by picking one side — see decodeDeploymentRow and
// docs/ARCHITECTURE.md §25 "Cache integrity checks". This can only result
// from database-level corruption or a bug elsewhere; internal/deployments'
// own writer (Store.ReplaceSnapshot) and strict RPC decoder
// (DecodeDeploymentInfo) never produce a state that would trigger it.
var ErrDeploymentCacheIntegrity = errors.New("query: deployment cache integrity check failed")

// DeploymentState is a read-only view of the deployment_state('main')
// singleton row PLUS, read from the SAME PostgreSQL snapshot, the
// confirmed chain's own indexed checkpoint (sync_state) — never two
// independent queries. Mirrors MempoolState's shape and freshness
// derivation exactly (see mempoolStateFrom's doc comment):
//
//   - UNINITIALIZED (Initialized=false): internal/deployments has never
//     successfully published a snapshot. Status is "uninitialized", Stale
//     is always false.
//   - FRESH (Initialized=true, anchor == confirmed tip): the cached
//     deployment snapshot was observed against the exact confirmed chain
//     state a reader is also seeing right now. This means only that the
//     cached observation and the confirmed tip agree — never that this
//     HTTP request queried Core live; the observer polls asynchronously
//     (docs/ARCHITECTURE.md §25 "Fresh does not mean synchronous Core").
//   - STALE (Initialized=true, anchor != confirmed tip): the confirmed
//     indexed tip no longer matches the snapshot's anchor. This does NOT
//     imply the confirmed chain advanced forward — it can equally result
//     from a same-height canonical reorg, a rollback, or a replacement
//     tip at another hash entirely. Never described as "advanced past" or
//     "ahead of" (docs/ARCHITECTURE.md §25 "Stale semantics").
type DeploymentState struct {
	Initialized bool  `json:"initialized"`
	Generation  int64 `json:"generation"`

	CoreTipHeight *int64  `json:"core_tip_height,omitempty"`
	CoreTipHash   *string `json:"core_tip_hash,omitempty"`

	DeploymentCount int        `json:"deployment_count"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`

	ConfirmedIndexedHeight int64   `json:"confirmed_indexed_height"`
	ConfirmedIndexedHash   *string `json:"confirmed_indexed_hash,omitempty"`

	Status string `json:"status"`
	Stale  bool   `json:"stale"`
}

// deploymentStateFrom reads deployment_state('main') and the confirmed
// sync_state checkpoint against q, deriving Status/Stale by comparing
// them — callers needing this alongside chain_deployments rows MUST call
// it against an already-open read-only REPEATABLE READ transaction (see
// readTx), never against s.pool directly, so the comparison and every
// other read in the same response share one consistent snapshot
// (docs/ARCHITECTURE.md §25 "Repeatable-read snapshots").
func deploymentStateFrom(ctx context.Context, q querier) (DeploymentState, error) {
	var st DeploymentState
	var observedAt *time.Time
	err := q.QueryRow(ctx, `
		SELECT initialized, generation, core_tip_height, core_tip_hash,
		       deployment_count, observed_at
		FROM deployment_state WHERE name = 'main'
	`).Scan(&st.Initialized, &st.Generation, &st.CoreTipHeight, &st.CoreTipHash,
		&st.DeploymentCount, &observedAt)
	if err != nil {
		return DeploymentState{}, fmt.Errorf("query: deployment state: %w", err)
	}
	st.ObservedAt = observedAt

	confirmed, err := statusFrom(ctx, q)
	if err != nil {
		return DeploymentState{}, err
	}
	st.ConfirmedIndexedHeight = confirmed.IndexedHeight
	st.ConfirmedIndexedHash = confirmed.IndexedHash

	switch {
	case !st.Initialized:
		st.Status = "uninitialized"
		st.Stale = false
	case st.CoreTipHeight != nil && st.CoreTipHash != nil && confirmed.IndexedHash != nil &&
		*st.CoreTipHeight == confirmed.IndexedHeight && *st.CoreTipHash == *confirmed.IndexedHash:
		st.Status = "fresh"
		st.Stale = false
	default:
		st.Status = "stale"
		st.Stale = true
	}

	return st, nil
}

// DeploymentStatistics mirrors Core's per-deployment "bip9.statistics"
// object exactly as cached — read directly from the persisted raw_json,
// never recomputed (docs/ARCHITECTURE.md §25 "No Core state
// recomputation"). Threshold/Possible are pointers because Core
// legitimately omits both once a deployment is LOCKED_IN.
type DeploymentStatistics struct {
	Period    int64  `json:"period"`
	Threshold *int64 `json:"threshold,omitempty"`
	Elapsed   int64  `json:"elapsed"`
	Count     int64  `json:"count"`
	Possible  *bool  `json:"possible,omitempty"`
}

// Deployment is one cached BIP9 deployment, decoded from
// chain_deployments' persisted raw_json plus its normalized columns
// (cross-checked for consistency — see decodeDeploymentRow). Every field
// is exactly what Core reported at CheckedAt; nothing here is
// independently computed BIP9/VersionBits state (docs/ARCHITECTURE.md
// §25). Raw is the complete, semantically unchanged getdeploymentinfo
// object Core returned for this deployment, embedded as json.RawMessage
// so a JSON API response carries the actual object rather than a
// quoted string.
type Deployment struct {
	Name string `json:"name"`
	Type string `json:"type"`

	Active bool `json:"active"`
	// ActivationHeight mirrors the deployment's top-level "height" field
	// — Core only reports it once status_next is ACTIVE (spec: never
	// forced non-optional here).
	ActivationHeight *int64 `json:"activation_height,omitempty"`

	Status     string `json:"status"`
	StatusNext string `json:"status_next"`
	Since      int64  `json:"since"`

	Bit                 *int  `json:"bit,omitempty"`
	StartTime           int64 `json:"start_time"`
	Timeout             int64 `json:"timeout"`
	MinActivationHeight int64 `json:"min_activation_height"`

	Statistics *DeploymentStatistics `json:"statistics,omitempty"`
	Signalling *string               `json:"signalling,omitempty"`

	CheckedAt time.Time       `json:"checked_at"`
	Raw       json.RawMessage `json:"raw"`
}

// rawDeploymentJSON/rawBIP9JSON/rawBIP9StatisticsJSON are a small,
// query-local decode shape for chain_deployments.raw_json — deliberately
// NOT internal/rpc.RawDeployment/RawBIP9Deployment/RawBIP9Statistics.
// internal/query must never import internal/rpc merely to decode already
// -persisted, already-strictly-validated JSON (docs/ARCHITECTURE.md §25
// "Raw JSON decode"): the presence-aware pointer rigor that package
// enforces at ACQUISITION time (internal/deployments.DecodeDeploymentInfo)
// is not re-litigated here — a query-time decode failure is cache
// corruption, reported as ErrDeploymentCacheIntegrity, never treated as
// "field legitimately absent".
type rawDeploymentJSON struct {
	Type   string       `json:"type"`
	Active bool         `json:"active"`
	Height *int64       `json:"height,omitempty"`
	BIP9   *rawBIP9JSON `json:"bip9,omitempty"`
}

type rawBIP9JSON struct {
	Bit                 *int                   `json:"bit,omitempty"`
	StartTime           int64                  `json:"start_time"`
	Timeout             int64                  `json:"timeout"`
	MinActivationHeight int64                  `json:"min_activation_height"`
	Status              string                 `json:"status"`
	StatusNext          string                 `json:"status_next"`
	Since               int64                  `json:"since"`
	Statistics          *rawBIP9StatisticsJSON `json:"statistics,omitempty"`
	Signalling          *string                `json:"signalling,omitempty"`
}

type rawBIP9StatisticsJSON struct {
	Period    int64  `json:"period"`
	Threshold *int64 `json:"threshold,omitempty"`
	Elapsed   int64  `json:"elapsed"`
	Count     int64  `json:"count"`
	Possible  *bool  `json:"possible,omitempty"`
}

// decodeDeploymentRow builds a Deployment from one chain_deployments row,
// requiring it be internally consistent (docs/ARCHITECTURE.md §25 "Cache
// integrity checks", spec items 14/49/50/51):
//
//   - raw_json must decode, and its "type" must be "bip9" — Phase 2G.1's
//     writer never persists a buried deployment.
//   - raw_json.bip9.status must equal the row's normalized status column.
//   - raw_json.bip9.since must equal the row's normalized since_height
//     column (which must itself be non-NULL).
//   - the row's checked_at must equal deployment_state.observed_at —
//     every row of one generation shares the same observation timestamp
//     (internal/deployments.Candidate.ObservedAt).
//
// Any disagreement is reported as ErrDeploymentCacheIntegrity — this
// function never silently prefers one side.
func decodeDeploymentRow(name, status string, sinceHeight *int64, rawJSON []byte, checkedAt time.Time, observedAt *time.Time) (Deployment, error) {
	var raw rawDeploymentJSON
	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return Deployment{}, fmt.Errorf("query: deployment %q: %w: raw_json does not decode: %v", name, ErrDeploymentCacheIntegrity, err)
	}
	if raw.Type != "bip9" {
		return Deployment{}, fmt.Errorf("query: deployment %q: %w: raw_json type %q, want \"bip9\"", name, ErrDeploymentCacheIntegrity, raw.Type)
	}
	if raw.BIP9 == nil {
		return Deployment{}, fmt.Errorf("query: deployment %q: %w: raw_json has type \"bip9\" but no bip9 object", name, ErrDeploymentCacheIntegrity)
	}
	b := raw.BIP9
	if b.Status != status {
		return Deployment{}, fmt.Errorf("query: deployment %q: %w: chain_deployments.status %q != raw_json.bip9.status %q", name, ErrDeploymentCacheIntegrity, status, b.Status)
	}
	if sinceHeight == nil {
		return Deployment{}, fmt.Errorf("query: deployment %q: %w: chain_deployments.since_height is NULL", name, ErrDeploymentCacheIntegrity)
	}
	if *sinceHeight != b.Since {
		return Deployment{}, fmt.Errorf("query: deployment %q: %w: chain_deployments.since_height %d != raw_json.bip9.since %d", name, ErrDeploymentCacheIntegrity, *sinceHeight, b.Since)
	}
	if observedAt == nil || !checkedAt.Equal(*observedAt) {
		return Deployment{}, fmt.Errorf("query: deployment %q: %w: chain_deployments.checked_at != deployment_state.observed_at", name, ErrDeploymentCacheIntegrity)
	}

	d := Deployment{
		Name:                name,
		Type:                raw.Type,
		Active:              raw.Active,
		ActivationHeight:    raw.Height,
		Status:              b.Status,
		StatusNext:          b.StatusNext,
		Since:               b.Since,
		Bit:                 b.Bit,
		StartTime:           b.StartTime,
		Timeout:             b.Timeout,
		MinActivationHeight: b.MinActivationHeight,
		Signalling:          b.Signalling,
		CheckedAt:           checkedAt,
		Raw:                 json.RawMessage(rawJSON),
	}
	if b.Statistics != nil {
		d.Statistics = &DeploymentStatistics{
			Period:    b.Statistics.Period,
			Threshold: b.Statistics.Threshold,
			Elapsed:   b.Statistics.Elapsed,
			Count:     b.Statistics.Count,
			Possible:  b.Statistics.Possible,
		}
	}
	return d, nil
}

// DeploymentOverview is the /deployments page's and GET /api/v1/deployments'
// state + full deployment list pair, read from ONE read-only REPEATABLE
// READ snapshot — the same composite-read-snapshot property
// MempoolOverview/ExplorerOverview already give their own pages (see
// mempool.go/composite.go). Deployments is never nil — an uninitialized
// cache and a legitimately empty (initialized, zero-deployment) snapshot
// both report an empty (non-nil) slice, distinguished only by
// State.Initialized (docs/ARCHITECTURE.md §25 "Uninitialized vs
// initialized-empty").
type DeploymentOverview struct {
	State       DeploymentState `json:"state"`
	Deployments []Deployment    `json:"deployments"`
}

// DeploymentOverview reads DeploymentState and every cached deployment
// from one snapshot, ordered by name ascending (spec item 9 — the
// deployment set is small enough that no pagination is needed). When the
// cache is uninitialized, Deployments is an empty slice — never a
// synthesized "zero deployments" as if a successful observation had
// returned none (spec item 32). When initialized, the number of rows read
// must equal deployment_state.deployment_count, or this fails with
// ErrDeploymentCacheIntegrity rather than exposing a partial list (spec
// item 16/52).
func (s *Store) DeploymentOverview(ctx context.Context) (DeploymentOverview, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return DeploymentOverview{}, err
	}
	defer done()

	state, err := deploymentStateFrom(ctx, tx)
	if err != nil {
		return DeploymentOverview{}, err
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first statement

	deployments := []Deployment{}
	if !state.Initialized {
		return DeploymentOverview{State: state, Deployments: deployments}, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT name, status, since_height, raw_json, checked_at
		FROM chain_deployments ORDER BY name ASC
	`)
	if err != nil {
		return DeploymentOverview{}, fmt.Errorf("query: deployments: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, status string
		var sinceHeight *int64
		var rawJSON []byte
		var checkedAt time.Time
		if err := rows.Scan(&name, &status, &sinceHeight, &rawJSON, &checkedAt); err != nil {
			return DeploymentOverview{}, fmt.Errorf("query: deployments: scan: %w", err)
		}
		d, err := decodeDeploymentRow(name, status, sinceHeight, rawJSON, checkedAt, state.ObservedAt)
		if err != nil {
			return DeploymentOverview{}, err
		}
		deployments = append(deployments, d)
	}
	if err := rows.Err(); err != nil {
		return DeploymentOverview{}, fmt.Errorf("query: deployments: %w", err)
	}

	if len(deployments) != state.DeploymentCount {
		return DeploymentOverview{}, fmt.Errorf("query: deployments: %w: deployment_state.deployment_count %d != %d rows read",
			ErrDeploymentCacheIntegrity, state.DeploymentCount, len(deployments))
	}

	return DeploymentOverview{State: state, Deployments: deployments}, nil
}

// DeploymentDetail is the /deployments/{name} page's and
// GET /api/v1/deployments/{name}'s state + single-deployment pair, read
// from ONE read-only REPEATABLE READ snapshot — same rationale as
// DeploymentOverview.
type DeploymentDetail struct {
	State      DeploymentState `json:"state"`
	Deployment Deployment      `json:"deployment"`
}

// DeploymentByName looks up one cached deployment by its exact name
// (Core's own deployment map key, e.g. "p2qpk") from one snapshot.
//
//   - If the cache has never been initialized, this returns
//     ErrDeploymentCacheUninitialized — an uninitialized cache cannot
//     truthfully claim a deployment does not exist (spec item 21).
//   - If the cache is initialized but name is not present in the current
//     snapshot, this returns ErrNotFound — meaning "not present in the
//     cached snapshot", never "Core guarantees this deployment does not
//     exist" (spec item 22). There is no Core RPC fallback.
func (s *Store) DeploymentByName(ctx context.Context, name string) (DeploymentDetail, error) {
	tx, done, err := s.readTx(ctx)
	if err != nil {
		return DeploymentDetail{}, err
	}
	defer done()

	state, err := deploymentStateFrom(ctx, tx)
	if err != nil {
		return DeploymentDetail{}, err
	}
	fireSnapshotTestHook() // snapshot is now fixed as of this first statement

	if !state.Initialized {
		return DeploymentDetail{}, ErrDeploymentCacheUninitialized
	}

	var status string
	var sinceHeight *int64
	var rawJSON []byte
	var checkedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT status, since_height, raw_json, checked_at
		FROM chain_deployments WHERE name = $1
	`, name).Scan(&status, &sinceHeight, &rawJSON, &checkedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeploymentDetail{}, ErrNotFound
	}
	if err != nil {
		return DeploymentDetail{}, fmt.Errorf("query: deployment %q: %w", name, err)
	}

	d, err := decodeDeploymentRow(name, status, sinceHeight, rawJSON, checkedAt, state.ObservedAt)
	if err != nil {
		return DeploymentDetail{}, err
	}

	return DeploymentDetail{State: state, Deployment: d}, nil
}
