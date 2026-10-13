// Package webui embeds and serves the browser UI.
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed all:static
var files embed.FS

type asset struct {
	data  []byte
	etag  string
	ctype string
}

// Handler serves the UI. "/" and "/viewer" return index.html; assets are
// revalidated on every load (ETag) so upgrades never serve stale code.
func Handler() http.Handler {
	assets := map[string]*asset{}
	root, _ := fs.Sub(files, "static")
	_ = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(root, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		ct := mime.TypeByExtension(path.Ext(p))
		switch path.Ext(p) {
		case ".js", ".mjs":
			ct = "text/javascript; charset=utf-8"
		case ".css":
			ct = "text/css; charset=utf-8"
		case ".html":
			ct = "text/html; charset=utf-8"
		case ".svg":
			ct = "image/svg+xml"
		case ".webmanifest", ".json":
			ct = "application/json"
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		assets["/"+p] = &asset{data: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: ct}
		return nil
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := r.URL.Path
		if p == "/" || p == "/viewer" || p == "/viewer/" || p == "/index.html" {
			p = "/index.html"
		}
		if p == "/favicon.ico" {
			p = "/assets/logo.png"
		}
		a, ok := assets[p]
		if !ok || strings.Contains(p, "..") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", a.ctype)
		w.Header().Set("ETag", a.etag)
		w.Header().Set("Cache-Control", "no-cache")
		if r.Header.Get("If-None-Match") == a.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(a.data)
	})
}
