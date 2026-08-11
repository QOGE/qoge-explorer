package web

import (
	"html/template"
	"time"
)

// templateFuncs are pure presentation helpers only — no query.Store access,
// no script classification, no accounting. Every value they touch has
// already been decided by internal/query; these only format it for
// display.
var templateFuncs = template.FuncMap{
	"formatTimeUTC": formatTimeUTC,
	"spentLabel":    spentLabel,
	"yesNo":         yesNo,
}

// formatTimeUTC renders a block header's Unix timestamp as an unambiguous
// absolute UTC time — see docs/ARCHITECTURE.md §20 "Time display": never
// server-local, always explicit about the "UTC" unit.
func formatTimeUTC(unixSeconds int64) string {
	return time.Unix(unixSeconds, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

// spentLabel distinguishes the three real states an output's Spent field
// carries (see query.OutputDetail.Spent's doc comment): nil means "never
// tracked in canonical UTXO state at all" (a genesis output or any
// unspendable script), which is NOT the same as "unspent" — the web layer
// preserves that distinction rather than collapsing it to a plain
// spent/unspent toggle.
func spentLabel(spent *bool) string {
	if spent == nil {
		return "not tracked (unspendable or genesis)"
	}
	if *spent {
		return "spent"
	}
	return "unspent"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
