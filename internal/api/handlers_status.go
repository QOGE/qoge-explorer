package api

import "net/http"

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.q.Status(r.Context())
	if err != nil {
		writeInternalError(s.log, w, "status", err)
		return
	}
	writeJSON(w, status)
}
