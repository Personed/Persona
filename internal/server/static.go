package server

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// serveStatic serves the web build like @hono/node-server's serveStatic:
// files by path, index.html for directories, 404 otherwise.
func serveStatic(w http.ResponseWriter, r *http.Request, root fs.FS) {
	if root == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		notFound(w)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || strings.HasSuffix(r.URL.Path, "/") {
		name = path.Join(name, "index.html")
	}
	data, err := fs.ReadFile(root, name)
	if err != nil {
		if data, err = fs.ReadFile(root, path.Join(name, "index.html")); err != nil {
			notFound(w)
			return
		}
		name = path.Join(name, "index.html")
	}
	modified := time.Time{}
	if info, err := fs.Stat(root, name); err == nil {
		modified = info.ModTime()
	}
	http.ServeContent(w, r, name, modified, io.ReadSeeker(bytes.NewReader(data)))
}
