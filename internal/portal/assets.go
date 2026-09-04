package portal

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
)

// The SPA is served from the embedded web/ directory. Two things are worth
// knowing about the caching here:
//
//   - assetVersion is a hash over every embedded asset, computed once at start.
//     index.html carries it on its own <link>/<script> URLs, so changing the CSS
//     changes the URL and a returning browser cannot pair new HTML with stale
//     CSS. Anything reached with the current version is immutable for a year;
//     anything else revalidates.
//   - Fonts are immutable regardless. Their names carry the family and the
//     subset, so a different font is a different file name — we never rewrite
//     one in place.
//
// The portal has no build step and no CDN in front of its own origin, so this
// is deliberately the whole of it: no manifest, no fingerprinted file names on
// disk, nothing a contributor has to run before editing a stylesheet.
var (
	assetVersion string
	indexPage    []byte
)

func init() {
	sum := sha256.New()
	var names []string
	fs.WalkDir(assets, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names = append(names, p)
		return nil
	})
	sort.Strings(names) // WalkDir is already lexical; make the dependency explicit
	for _, n := range names {
		b, err := assets.ReadFile(n)
		if err != nil {
			continue
		}
		sum.Write([]byte(n))
		sum.Write(b)
	}
	assetVersion = hex.EncodeToString(sum.Sum(nil))[:10]

	b, _ := assets.ReadFile("web/index.html")
	indexPage = []byte(strings.ReplaceAll(string(b), "__V__", assetVersion))
}

// handleAsset serves the SPA's stylesheet, script, fonts and favicon. It is
// deliberately unauthenticated: none of it is secret, and the login and
// pending-invite pages want the same typography as the app behind them.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	// path.Clean collapses any ".." before it can leave the directory; the
	// embedded FS would reject an escape anyway, but failing early is clearer.
	name = path.Clean("/" + name)[1:]
	// HTML never comes out of here. web/ also holds index.html and the three
	// page templates, and handing those out raw would serve a page with its
	// placeholders still in it — /assets/index.html used to do exactly that.
	if name == "" || strings.HasPrefix(name, "..") || strings.HasSuffix(name, ".html") {
		http.NotFound(w, r)
		return
	}
	b, err := assets.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	immutable := strings.HasPrefix(name, "fonts/") || r.URL.Query().Get("v") == assetVersion
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", `"`+assetVersion+`"`)
		if strings.Contains(r.Header.Get("If-None-Match"), assetVersion) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Write(b)
}
