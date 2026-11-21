package edge

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spa serves index.html for extensionless paths that are not files, so GUI routes like /vms survive a reload.
func spa(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if _, err := fs.Stat(dist, strings.TrimPrefix(p, "/")); err != nil && path.Ext(p) == "" {
			http.ServeFileFS(w, r, dist, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}
