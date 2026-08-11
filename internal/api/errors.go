package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// apiError is the JSON error body shape every non-2xx response uses. It
// never carries a SQL string, a database/RPC credential, or a Go stack
// trace — see docs/ARCHITECTURE.md §19.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{apiError{Code: code, Message: message}})
}

func writeBadRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, "bad_request", message)
}

func writeNotFound(w http.ResponseWriter, message string) {
	writeError(w, http.StatusNotFound, "not_found", message)
}

// writeMethodNotAllowed sets an Allow header (the RFC 7231-recommended way
// to advertise which methods a resource actually accepts) alongside the
// JSON error envelope.
func writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

// writeInternalError logs the real error server-side (never a stack trace
// to the client) and returns a generic 500 body that never leaks a SQL
// string or any internal detail.
func writeInternalError(log *slog.Logger, w http.ResponseWriter, context string, err error) {
	if log != nil {
		log.Error("api: internal error", "context", context, "error", err)
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
