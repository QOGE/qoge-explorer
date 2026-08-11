package api

import (
	"net/http"
	"regexp"
	"strconv"
)

// hash64 matches exactly 64 lowercase hex characters — the format
// migrations/0001_initial.up.sql's CHECK constraints enforce for every
// hash/txid/wtxid column. Uppercase input is deliberately rejected, never
// normalized — see docs/ARCHITECTURE.md §19 "Identifier validation".
var hash64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isHash64(s string) bool {
	return hash64.MatchString(s)
}

// maxAddressLength is a reasonable input-shape bound only — this
// deliberately does not attempt to duplicate full consensus address
// validation (Base58Check/Bech32 decoding, version byte checks); the query
// layer treats any string as an opaque address key.
const maxAddressLength = 128

func isValidAddressShape(s string) bool {
	if s == "" || len(s) > maxAddressLength {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e { // printable ASCII, no whitespace/control bytes
			return false
		}
	}
	return true
}

// parseHeight parses a non-negative decimal height. Leading '+' or any
// non-digit character is rejected — strconv.ParseInt with base 10 already
// enforces this for everything except a leading '-', checked separately.
func parseHeight(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// blockIdentifierKind reports how to interpret a GET /api/v1/block/{id}
// path segment: exactly 64 lowercase hex chars is a hash; otherwise, if it
// parses as a non-negative decimal integer, it's a height. Anything else is
// malformed.
func blockIdentifierKind(id string) (height int64, isHeight bool, isHash bool) {
	if isHash64(id) {
		return 0, false, true
	}
	if h, ok := parseHeight(id); ok {
		return h, true, false
	}
	return 0, false, false
}

// queryParamInt reads r's query parameter name as a non-negative decimal
// integer. Absent/empty returns (0, false, true) — "not present" is valid;
// present-but-malformed returns ok=false so the caller can 400.
func queryParamInt(r *http.Request, name string) (value int64, present bool, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false, true
	}
	n, valid := parseHeight(raw)
	if !valid {
		return 0, true, false
	}
	return n, true, true
}
