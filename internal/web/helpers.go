package web

import (
	"html/template"
	"net/url"
	"strings"
	"time"
)

// templateFuncs are pure presentation helpers only — no query.Store access,
// no script classification, no accounting. Every value they touch has
// already been decided by internal/query; these only format it for
// display.
var templateFuncs = template.FuncMap{
	"formatTimeUTC":    formatTimeUTC,
	"spentLabel":       spentLabel,
	"yesNo":            yesNo,
	"formatObservedAt": formatObservedAt,
	"replaceableLabel": replaceableLabel,
	"prevOutLink":      prevOutLink,
	"optionalYesNo":    optionalYesNo,
	"deploymentPath":   deploymentPath,
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

// formatObservedAt renders mempool_state.observed_at (nil when the mempool
// cache has never successfully synchronized — see query.MempoolState) as an
// unambiguous absolute UTC time, same convention as formatTimeUTC.
func formatObservedAt(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// prevOutLink returns the correct navigation target for a mempool
// transaction input's previous output. A mempool transaction input can
// spend EITHER another current mempool transaction OR an already-confirmed
// one — Core's "depends" list (query.MempoolTransactionDetail.Depends,
// already loaded into every detail response) is the only reliable, already
// -available signal for which: prevTxid links to "/mempool/tx/{prevTxid}"
// only when it's one of THIS transaction's own in-mempool parents,
// otherwise "/tx/{prevTxid}" (confirmed). This never queries the database
// merely to choose a link — see docs/ARCHITECTURE.md §23.
func prevOutLink(prevTxid string, depends []string) string {
	for _, d := range depends {
		if d == prevTxid {
			return "/mempool/tx/" + prevTxid
		}
	}
	return "/tx/" + prevTxid
}

// replaceableLabel distinguishes the three states Core's
// "bip125-replaceable" metadata carries (see
// query.MempoolTxSummary.Replaceable/MempoolTransactionDetail.Replaceable's
// doc comments): nil means Core did not report reliable RBF metadata for
// this transaction — never presented as a false "no".
func replaceableLabel(b *bool) string {
	return optionalYesNo(b)
}

// optionalYesNo renders a tri-state *bool (nil / true / false) as
// "unknown"/"yes"/"no". Only use this where nil genuinely means "not
// reliably known" — e.g. BIP125 replaceability metadata. It is NOT
// appropriate for BIP9 bip9.statistics.possible: Core omits that field for
// LOCKED_IN deployments as a structural consequence of that state, not
// because the value is unknown (see deployment.tmpl, which renders the
// Possible row conditionally instead of calling this helper).
func optionalYesNo(b *bool) string {
	if b == nil {
		return "unknown"
	}
	return yesNo(*b)
}

// deploymentPath returns the URL for a deployment's detail page such that
// the {name} wildcard in "GET /deployments/{name}" decodes back to the
// exact original name. The Phase 2G.1 writer accepts any non-empty name up
// to the length bound (internal/deployments' validateDeploymentName) — this
// helper must not narrow that namespace, only encode it safely for a
// single URL path segment.
//
// url.PathEscape handles ordinary reserved characters (/, ?, #, %, ...).
// It leaves '.' unescaped, which is not enough on its own: a segment that
// is exactly "." or ".." is collapsed by net/http.ServeMux's canonical
// -path redirect before the {name} route ever sees it. Percent-encoding
// every literal '.' as %2E sidesteps that unconditionally, for any name.
func deploymentPath(name string) string {
	escaped := url.PathEscape(name)
	escaped = strings.ReplaceAll(escaped, ".", "%2E")
	return "/deployments/" + escaped
}
