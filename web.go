// WebUI 静态资源嵌入（web/ 目录）。go build 时通过 embed 打包进二进制。
package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFS embed.FS

// webHandler 服务 WebUI 静态资源；未命中路径回落到 index.html（SPA）。
func webHandler(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if f, err := sub.Open(path); err == nil {
		f.Close()
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
		return
	}
	// SPA 回落
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}
