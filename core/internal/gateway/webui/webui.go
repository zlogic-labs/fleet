// Package webui serves the console as static assets embedded in the binary.
//
// Embedding rather than shipping a separate container is deliberate: the
// gateway is the only component a self-hosted operator has to run, and a
// second image for the console would double the deployment surface.
//
// The dist directory is produced by `make web` and is not in version control.
// A placeholder page is compiled in so that `go build` works in a fresh clone
// and the operator is told how to build the console rather than being handed
// a blank page.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist is the vite output directory, staged here by `make web`. It is
// embedded whole — including its own assets/ subdirectory — so the web root is
// a sub-FS of the embed root rather than the embed root itself. Slicing it
// here is what keeps the URL /assets/app.js resolving to dist/assets/app.js
// instead of to dist/app.js.
//
//go:embed all:dist
var embedded embed.FS

// assetPrefix is where the console's own files live, in both the URL and the
// embedded dist tree. Vite names this directory too, so they line up.
const assetPrefix = "/assets/"

// Handler serves the console at / with its assets under /assets/.
//
// An unknown path falls back to index.html so the console can own its own
// client-side routing. Every path that matches a real file is served as a
// file first, so a stale deep link cannot shadow an asset.
func Handler() http.Handler {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		// Unreachable: the embed directive guarantees the directory exists.
		panic("webui: embedded dist missing: " + err.Error())
	}

	// The FS root is dist, not dist/assets: a request for /assets/app.js has
	// to resolve to dist/assets/app.js, and stripping the prefix here would
	// make FileServer look for app.js at the top level.
	files := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleaned := path.Clean("/" + r.URL.Path)
		if strings.HasPrefix(cleaned, assetPrefix) {
			name := strings.TrimPrefix(cleaned, "/")
			if _, statErr := fs.Stat(dist, name); statErr == nil {
				// Vite content-hashes everything under assets/, so those files
				// can be cached forever. The entry document cannot: it names
				// the hashed chunks, so a cached copy pins a stale bundle.
				if strings.HasSuffix(name, ".html") {
					w.Header().Set("Cache-Control", "no-cache")
				} else {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, dist)
	})
}

func serveIndex(w http.ResponseWriter, dist fs.FS) {
	body, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		writePlaceholder(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func writePlaceholder(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(placeholder))
}

const placeholder = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Fleet</title>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0d1117;color:#e6edf3;
font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif}
main{max-width:34rem;padding:2rem}
h1{font-size:1.3rem;margin:0 0 .75rem}
p{color:#8b949e;margin:0 0 1rem}
code,pre{font-family:ui-monospace,Menlo,Consolas,monospace}
code{background:#161b22;border:1px solid #30363d;border-radius:5px;padding:.15em .45em;color:#79c0ff}
pre{background:#161b22;border:1px solid #30363d;border-radius:6px;padding:.9rem 1.1rem;overflow-x:auto}
</style></head>
<body><main>
<h1>The Fleet console has not been built</h1>
<p>This binary embeds the operator console, which is a separate npm project.
Build it once with:</p>
<pre>make web</pre>
<p>or, from <code>web/</code>: <code>npm install &amp;&amp; npm run build</code>.
The gateway serves the console at <code>/</code>.</p>
</main></body></html>
`
