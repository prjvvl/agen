// Package ui serves the web UI (web/, built into ./dist by
// scripts/build-web.sh) from the Hub at "/". The UI is a client of the Hub
// API with the user's token, like the CLI.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

const notBuilt = `<!doctype html><meta charset="utf-8"><title>Agen</title>
<p style="font-family:system-ui;margin:3rem">The web UI is not built into this binary. Run <code>scripts/build-web.sh</code> and rebuild agen.</p>`

// Built reports whether the UI assets are embedded.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// Handler serves the UI: static assets, and index.html for every other path
// (client-side routes). API and hook paths are mounted separately.
func Handler() http.Handler {
	sub, _ := fs.Sub(dist, "dist")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !Built() {
			h.Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(notBuilt))
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if _, err := fs.Stat(sub, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(p, "assets/") {
				http.NotFound(w, r) // a missing asset is not a page
				return
			}
		}
		h.Set("Cache-Control", "no-cache")
		b, _ := fs.ReadFile(sub, "index.html")
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
}
