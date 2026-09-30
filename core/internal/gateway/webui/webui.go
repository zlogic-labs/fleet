// Package webui serves the console as static assets embedded in the binary.
//
// Embedding rather than shipping a separate container is deliberate: the
// gateway is the only component a self-hosted operator has to run, and a
// second image for a page that is a few kilobytes of HTML, CSS and JavaScript
// would double the deployment surface for nothing.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed assets
var embedded embed.FS

// assetPrefix is where the console's own files live, both on the embedded FS
// and in the URL. Keeping them equal is what lets one http.FileServer serve
// the tree without a second rewrite layer.
const assetPrefix = "/assets/"

// Handler serves the console at / with its assets under /assets/.
//
// An unknown path falls back to index.html so the console can own its own
// client-side routing. That is safe here because there is exactly one screen
// and no server-rendered URLs; a multi-route console would need it removed and
// a 404 restored.
func Handler() http.Handler {
	// The FS root is the embed root, not the assets subdirectory: a request
	// for /assets/app.css has to resolve to assets/app.css, and stripping the
	// prefix here would make FileServer look for app.css at the top level.
	files := http.FileServer(http.FS(embedded))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleaned := path.Clean("/" + r.URL.Path)
		if strings.HasPrefix(cleaned, assetPrefix) {
			name := strings.TrimPrefix(cleaned, "/")
			if _, err := fs.Stat(embedded, name); err == nil {
				w.Header().Set("Cache-Control", "no-cache")
				files.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w)
	})
}

func serveIndex(w http.ResponseWriter) {
	body, err := embedded.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "console not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The console is replaced wholesale between releases. A cached shell
	// pointing at a renamed script is a confusing bug report, and the assets
	// are small enough that revalidating them costs nothing.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}
