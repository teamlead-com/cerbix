// Package web serves the embedded single-page application. The Vue build output
// is copied into dist/ at build time and embedded via embed.FS, so the whole UI
// ships inside the cerbix binary. Requests for real assets are served from the
// embedded FS; unknown paths fall back to index.html for client-side routing.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// New builds the SPA handler over the embedded dist/ directory.
func New() (http.Handler, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" {
			if st, err := fs.Stat(sub, p); err == nil && !st.IsDir() {
				// Vite emits content-hashed files under assets/ — they never change
				// for a given name, so cache them hard. Other root files (favicon…)
				// revalidate so a redeploy is picked up.
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				fileServer.ServeHTTP(w, r) // real asset (js/css/img/…)
				return
			}
		}
		// SPA fallback: hand index.html to the client router. Never cache it — it
		// references the current content-hashed chunks, so a stale copy would keep
		// loading old JS after a redeploy.
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	}), nil
}
