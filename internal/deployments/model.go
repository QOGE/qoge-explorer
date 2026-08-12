package deployments

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/QOGE/qoge-explorer/internal/rpc"
)

// validBIP9Statuses is the exact, closed set of values Core's
// bip9.status/bip9.status_next may report (spec item 9). Never extended
// or normalized — an unrecognized value is a strict decode failure, not
// silently mapped to a known one.
var validBIP9Statuses = map[string]bool{
	"defined":   true,
	"started":   true,
	"locked_in": true,
	"active":    true,
	"failed":    true,
}

// maxDeploymentNameLength bounds a deployment map key to a "reasonable
// length" (spec item 9) — generous enough for any real Core deployment
// name, small enough to reject a clearly malformed/hostile response.
const maxDeploymentNameLength = 128

// CandidateDeployment is one BIP9 deployment observed from Core's
// getdeploymentinfo, strictly decoded and validated, ready to persist
// into chain_deployments. Buried deployments never reach this type — see
// DecodeDeploymentInfo.
type CandidateDeployment struct {
	// Name is the deployment map key exactly as Core reported it (e.g.
	// "p2qpk") — never invented, never renamed.
	Name string

	// Status is bip9.status EXACTLY, one of validBIP9Statuses.
	Status string

	// SinceHeight is bip9.since exactly.
	SinceHeight int64

	// RawJSON is the EXACT bytes of the complete deployment object Core
	// returned for Name — never reconstructed from Status/SinceHeight
	// after decoding (spec item 11). Persisted as-is into
	// chain_deployments.raw_json.
	RawJSON json.RawMessage
}

// Candidate is one complete, not-yet-persisted deployment snapshot,
// anchored to a specific confirmed/Core tip observed during acquisition —
// see sync.go's anchored acquisition algorithm and
// docs/ARCHITECTURE.md §24.
type Candidate struct {
	CoreTipHeight int64
	CoreTipHash   string

	// ObservedAt is the single observation timestamp shared by every
	// deployment row in this snapshot (spec item 12) and mirrored onto
	// deployment_state.observed_at.
	ObservedAt time.Time

	Deployments []CandidateDeployment
}

// validate enforces the structural invariants Store.ReplaceSnapshot
// requires before ever touching PostgreSQL — defense in depth alongside
// DecodeDeploymentInfo's own strict decoding, the same "Store validation
// must not rely on the caller always being the RPC decoder" posture
// internal/mempool.Candidate.validate documents (spec item 14).
func (c Candidate) validate() error {
	if err := validateHexHash(c.CoreTipHash); err != nil {
		return fmt.Errorf("deployments: candidate core tip hash: %w", err)
	}
	if c.CoreTipHeight < 0 {
		return fmt.Errorf("deployments: candidate core tip height %d is negative", c.CoreTipHeight)
	}
	if c.ObservedAt.IsZero() {
		return fmt.Errorf("deployments: candidate observed_at is zero")
	}

	names := make(map[string]bool, len(c.Deployments))
	for _, d := range c.Deployments {
		if err := validateDeploymentName(d.Name); err != nil {
			return fmt.Errorf("deployments: candidate: %w", err)
		}
		if names[d.Name] {
			return fmt.Errorf("deployments: candidate has duplicate deployment name %q", d.Name)
		}
		names[d.Name] = true

		if !validBIP9Statuses[d.Status] {
			return fmt.Errorf("deployments: candidate deployment %q has invalid status %q", d.Name, d.Status)
		}
		if d.SinceHeight < 0 {
			return fmt.Errorf("deployments: candidate deployment %q has negative since_height %d", d.Name, d.SinceHeight)
		}
		if len(d.RawJSON) == 0 {
			return fmt.Errorf("deployments: candidate deployment %q has empty raw_json", d.Name)
		}
	}
	return nil
}

func validateDeploymentName(name string) error {
	if name == "" {
		return fmt.Errorf("deployment name is empty")
	}
	if len(name) > maxDeploymentNameLength {
		return fmt.Errorf("deployment name %q exceeds %d bytes", name, maxDeploymentNameLength)
	}
	return nil
}

func validateHexHash(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("invalid hash %q: want 64 hex characters, got %d", s, len(s))
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("invalid hash %q: not lowercase hex", s)
		}
	}
	return nil
}

// DecodedResponse is the strictly-decoded, validated result of one
// getdeploymentinfo call: Core's exact hash/height anchor for the queried
// block, plus every BIP9 deployment it reported. Buried deployments are
// decoded just enough to prove they aren't malformed, then intentionally
// dropped (spec items 3/33) — Deployments contains ONLY type=="bip9"
// entries.
type DecodedResponse struct {
	Hash        string
	Height      int64
	Deployments []CandidateDeployment
}

// DecodeDeploymentInfo strictly decodes and validates one Core
// getdeploymentinfo response (spec item 9). It rejects any malformed or
// out-of-range field rather than silently normalizing it, and respects
// Core's actual field optionality (spec item 10) — it never requires
// bit/statistics/threshold/possible/signalling/height in a state where
// Core legitimately omits them, and never invents a value for a field
// Core didn't report.
func DecodeDeploymentInfo(info rpc.RawDeploymentInfo) (DecodedResponse, error) {
	if err := validateHexHash(info.Hash); err != nil {
		return DecodedResponse{}, fmt.Errorf("deployments: response hash: %w", err)
	}
	if info.Height < 0 {
		return DecodedResponse{}, fmt.Errorf("deployments: response height %d is negative", info.Height)
	}

	out := make([]CandidateDeployment, 0, len(info.Deployments))
	for name, raw := range info.Deployments {
		if err := validateDeploymentName(name); err != nil {
			return DecodedResponse{}, fmt.Errorf("deployments: %w", err)
		}

		var d rpc.RawDeployment
		if err := json.Unmarshal(raw, &d); err != nil {
			return DecodedResponse{}, fmt.Errorf("deployments: deployment %q: decode: %w", name, err)
		}

		switch d.Type {
		case "buried":
			// Decoded enough to prove it isn't malformed; intentionally
			// not persisted here — buried deployments are static
			// historical consensus rules with no BIP9 status model
			// (spec item 3; docs/ARCHITECTURE.md §24).
			if d.Height != nil && *d.Height < 0 {
				return DecodedResponse{}, fmt.Errorf("deployments: buried deployment %q has negative height %d", name, *d.Height)
			}
		case "bip9":
			cd, err := decodeBIP9Deployment(name, d, raw)
			if err != nil {
				return DecodedResponse{}, err
			}
			out = append(out, cd)
		default:
			return DecodedResponse{}, fmt.Errorf("deployments: deployment %q has unrecognized type %q (want %q or %q)", name, d.Type, "buried", "bip9")
		}
	}

	return DecodedResponse{Hash: info.Hash, Height: info.Height, Deployments: out}, nil
}

// decodeBIP9Deployment strictly validates one type=="bip9" deployment
// object per spec item 9's field-by-field rules, respecting spec item
// 10's optionality (only bit/statistics/statistics.threshold/
// statistics.possible/signalling/top-level height are ever allowed to be
// absent — start_time/timeout/min_activation_height/status/status_next/
// since are present on every bip9 deployment Core reports and are
// decoded as plain, non-optional fields).
func decodeBIP9Deployment(name string, d rpc.RawDeployment, raw json.RawMessage) (CandidateDeployment, error) {
	if d.BIP9 == nil {
		return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has type %q but no %q object", name, "bip9", "bip9")
	}
	b := d.BIP9

	if !validBIP9Statuses[b.Status] {
		return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has invalid bip9.status %q", name, b.Status)
	}
	if !validBIP9Statuses[b.StatusNext] {
		return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has invalid bip9.status_next %q", name, b.StatusNext)
	}
	if b.Since < 0 {
		return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has negative bip9.since %d", name, b.Since)
	}
	if b.Bit != nil && (*b.Bit < 0 || *b.Bit > 28) {
		return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has bip9.bit %d out of range [0,28]", name, *b.Bit)
	}
	if b.MinActivationHeight < 0 {
		return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has negative bip9.min_activation_height %d", name, b.MinActivationHeight)
	}
	if d.Height != nil && *d.Height < 0 {
		return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has negative top-level height %d", name, *d.Height)
	}

	if b.Statistics != nil {
		st := b.Statistics
		if st.Period <= 0 {
			return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has bip9.statistics.period %d, want > 0", name, st.Period)
		}
		if st.Elapsed < 0 || st.Elapsed > st.Period {
			return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has bip9.statistics.elapsed %d out of range [0,%d]", name, st.Elapsed, st.Period)
		}
		if st.Count < 0 || st.Count > st.Elapsed {
			return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has bip9.statistics.count %d out of range [0,%d]", name, st.Count, st.Elapsed)
		}
		if st.Threshold != nil && (*st.Threshold <= 0 || *st.Threshold > st.Period) {
			return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has bip9.statistics.threshold %d out of range (0,%d]", name, *st.Threshold, st.Period)
		}
	}

	if b.Signalling != nil {
		for _, r := range *b.Signalling {
			if r != '#' && r != '-' {
				return CandidateDeployment{}, fmt.Errorf("deployments: deployment %q has bip9.signalling containing invalid character %q (only '#' and '-' allowed)", name, r)
			}
		}
	}

	return CandidateDeployment{
		Name:        name,
		Status:      b.Status,
		SinceHeight: b.Since,
		RawJSON:     raw,
	}, nil
}
