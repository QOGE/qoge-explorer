package web

import "net/http"

// errorView backs templates/error.tmpl. Message is always a fixed,
// hardcoded string chosen by the caller below — never a raw Go error or
// any blockchain-derived value — so there is nothing here that could leak
// a SQL string, a database URL, or a Go internal error (see
// docs/ARCHITECTURE.md §20 "web error pages never leak internals").
type errorView struct {
	Status  int
	Title   string
	Message string
}

func (s *Server) renderError(w http.ResponseWriter, status int, title, message string) {
	s.render(w, "error", status, errorView{Status: status, Title: title, Message: message})
}

func (s *Server) renderBadRequest(w http.ResponseWriter, message string) {
	s.renderError(w, http.StatusBadRequest, "Bad Request", message)
}

func (s *Server) renderNotFound(w http.ResponseWriter, message string) {
	s.renderError(w, http.StatusNotFound, "Not Found", message)
}

// renderInternalError logs the real error server-side (never to the
// client) and renders a generic HTML 500 — mirrors
// internal/api/errors.go's writeInternalError, HTML instead of JSON.
func (s *Server) renderInternalError(w http.ResponseWriter, context string, err error) {
	if s.log != nil {
		s.log.Error("web: internal error", "context", context, "error", err)
	}
	s.renderError(w, http.StatusInternalServerError, "Internal Server Error", "Something went wrong. Please try again later.")
}

// render executes page's "layout" template with data, writing status first.
// A template execution failure after WriteHeader can't change the response
// code any more — it is logged server-side only, matching what
// internal/api's writeJSON/writeError already accept for the symmetric
// json.Encode failure case.
func (s *Server) render(w http.ResponseWriter, page string, status int, data any) {
	t, ok := s.tmpl[page]
	if !ok {
		// Unreachable in production: page names are a fixed compile-time
		// set enumerated in pageTemplateFiles and always match what
		// handlers pass — see TestTemplatesParse.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout", data); err != nil && s.log != nil {
		s.log.Error("web: render template failed", "page", page, "error", err)
	}
}
