package deployments

import "errors"

var (
	// ErrConfirmedIndexNotReady means the confirmed PostgreSQL checkpoint
	// is either uninitialized (never synced) or does not currently match
	// Core's active-chain tip (docs/ARCHITECTURE.md §24, spec item 15
	// steps A/B). Publishing a deployment snapshot in either case would
	// anchor it to a confirmed-state view this phase cannot yet trust
	// against. refreshOnce returns this instead of publishing; Run treats
	// it as an ordinary skip (debug-logged), never a failure — historical
	// confirmed indexing continues normally regardless (spec item 24).
	ErrConfirmedIndexNotReady = errors.New("deployments: confirmed index is uninitialized or behind core's active tip; skipping publication")

	// ErrDeploymentRace means Core's active tip or the confirmed
	// checkpoint changed during snapshot acquisition, or
	// getdeploymentinfo's own reported hash/height anchor didn't match
	// the confirmed checkpoint it was queried against (docs/
	// ARCHITECTURE.md §24, spec item 15 steps C/G). This is a normal,
	// expected condition, not corruption: refreshOnce discards the
	// candidate and the previously committed snapshot is preserved
	// untouched; Run retries on the next poll.
	ErrDeploymentRace = errors.New("deployments: core active tip, confirmed checkpoint, or getdeploymentinfo anchor disagreed during snapshot acquisition")
)
