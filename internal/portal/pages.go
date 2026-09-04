package portal

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
)

// The three pages a visitor can meet before the app itself: sign in, waiting
// for an invite, and authorizing a device. They are ordinary files under web/
// rather than Go string constants, for two reasons:
//
//   - tools/portalpreview can then serve the same bytes the server does. The
//     constants had to be re-derived by a throwaway script that re-implemented
//     the format verbs, and a preview that re-derives its subject is a preview
//     that can disagree with it.
//   - html/template escapes by context, so the one value that comes from
//     outside (a GitHub login) cannot end up interpreted as markup. The
//     constants were fmt.Fprintf with a hand-rolled escaper.
//
// All three share /assets/app.css and /assets/auth.js with the app. That is
// what handleAsset's deliberate lack of authentication is for.
var pages = template.Must(template.ParseFS(assets, "web/login.html", "web/pending.html", "web/enroll.html"))

// render writes one of those pages. It buffers first: a template that fails
// halfway would otherwise have already sent 200 plus half a page, which reads
// to the visitor as a broken product rather than as a server error.
func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["V"] = assetVersion
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "page template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// publicHost is the instance as the visitor's address bar shows it, port and
// all. The sign-in page prints it because "which wanctl am I about to hand my
// GitHub identity to" is the question that page exists to answer.
func (s *Server) publicHost(r *http.Request) string {
	if u, err := url.Parse(s.requestOrigin(r)); err == nil && u.Host != "" {
		return u.Host
	}
	return r.Host
}
