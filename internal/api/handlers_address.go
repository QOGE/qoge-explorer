package api

import "net/http"

func (s *Server) handleAddress(w http.ResponseWriter, r *http.Request) {
	address := r.PathValue("address")
	if !isValidAddressShape(address) {
		writeBadRequest(w, "malformed address")
		return
	}

	summary, err := s.q.AddressSummary(r.Context(), address)
	if err != nil {
		writeInternalError(s.log, w, "address summary", err)
		return
	}
	writeJSON(w, summary)
}

func (s *Server) handleAddressHistory(w http.ResponseWriter, r *http.Request) {
	address := r.PathValue("address")
	if !isValidAddressShape(address) {
		writeBadRequest(w, "malformed address")
		return
	}

	beforeHeight, ok := parseBeforeHeight(r)
	if !ok {
		writeBadRequest(w, "malformed \"before\" query parameter: must be a non-negative decimal integer")
		return
	}
	beforeTxID := r.URL.Query().Get("before_txid")
	if beforeTxID != "" && !isHash64(beforeTxID) {
		writeBadRequest(w, "malformed \"before_txid\" query parameter: must be exactly 64 lowercase hex characters")
		return
	}
	if (beforeHeight != nil) != (beforeTxID != "") {
		writeBadRequest(w, "\"before\" and \"before_txid\" must both be supplied together, or both omitted")
		return
	}
	var beforeTxIDPtr *string
	if beforeTxID != "" {
		beforeTxIDPtr = &beforeTxID
	}

	limit, ok := parsePageSize(r)
	if !ok {
		writeBadRequest(w, "malformed \"limit\" query parameter: must be a non-negative decimal integer")
		return
	}

	page, err := s.q.AddressHistory(r.Context(), address, beforeHeight, beforeTxIDPtr, limit)
	if err != nil {
		writeInternalError(s.log, w, "address history", err)
		return
	}
	writeJSON(w, page)
}
