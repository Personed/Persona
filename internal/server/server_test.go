package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Personed/Persona/internal/contract"
	"github.com/Personed/Persona/internal/rpc"
)

type fakeSessions struct {
	mu        sync.Mutex
	devices   map[string]Device // by token
	tracked   map[string][]context.CancelFunc
	bootstrap int
	removed   int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{
		devices: map[string]Device{
			"admin-token":  {ID: "a", Label: "Admin", Scope: "admin", Expires: 2e12},
			"client-token": {ID: "c", Label: "Phone", Scope: "client", Expires: 2e12},
		},
		tracked: map[string][]context.CancelFunc{},
	}
}

func (s *fakeSessions) List() ([]Device, error) {
	var list []Device
	for _, d := range s.devices {
		list = append(list, d)
	}
	slices.SortFunc(list, func(a, b Device) int { return strings.Compare(a.ID, b.ID) })
	return list, nil
}
func (s *fakeSessions) Pairing(scope string) (Pairing, error) {
	return Pairing{Code: "code-" + scope, Expires: 1e12}, nil
}
func (s *fakeSessions) Exchange(code, label string) (string, Device, bool, error) {
	scope, found := strings.CutPrefix(code, "code-")
	if !found {
		return "", Device{}, false, nil
	}
	d := Device{ID: "new", Label: label, Scope: scope, Expires: 1e12}
	s.devices["new-token"] = d
	return "new-token", d, true, nil
}
func (s *fakeSessions) Authenticate(token string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[token]
	return d, ok
}
func (s *fakeSessions) Revoke(id string) error {
	if id == "a" {
		return errors.New("请保留至少一台管理员设备。")
	}
	s.Logout(id)
	return nil
}
func (s *fakeSessions) Logout(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, d := range s.devices {
		if d.ID == id {
			delete(s.devices, token)
		}
	}
	for _, cancel := range s.tracked[id] {
		cancel()
	}
}
func (s *fakeSessions) Track(d Device) (context.Context, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	s.tracked[d.ID] = append(s.tracked[d.ID], cancel)
	return ctx, cancel
}
func (s *fakeSessions) Bootstrap() error       { s.bootstrap++; return nil }
func (s *fakeSessions) RemoveBootstrap() error { s.removed++; return nil }

type fakeFiles map[string]File

func (f fakeFiles) Open(_ context.Context, botID int64, id string) (File, error) {
	if file, ok := f[id]; ok && botID == 1 {
		return file, nil
	}
	return File{}, errors.New("missing")
}

type fakeOAuth struct{}

func (fakeOAuth) Complete(_ context.Context, state, _ string) error {
	if state != "good" {
		return errors.New("bad state")
	}
	return nil
}

func newRPC(t *testing.T) *rpc.Server {
	t.Helper()
	c, err := contract.Load(filepath.Join("..", "..", "contracts", "api"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := rpc.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newLocal(t *testing.T) (http.Handler, *rpc.Server) {
	t.Helper()
	procedures := newRPC(t)
	h, err := New(Config{
		RPC:    procedures,
		Static: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}, "assets/app.js": {Data: []byte("app")}},
		Files:  fakeFiles{"img": {Type: "image/png", Data: []byte("png")}, "doc": {Type: "text/plain", Data: []byte("x")}},
		MCP:    fakeOAuth{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, procedures
}

func newHostedApp(t *testing.T, sessions Sessions) (http.Handler, *rpc.Server) {
	t.Helper()
	local, procedures := newLocal(t)
	credential := filepath.Join(t.TempDir(), "desktop")
	if err := os.WriteFile(credential, []byte(strings.Repeat("k", 40)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := newHosted(Hosting{PublicURL: "https://persona.example:443", Sessions: sessions, DesktopCredentialFile: credential}, local)
	if err != nil {
		t.Fatal(err)
	}
	return h, procedures
}

type req struct {
	method, path, body string
	header             map[string]string
}

func do(h http.Handler, q req) *httptest.ResponseRecorder {
	r := httptest.NewRequest(q.method, "https://persona.example"+q.path, strings.NewReader(q.body))
	for k, v := range q.header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var (
	sameOrigin = map[string]string{"Origin": "https://persona.example"}
	admin      = map[string]string{"Authorization": "Bearer admin-token"}
	client     = map[string]string{"Authorization": "Bearer client-token"}
)

func check(t *testing.T, w *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if w.Code != status || strings.TrimSpace(w.Body.String()) != body {
		t.Errorf("got %d %s, want %d %s", w.Code, w.Body, status, body)
	}
}

func TestLocal(t *testing.T) {
	h, procedures := newLocal(t)
	procedures.Handle("bots.list", func(context.Context, json.RawMessage) (any, error) { return []int{}, nil })
	check(t, do(h, req{method: "GET", path: "/health"}), 200, `{"ok":true}`)
	check(t, do(h, req{method: "POST", path: "/rpc/bots/list", body: "{}"}), 200, `{"json":[]}`)
	check(t, do(h, req{method: "GET", path: "/"}), 200, "<!doctype html>")
	check(t, do(h, req{method: "GET", path: "/assets/app.js"}), 200, "app")
	check(t, do(h, req{method: "GET", path: "/rpc/nope"}), 404, "404 Not Found")
	check(t, do(h, req{method: "GET", path: "/missing"}), 404, "404 Not Found")

	w := do(h, req{method: "GET", path: "/files/1/img"})
	check(t, w, 200, "png")
	if w.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("file headers %v", w.Header())
	}
	for _, path := range []string{"/files/1/doc", "/files/2/img", "/files/x/img", "/files/1.5/img", "/files/+0x1/img", "/files/0x_1/img"} {
		check(t, do(h, req{method: "GET", path: path}), 404, "404 Not Found")
	}

	check(t, do(h, req{method: "GET", path: "/mcp/oauth/callback?state=good&code=c"}), 200, "授权已完成。请返回 Persona 刷新连接。")
	check(t, do(h, req{method: "GET", path: "/mcp/oauth/callback?state=bad&code=c"}), 400, "授权失败或已取消。请返回 Persona 重试。")
	check(t, do(h, req{method: "GET", path: "/mcp/oauth/callback"}), 400, "授权未完成。请返回 Persona 重试。")

	if w := do(h, req{method: "GET", path: "/files/0x1/img"}); w.Code != 200 {
		t.Errorf("hex bot id: %d", w.Code)
	}

	big := do(h, req{method: "POST", path: "/rpc/files/upload", body: strings.Repeat("x", maxUpload+1)})
	check(t, big, 413, "Payload Too Large")
	check(t, chunked(h, "/rpc/files/upload", maxUpload+1), 413, "Payload Too Large")
}

// chunked posts size bytes with no Content-Length.
func chunked(h http.Handler, path string, size int) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://persona.example"+path, strings.NewReader(strings.Repeat("x", size)))
	r.ContentLength, r.TransferEncoding = -1, []string{"chunked"}
	r.Header.Set("Origin", "https://persona.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOrigin(t *testing.T) {
	h, _ := newHostedApp(t, newFakeSessions())
	check(t, do(h, req{method: "GET", path: "/health", header: map[string]string{"Origin": "https://evil.example"}}), 200, `{"ok":true}`)
	check(t, do(h, req{method: "GET", path: "/hosting/info", header: map[string]string{"Origin": "https://evil.example"}}), 403, `{"error":"请求来源不匹配。"}`)
	check(t, do(h, req{method: "GET", path: "/", header: map[string]string{"Sec-Fetch-Site": "cross-site"}}), 403, `{"error":"请从 Persona 页面操作。"}`)
	check(t, do(h, req{method: "POST", path: "/hosting/exchange", body: "{}"}), 403, `{"error":"需要同源请求或设备凭据。"}`)
	r := httptest.NewRequest("GET", "https://other.example/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	check(t, w, 403, `{"error":"服务器地址不匹配。"}`)
	info := do(h, req{method: "GET", path: "/hosting/info"})
	check(t, info, 200, `{"product":"persona","version":"1"}`)
	if head := do(h, req{method: "HEAD", path: "/hosting/info"}); head.Code != 200 {
		t.Errorf("HEAD /hosting/info: %d", head.Code)
	}
	check(t, chunked(h, "/hosting/logout", hostingLimit+1), 413, "Payload Too Large")
}

func TestHostingSessions(t *testing.T) {
	sessions := newFakeSessions()
	h, _ := newHostedApp(t, sessions)

	w := do(h, req{method: "POST", path: "/hosting/exchange", body: `{"code":"code-admin","label":"  Laptop "}`, header: sameOrigin})
	check(t, w, 200, `{"device":{"id":"new","label":"Laptop","scope":"admin","created":0,"expires":1000000000000}}`)
	if c := w.Header().Get("Set-Cookie"); !strings.Contains(c, "persona_session=new-token") || !strings.Contains(c, "HttpOnly") ||
		!strings.Contains(c, "Secure") || !strings.Contains(c, "SameSite=Strict") || !strings.Contains(c, "Path=/") {
		t.Errorf("cookie %q", c)
	}
	if w.Header().Get("Cache-Control") != "no-store" || sessions.removed != 1 {
		t.Errorf("no-store %q, bootstrap removed %d", w.Header().Get("Cache-Control"), sessions.removed)
	}
	check(t, do(h, req{method: "POST", path: "/hosting/exchange", body: `{"code":"wrong","label":"x"}`, header: sameOrigin}), 401, `{"error":"配对码无效或已过期。"}`)
	check(t, do(h, req{method: "POST", path: "/hosting/exchange", body: `{"code":"","label":"x"}`, header: sameOrigin}), 400, `{"error":"输入格式无效。"}`)
	check(t, do(h, req{method: "POST", path: "/hosting/exchange", body: `{`, header: sameOrigin}), 400, `{"error":"操作未完成。请检查设置后重试。"}`)
	check(t, do(h, req{method: "POST", path: "/hosting/exchange", body: strings.Repeat(" ", hostingLimit+1), header: sameOrigin}), 413, "Payload Too Large")

	cookie := map[string]string{"Cookie": "persona_session=client-token"}
	check(t, do(h, req{method: "GET", path: "/hosting/session", header: cookie}), 200, `{"device":{"id":"c","label":"Phone","scope":"client","created":0,"expires":2000000000000}}`)
	check(t, do(h, req{method: "GET", path: "/hosting/session"}), 401, `{"error":"Dispositivo não autorizado."}`)
	check(t, do(h, req{method: "GET", path: "/hosting/devices", header: client}), 403, `{"error":"需要管理员权限。"}`)
	check(t, do(h, req{method: "POST", path: "/hosting/pairing", body: `{"scope":"client"}`, header: admin}), 200, `{"code":"code-client","expires":1000000000000}`)
	check(t, do(h, req{method: "POST", path: "/hosting/pairing", body: `{"scope":"root"}`, header: admin}), 400, `{"error":"输入格式无效。"}`)
	check(t, do(h, req{method: "DELETE", path: "/hosting/devices/a", header: admin}), 400, `{"error":"操作未完成。请检查设置后重试。"}`)
	check(t, do(h, req{method: "GET", path: "/hosting/models", header: admin}), 400, `{"error":"操作未完成。请检查设置后重试。"}`)

	w = do(h, req{method: "POST", path: "/hosting/logout", header: client})
	check(t, w, 200, `{"ok":true}`)
	if c := w.Header().Get("Set-Cookie"); !strings.Contains(c, "persona_session=;") || !strings.Contains(c, "Expires=Thu, 01 Jan 1970") || sessions.bootstrap != 1 {
		t.Errorf("logout cookie %q, bootstrap %d", c, sessions.bootstrap)
	}
	check(t, do(h, req{method: "GET", path: "/hosting/session", header: client}), 401, `{"error":"Dispositivo não autorizado."}`)
}

func TestDesktop(t *testing.T) {
	h, _ := newHostedApp(t, newFakeSessions())
	check(t, do(h, req{method: "POST", path: "/hosting/desktop", header: map[string]string{"Authorization": "Bearer wrong"}}), 401, `{}`)
	w := do(h, req{method: "POST", path: "/hosting/desktop", header: map[string]string{"Authorization": "Bearer " + strings.Repeat("k", 40)}})
	check(t, w, 200, `{"expires":1000000000000,"token":"new-token"}`)
}

func TestProtected(t *testing.T) {
	sessions := newFakeSessions()
	h, procedures := newHostedApp(t, sessions)
	procedures.Handle("bots.list", func(context.Context, json.RawMessage) (any, error) { return []int{}, nil })
	procedures.Handle("mcp.list", func(context.Context, json.RawMessage) (any, error) { return []int{}, nil })
	check(t, do(h, req{method: "POST", path: "/rpc/bots/list", body: "{}", header: sameOrigin}), 401, `{"error":"Dispositivo não autorizado."}`)
	check(t, do(h, req{method: "POST", path: "/rpc/bots/list", body: "{}", header: client}), 200, `{"json":[]}`)
	check(t, do(h, req{method: "POST", path: "/rpc/mcp/list", body: "{}", header: client}), 403, `{"error":"需要管理员权限。"}`)
	check(t, do(h, req{method: "POST", path: "/rpc/mcp/list", body: "{}", header: admin}), 200, `{"json":[]}`)
	w := do(h, req{method: "GET", path: "/files/1/img", header: client})
	if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("hosted file: %d %v", w.Code, w.Header())
	}

	started := make(chan struct{})
	procedures.HandleStream("subscribe", func(ctx context.Context, _ json.RawMessage, _ func(any) error) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := make(chan struct{})
	go func() {
		do(h, req{method: "POST", path: "/rpc/subscribe", body: `{"json":{"botId":1}}`, header: client})
		close(done)
	}()
	wait(t, started, "stream start")
	sessions.Logout("c")
	wait(t, done, "stream cancellation")
}

func wait(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestAdminProcedures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "api", "http.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		AdminProcedures []string `json:"adminProcedures"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(doc.AdminProcedures, AdminProcedures) {
		t.Errorf("AdminProcedures differ from http.json: %v", doc.AdminProcedures)
	}
	c, err := contract.Load(filepath.Join("..", "..", "contracts", "api"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range AdminProcedures {
		if _, ok := c.Procedure(strings.TrimSuffix(name, ".*") + "."); !ok && !strings.HasSuffix(name, "*") {
			if _, ok := c.Procedure(name); !ok {
				t.Errorf("admin procedure %s is not in the contract", name)
			}
		}
	}
}
