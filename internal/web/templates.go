package web

import (
	"embed"
	"fmt"
	"html/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

//go:embed static/app.css
var staticFS embed.FS

// pageTemplates are the shared layout/partials every page template is
// parsed together with. Each page below additionally defines a "body"
// block, which layout.tmpl invokes via {{template "body" .}} — parsing
// each page as its own *template.Template (rather than one combined set)
// is what lets every page reuse the SAME "body" template name without a
// name collision (a well-known html/template idiom for a shared layout).
var sharedTemplateFiles = []string{
	"templates/layout.tmpl",
	"templates/header.tmpl",
	"templates/footer.tmpl",
	"templates/pagination.tmpl",
	"templates/blocktable.tmpl",
}

var pageTemplateFiles = map[string]string{
	"home":    "templates/home.tmpl",
	"blocks":  "templates/blocks.tmpl",
	"block":   "templates/block.tmpl",
	"tx":      "templates/tx.tmpl",
	"address": "templates/address.tmpl",
	"search":  "templates/search.tmpl",
	"error":   "templates/error.tmpl",
}

// pages maps a page name to its fully-parsed layout+page *template.Template
// (rendered via ExecuteTemplate(w, "layout", data)).
type pages map[string]*template.Template

// loadTemplates parses every embedded page once at Server construction. A
// parse error here is a bug in the embedded templates themselves — a
// build-time invariant, not a runtime/data condition — and is always
// caught immediately by TestTemplatesParse (every page is parsed in every
// test run) and by New's own panic (see New's doc comment), never
// silently deferred to whichever request happens to hit that page first.
func loadTemplates() (pages, error) {
	p := pages{}
	for name, file := range pageTemplateFiles {
		files := append(append([]string{}, sharedTemplateFiles...), file)
		t, err := template.New("layout").Funcs(templateFuncs).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("web: parse template %s: %w", name, err)
		}
		p[name] = t
	}
	return p, nil
}
