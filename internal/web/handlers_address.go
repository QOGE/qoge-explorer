package web

import "net/http"

// handleAddress resolves GET /address/{address}, mirroring internal/api's
// handleAddress/handleAddressHistory semantics exactly. Balance/accounting
// (AddressSummary) and historical destination visibility (AddressHistory)
// are deliberately independent — a genesis-only P2PK destination may show
// a zero balance while still showing its canonical genesis transaction in
// history; the template renders whatever the query layer returns without
// trying to reconcile the two (see query.Store.AddressHistory's doc
// comment and docs/ARCHITECTURE.md §20).
func (s *Server) handleAddress(w http.ResponseWriter, r *http.Request) {
	address := r.PathValue("address")
	if !isValidAddressShape(address) {
		s.renderBadRequest(w, "Malformed address.")
		return
	}

	summary, err := s.q.AddressSummary(r.Context(), address)
	if err != nil {
		s.renderInternalError(w, "address summary", err)
		return
	}

	beforeHeight, ok := parseBeforeHeight(r)
	if !ok {
		s.renderBadRequest(w, "Malformed \"before\" query parameter: must be a non-negative decimal integer.")
		return
	}
	beforeTxID := r.URL.Query().Get("before_txid")
	if beforeTxID != "" && !isHash64(beforeTxID) {
		s.renderBadRequest(w, "Malformed \"before_txid\" query parameter: must be exactly 64 lowercase hex characters.")
		return
	}
	if (beforeHeight != nil) != (beforeTxID != "") {
		s.renderBadRequest(w, "\"before\" and \"before_txid\" must both be supplied together, or both omitted.")
		return
	}
	var beforeTxIDPtr *string
	if beforeTxID != "" {
		beforeTxIDPtr = &beforeTxID
	}

	limit, ok := parsePageSize(r)
	if !ok {
		s.renderBadRequest(w, "Malformed \"limit\" query parameter: must be a non-negative decimal integer.")
		return
	}

	history, err := s.q.AddressHistory(r.Context(), address, beforeHeight, beforeTxIDPtr, limit)
	if err != nil {
		s.renderInternalError(w, "address history", err)
		return
	}

	s.render(w, "address", http.StatusOK, addressView{
		Summary:    summary,
		History:    history.Transactions,
		Pagination: addressPagination(address, history.NextBeforeHeight, history.NextBeforeTxID),
	})
}
