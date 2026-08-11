package web

import (
	"net/http"
	"regexp"
	"strconv"
)

// hash64/isHash64/maxAddressLength/isValidAddressShape/parseHeight mirror
// internal/api/validate.go exactly — duplicated per-package on purpose (see
// internal/api/dbtest_test.go's note on this repo's cross-package
// duplication convention): internal/web must not import internal/api, and
// both packages independently need the same conservative identifier-shape
// checks ahead of a query.Store call.
var hash64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isHash64(s string) bool {
	return hash64.MatchString(s)
}

const maxAddressLength = 128

func isValidAddressShape(s string) bool {
	if s == "" || len(s) > maxAddressLength {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

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

// blockIdentifierKind mirrors internal/api's: exactly 64 lowercase hex
// chars is a hash; otherwise, if it parses as a non-negative decimal
// integer, it's a height. Anything else is malformed.
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
// present-but-malformed returns ok=false so the caller can render a 400
// page.
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

// parsePageSize reads the "limit" query parameter; the query layer
// (query.clampPageSize) enforces the hard maximum regardless.
func parsePageSize(r *http.Request) (limit int, ok bool) {
	n, present, valid := queryParamInt(r, "limit")
	if !present {
		return 0, true
	}
	if !valid {
		return 0, false
	}
	return int(n), true
}

// parseBeforeHeight reads the "before" query parameter as a keyset
// pagination cursor (an exclusive upper bound on block height).
func parseBeforeHeight(r *http.Request) (before *int64, ok bool) {
	n, present, valid := queryParamInt(r, "before")
	if !present {
		return nil, true
	}
	if !valid {
		return nil, false
	}
	return &n, true
}

// maxSearchLength bounds /search?q= input — a courtesy shape check, not an
// attempt at full consensus address validation (see
// isValidAddressShape's doc comment).
const maxSearchLength = 128
