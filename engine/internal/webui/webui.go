// Package webui serves the statically exported Next.js UI, embedded in the binary at build time.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the UI from dir (if non-empty) or from the embedded build.
// It returns nil when no UI build is present (API-only binary).
func Handler(dir string) http.Handler {
	var root fs.FS
	if dir != "" {
		root = os.DirFS(dir)
	} else {
		sub, err := fs.Sub(dist, "dist")
		if err != nil {
			return nil
		}
		root = sub
	}
	if _, err := fs.Stat(root, "index.html"); err != nil {
		return nil
	}
	exists := func(p string) bool {
		st, err := fs.Stat(root, strings.TrimPrefix(p, "/"))
		return err == nil && !st.IsDir()
	}
	// Pretty detail URLs from the spec redirect to the statically exported query-param pages.
	redirects := map[string]string{"/orders/": "/order/?id=", "/creator/": "/creator/?id="}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if strings.HasPrefix(p, "/_next/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		for prefix, target := range redirects {
			if id := strings.TrimPrefix(p, prefix); strings.HasPrefix(p, prefix) && id != "" && !strings.Contains(id, "/") && !strings.Contains(id, ".") {
				http.Redirect(w, r, target+url.QueryEscape(id), http.StatusFound)
				return
			}
		}
		candidates := []string{p, strings.TrimSuffix(p, "/") + "/index.html", p + ".html"}
		if p == "/" {
			candidates = []string{"/index.html"}
		}
		for _, c := range candidates {
			if exists(c) {
				http.ServeFileFS(w, r, root, strings.TrimPrefix(c, "/"))
				return
			}
		}
		if exists("/404.html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			b, _ := fs.ReadFile(root, "404.html")
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	})
}
