package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// staticVersion changes whenever an asset does, so the layout's links can
// be cached for good.
var staticVersion = func() string {
	h := sha256.New()
	_ = fs.WalkDir(static, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := static.ReadFile(path)
		h.Write([]byte(path))
		h.Write(b)
		return err
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

func assetURL(name string) string {
	return "/static/" + name + "?v=" + staticVersion
}

// FaviconHandler answers /favicon.ico, which browsers ask for on their own
// whatever the page links, with the SVG icon rather than a 404.
func FaviconHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := static.ReadFile("static/favicon.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	})
}

// StaticHandler serves the UI's scripts and styles under /static/. A
// versioned link never changes content, so it is cached for a year.
func StaticHandler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == staticVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
