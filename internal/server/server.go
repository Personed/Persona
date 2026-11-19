// Package server is the Persona HTTP service: the oRPC wire, files, MCP OAuth
// callbacks, the web UI, and in hosted mode the /hosting/* device sessions.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Personed/Persona/internal/rpc"
)

// maxUpload is MAX_FILE_BYTES plus room for the multipart envelope.
const maxUpload = 10<<20 + 1<<20

// File is a stored Bot file.
type File struct {
	Type string
	Data []byte
}

// Files reads stored Bot files. Any error is answered with 404.
type Files interface {
	Open(ctx context.Context, botID int64, id string) (File, error)
}

// OAuth completes managed MCP OAuth logins.
type OAuth interface {
	Complete(ctx context.Context, state, code string) error
}

// Config composes the service. Nil fields disable their routes.
type Config struct {
	RPC     *rpc.Server
	Static  fs.FS
	Files   Files
	MCP     OAuth
	Hosting *Hosting
}

// New returns the service handler: the local app, wrapped by the hosting layer
// when Hosting is set.
func New(cfg Config) (http.Handler, error) {
	var local http.Handler = &app{cfg: cfg}
	if cfg.Hosting != nil {
		return newHosted(*cfg.Hosting, local)
	}
	return recoverer(local), nil
}

type app struct{ cfg Config }

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if a.cfg.RPC != nil && strings.HasPrefix(path, "/rpc/") {
		if _, ok := a.cfg.RPC.Procedure(path); ok {
			if path == "/rpc/files/upload" && !limitBody(w, r, maxUpload) {
				return
			}
			a.cfg.RPC.ServeHTTP(w, r)
			return
		}
	}
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case get && path == "/health":
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case get && path == "/mcp/oauth/callback":
		a.oauth(w, r)
	case get && strings.HasPrefix(path, "/files/"):
		a.file(w, r)
	default:
		serveStatic(w, r, a.cfg.Static)
	}
}

func (a *app) oauth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if a.cfg.MCP == nil || state == "" || code == "" {
		writeText(w, http.StatusBadRequest, "授权未完成。请返回 Persona 重试。")
		return
	}
	if err := a.cfg.MCP.Complete(r.Context(), state, code); err != nil {
		writeText(w, http.StatusBadRequest, "授权失败或已取消。请返回 Persona 重试。")
		return
	}
	writeText(w, http.StatusOK, "授权已完成。请返回 Persona 刷新连接。")
}

func (a *app) file(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/files/"), "/")
	botID, ok := safeInteger(parts[0])
	if len(parts) != 2 || parts[1] == "" || !ok || a.cfg.Files == nil {
		notFound(w)
		return
	}
	file, err := a.cfg.Files.Open(r.Context(), botID, parts[1])
	if err != nil || !strings.HasPrefix(file.Type, "image/") {
		notFound(w)
		return
	}
	h := w.Header()
	h.Set("Content-Type", file.Type)
	h.Set("Content-Disposition", "inline")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	h.Set("Content-Length", strconv.Itoa(len(file.Data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(file.Data)
	}
}

// safeInteger parses a path segment as Number.isSafeInteger(Number(s)) accepts it.
func safeInteger(s string) (int64, bool) {
	s = rpc.Trim(s)
	if s == "" {
		return 0, true
	}
	if len(s) > 2 && s[0] == '0' && strings.ContainsRune("xXoObB", rune(s[1])) {
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[s[1]|0x20]
		n, err := strconv.ParseUint(s[2:], base, 64)
		if err != nil || strings.Contains(s, "_") || n > 1<<53-1 {
			return 0, false
		}
		return int64(n), true
	}
	if strings.Trim(s, "0123456789+-.eE") != "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53-1 {
		return 0, false
	}
	return int64(f), true
}

// limitBody answers 413 like Hono's bodyLimit when the body exceeds limit
// bytes. Like Hono, it reads a body of unknown length before the handler runs.
func limitBody(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	if r.ContentLength >= 0 && len(r.TransferEncoding) == 0 {
		if r.ContentLength > limit {
			writeText(w, http.StatusRequestEntityTooLarge, "Payload Too Large")
			return false
		}
		return true
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeText(w, http.StatusBadRequest, "Bad Request")
		return false
	}
	if int64(len(data)) > limit {
		writeText(w, http.StatusRequestEntityTooLarge, "Payload Too Large")
		return false
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(data)), int64(len(data))
	return true
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // re-panic the sentinel unchanged
					panic(v)
				}
				writeText(w, http.StatusInternalServerError, "服务暂时不可用。")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func notFound(w http.ResponseWriter) { writeText(w, http.StatusNotFound, "404 Not Found") }

func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		status, buf = http.StatusInternalServerError, *bytes.NewBufferString(`{"error":"服务暂时不可用。"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

var errInput = errors.New("invalid input")

func unixMilli(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
