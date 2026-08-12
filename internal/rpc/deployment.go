package rpc

import (
	"context"
	"encoding/json"
	"fmt"
)

// RawDeploymentInfo mirrors Core's `getdeploymentinfo [blockhash]`
// top-level response. Height is a pointer so internal/deployments can
// distinguish "field absent/null" from a legitimate height of 0 — Core
// always emits it, so a nil Height is a strict-decode failure, never
// silently treated as height 0 (docs/ARCHITECTURE.md §24). Deployments is
// kept as raw per-entry bytes (json.RawMessage), never a typed map value,
// so the caller can persist the EXACT semantic JSON Core returned for
// each deployment without any lossy round-trip through a normalized Go
// struct — see internal/deployments/model.go. A plain (non-pointer) map
// field already lets Go's encoding/json distinguish all three states we
// care about: the key absent from the JSON object and the key present
// with a JSON null both leave Deployments nil; the key present as `{}`
// allocates a non-nil, empty map. internal/deployments relies on exactly
// this nil-vs-non-nil distinction to reject a missing/null deployments
// object while still accepting a legitimate empty one.
type RawDeploymentInfo struct {
	Hash        string                     `json:"hash"`
	Height      *int64                     `json:"height"`
	Deployments map[string]json.RawMessage `json:"deployments"`
}

// RawDeployment mirrors one value of RawDeploymentInfo.Deployments, typed
// just enough for internal/deployments to strictly validate and classify
// it (type == "buried" vs "bip9"). It is decoded FROM the same
// json.RawMessage bytes that are separately preserved verbatim — this
// struct is used only for validation/classification, never as the
// persisted representation. Active is a pointer: Core's
// SoftForkDescPushBack unconditionally emits "active" for both buried and
// bip9 deployments, so a missing/null value is a strict-decode failure,
// never silently treated as false. Height is a pointer because Core only
// includes it for buried deployments and for bip9 deployments whose
// status_next is ACTIVE — internal/deployments enforces presence
// per-type (required for buried, optional for bip9).
type RawDeployment struct {
	Type   string             `json:"type"`
	Active *bool              `json:"active"`
	Height *int64             `json:"height,omitempty"`
	BIP9   *RawBIP9Deployment `json:"bip9,omitempty"`
}

// RawBIP9Deployment mirrors Core's per-deployment "bip9" object. Fields
// Core only sometimes emits (bit, statistics, signalling) are pointers.
// StartTime/Timeout/MinActivationHeight/Since are also pointers even
// though Core emits them on every bip9 deployment: without a pointer,
// encoding/json cannot distinguish "field absent/null" from a legitimate
// zero value (start_time/since/min_activation_height can genuinely be 0),
// which would let a malformed Core response silently pass strict
// decoding (docs/ARCHITECTURE.md §24). Status/StatusNext stay plain
// strings because the empty string they'd decode to on a missing/null
// value is already outside validBIP9Statuses and is rejected by the
// existing enum check.
type RawBIP9Deployment struct {
	Bit                 *int               `json:"bit,omitempty"`
	StartTime           *int64             `json:"start_time"`
	Timeout             *int64             `json:"timeout"`
	MinActivationHeight *int64             `json:"min_activation_height"`
	Status              string             `json:"status"`
	StatusNext          string             `json:"status_next"`
	Since               *int64             `json:"since"`
	Statistics          *RawBIP9Statistics `json:"statistics,omitempty"`
	Signalling          *string            `json:"signalling,omitempty"`
}

// RawBIP9Statistics mirrors Core's per-deployment "bip9.statistics"
// object, present only while Core is actively tallying signalling for the
// current period (typically STARTED, sometimes LOCKED_IN). Period,
// Elapsed, and Count are pointers: Core always emits all three whenever
// the statistics object itself is present, and each can legitimately be
// 0 (Elapsed/Count at the very start of a period), so only a pointer lets
// a missing/null value be told apart from a real zero.
type RawBIP9Statistics struct {
	Period    *int64 `json:"period"`
	Threshold *int64 `json:"threshold,omitempty"`
	Elapsed   *int64 `json:"elapsed"`
	Count     *int64 `json:"count"`
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
