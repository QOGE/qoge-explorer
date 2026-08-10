package api

import "net/http"

// parsePageSize reads the "limit" query parameter. Absent means "use the
// query layer's default"; present-but-malformed is a 400, reported by the
// caller. The query layer (query.clampPageSize) enforces the hard maximum
// regardless of what's requested here — this is a courtesy early check,
// not the only enforcement point.
func parsePageSize(r *http.Request) (limit int, ok bool) {
	n, present, valid := queryParamInt(r, "limit")
	if !present {
		return 0, true // 0 tells the query layer "use its default"
	}
	if !valid {
		return 0, false
	}
	return int(n), true
}

// parseBeforeHeight reads the "before" query parameter as a keyset
// pagination cursor (an exclusive upper bound on block height). Absent
// means "start from the top" (nil, true).
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
