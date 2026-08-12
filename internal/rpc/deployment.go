package rpc

import (
	"context"
	"encoding/json"
	"fmt"
)

// RawDeploymentInfo mirrors Core's `getdeploymentinfo [blockhash]`
// top-level response. Deployments is kept as raw per-entry bytes
// (json.RawMessage), never a typed map value, so the caller
// (internal/deployments) can persist the EXACT semantic JSON Core
// returned for each deployment without any lossy round-trip through a
// normalized Go struct — see internal/deployments/model.go.
type RawDeploymentInfo struct {
	Hash        string                     `json:"hash"`
	Height      int64                      `json:"height"`
	Deployments map[string]json.RawMessage `json:"deployments"`
}

// RawDeployment mirrors one value of RawDeploymentInfo.Deployments, typed
// just enough for internal/deployments to strictly validate and classify
// it (type == "buried" vs "bip9"). It is decoded FROM the same
// json.RawMessage bytes that are separately preserved verbatim — this
// struct is used only for validation/classification, never as the
// persisted representation.
type RawDeployment struct {
	Type   string             `json:"type"`
	Active bool               `json:"active"`
	Height *int64             `json:"height,omitempty"`
	BIP9   *RawBIP9Deployment `json:"bip9,omitempty"`
}

// RawBIP9Deployment mirrors Core's per-deployment "bip9" object. Fields
// Core only sometimes emits (bit, statistics, signalling) are pointers;
// start_time/timeout/min_activation_height/since/status/status_next are
// present on every bip9 deployment Core reports and are decoded directly.
type RawBIP9Deployment struct {
	Bit                 *int               `json:"bit,omitempty"`
	StartTime           int64              `json:"start_time"`
	Timeout             int64              `json:"timeout"`
	MinActivationHeight int64              `json:"min_activation_height"`
	Status              string             `json:"status"`
	StatusNext          string             `json:"status_next"`
	Since               int64              `json:"since"`
	Statistics          *RawBIP9Statistics `json:"statistics,omitempty"`
	Signalling          *string            `json:"signalling,omitempty"`
}

// RawBIP9Statistics mirrors Core's per-deployment "bip9.statistics"
// object, present only while Core is actively tallying signalling for the
// current period (typically STARTED, sometimes LOCKED_IN).
type RawBIP9Statistics struct {
	Period    int64  `json:"period"`
	Threshold *int64 `json:"threshold,omitempty"`
	Elapsed   int64  `json:"elapsed"`
	Count     int64  `json:"count"`
	Possible  *bool  `json:"possible,omitempty"`
}

// GetDeploymentInfo calls `getdeploymentinfo <blockhash>` against the
// EXPLICIT block hash supplied by the caller. Deliberately never called
// with no argument (which would query Core's implicit, possibly-moving,
// active tip) — internal/deployments always supplies the confirmed
// PostgreSQL checkpoint's own block hash, per its anchored acquisition
// algorithm (docs/ARCHITECTURE.md §24).
func (c *Client) GetDeploymentInfo(ctx context.Context, blockHash string) (RawDeploymentInfo, error) {
	var info RawDeploymentInfo
	if err := c.CallInto(ctx, &info, "getdeploymentinfo", blockHash); err != nil {
		return RawDeploymentInfo{}, fmt.Errorf("getdeploymentinfo %s: %w", blockHash, err)
	}
	return info, nil
}
