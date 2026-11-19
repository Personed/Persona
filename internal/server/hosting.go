package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/Personed/Persona/internal/rpc"
)

// Device is a paired client device session.
type Device struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Scope   string `json:"scope"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
}

// Pairing is a one-time pairing code.
type Pairing struct {
	Code    string `json:"code"`
	Expires int64  `json:"expires"`
}

// Sessions stores device sessions, as packages/hosting/src/sessions.ts does.
type Sessions interface {
	List() ([]Device, error)
	Pairing(scope string) (Pairing, error)
	// Exchange trades a pairing code for a session token; ok is false for an invalid or expired code.
	Exchange(code, label string) (token string, device Device, ok bool, err error)
	Authenticate(token string) (Device, bool)
	// Revoke logs a device out, refusing the last admin device.
	Revoke(id string) error
	Logout(id string)
	// Track returns a context that ends when the device logs out; release ends the tracking.
	Track(device Device) (ctx context.Context, release func())
	// Bootstrap writes a new owner pairing code when no admin device is left.
	Bootstrap() error
	RemoveBootstrap() error
}

// Logins runs model provider logins for admin devices, as apps/api/src/hosting/login.ts does.
type Logins interface {
	Models(ctx context.Context) (any, error)
	RemoveProvider(ctx context.Context, provider string) error
	Start(owner, provider, authType string) (any, error)
	Status(id, owner string) (any, error)
	// Answer answers the flow's current prompt; stale is true when promptID is no longer current.
	Answer(id, owner, promptID, value string) (stale bool, err error)
	CancelFlow(id, owner string) error
	Cancel(owner string)
}

// Hosting configures hosted mode: device authentication in front of the local app.
type Hosting struct {
	PublicURL             string
	Sessions              Sessions
	Logins                Logins
	DesktopCredentialFile string
}

// AdminProcedures are the adminProcedures in contracts/api/http.json.
var AdminProcedures = []string{
	"mcp.*",
	"models.setVisibility",
	"computer.manage", "computer.add", "computer.authorize", "computer.default", "computer.configure",
	"computer.prepare", "computer.service", "computer.browserMode", "computer.approval",
	"computer.assistantConfigure", "computer.assistantKey", "computer.assistantTest",
	"engines.configure", "engines.inspect", "engines.install", "engines.login",
	"engines.loginState", "engines.answerLogin", "engines.cancelLogin",
}

const (
	cookieName   = "persona_session"
	hostingLimit = 64 << 10
)

type hosted struct {
	cfg    Hosting
	local  http.Handler
	origin string
	host   string
	secure bool
	public *http.ServeMux
	device *http.ServeMux
	admin  *http.ServeMux
}

type deviceKey struct{}

func newHosted(cfg Hosting, local http.Handler) (http.Handler, error) {
	if cfg.Sessions == nil {
		return nil, errors.New("server: hosting needs Sessions")
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("server: invalid public URL %q", cfg.PublicURL)
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if (scheme == "http" && strings.HasSuffix(host, ":80")) || (scheme == "https" && strings.HasSuffix(host, ":443")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	h := &hosted{cfg: cfg, local: local, origin: scheme + "://" + host, host: host, secure: scheme == "https",
		public: http.NewServeMux(), device: http.NewServeMux(), admin: http.NewServeMux()}
	h.public.Handle("POST /hosting/exchange", h.handle(h.exchange))
	h.public.Handle("POST /hosting/desktop", h.handle(h.desktop))
	h.device.Handle("GET /hosting/session", h.handle(func(_ http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]Device{"device": device(r)}, nil
	}))
	h.device.Handle("POST /hosting/logout", h.handle(h.logout))
	h.admin.Handle("GET /hosting/devices", h.handle(func(http.ResponseWriter, *http.Request) (any, error) {
		return cfg.Sessions.List()
	}))
	h.admin.Handle("POST /hosting/pairing", h.handle(h.pairing))
	h.admin.Handle("DELETE /hosting/devices/{id}", h.handle(h.revoke))
	h.admin.Handle("GET /hosting/models", h.handle(h.withLogins(func(l Logins, r *http.Request) (any, error) {
		return l.Models(r.Context())
	})))
	h.admin.Handle("DELETE /hosting/models/{provider}", h.handle(h.withLogins(func(l Logins, r *http.Request) (any, error) {
		return ok(l.RemoveProvider(r.Context(), r.PathValue("provider")))
	})))
	h.admin.Handle("POST /hosting/login", h.handle(h.withLogins(h.startLogin)))
	h.admin.Handle("GET /hosting/login/{id}", h.handle(h.withLogins(func(l Logins, r *http.Request) (any, error) {
		return l.Status(r.PathValue("id"), device(r).ID)
	})))
	h.admin.Handle("POST /hosting/login/{id}", h.handle(h.withLogins(h.answerLogin)))
	h.admin.Handle("DELETE /hosting/login/{id}", h.handle(h.withLogins(func(l Logins, r *http.Request) (any, error) {
		return ok(l.CancelFlow(r.PathValue("id"), device(r).ID))
	})))
	h.admin.Handle("DELETE /hosting/login", h.handle(h.withLogins(func(l Logins, r *http.Request) (any, error) {
		l.Cancel(device(r).ID)
		return ok(nil)
	})))
	return recoverer(h), nil
}

func (h *hosted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/health" {
		h.local.ServeHTTP(w, r)
		return
	}
	if message := h.checkOrigin(r); message != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": message})
		return
	}
	switch {
	case path == "/hosting/info" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"product": "persona", "version": "1"})
	case path == "/hosting" || strings.HasPrefix(path, "/hosting/"):
		h.hosting(w, r)
	case path == "/rpc" || strings.HasPrefix(path, "/rpc/") || path == "/files" || strings.HasPrefix(path, "/files/"):
		h.protected(w, r)
	default:
		h.local.ServeHTTP(w, r)
	}
}

// checkOrigin mirrors originGuard in apps/api/src/hosting/security.ts.
func (h *hosted) checkOrigin(r *http.Request) string {
	if r.Host != "" && r.Host != h.host {
		return "服务器地址不匹配。"
	}
	supplied := r.Header.Get("Origin")
	if supplied != "" && supplied != h.origin {
		return "请求来源不匹配。"
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return "请从 Persona 页面操作。"
	}
	safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
	if !safe && supplied == "" && r.Header.Get("Authorization") == "" {
		return "需要同源请求或设备凭据。"
	}
	return ""
}

func (h *hosted) hosting(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !limitBody(w, r, hostingLimit) {
		return
	}
	if serve(h.public, w, r) {
		return
	}
	r, ok := h.authenticate(w, r)
	if !ok || serve(h.device, w, r) {
		return
	}
	if device(r).Scope != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "需要管理员权限。"})
		return
	}
	if !serve(h.admin, w, r) {
		h.local.ServeHTTP(w, r)
	}
}

func (h *hosted) protected(w http.ResponseWriter, r *http.Request) {
	r, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	dev := device(r)
	if procedure, found := strings.CutPrefix(r.URL.Path, "/rpc/"); found && adminOnly(procedure) && dev.Scope != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "需要管理员权限。"})
		return
	}
	tracked, release := h.cfg.Sessions.Track(dev)
	defer release()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(tracked, cancel)
	defer stop()
	if strings.HasPrefix(r.URL.Path, "/files/") {
		w = &headerWriter{ResponseWriter: w, header: "Cache-Control", value: "private, no-store"}
	}
	h.local.ServeHTTP(w, r.WithContext(ctx))
}

func adminOnly(procedure string) bool {
	procedure = strings.ReplaceAll(procedure, "/", ".")
	for _, admin := range AdminProcedures {
		if prefix, wildcard := strings.CutSuffix(admin, "*"); wildcard && strings.HasPrefix(procedure, prefix) || procedure == admin {
			return true
		}
	}
	return false
}

func (h *hosted) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !bearer || token == "" {
		token = ""
		if c, err := r.Cookie(cookieName); err == nil {
			token = c.Value
		}
	}
	dev, ok := h.cfg.Sessions.Authenticate(token)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Dispositivo não autorizado."})
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), deviceKey{}, dev)), true
}

func device(r *http.Request) Device {
	dev, _ := r.Context().Value(deviceKey{}).(Device)
	return dev
}

func serve(mux *http.ServeMux, w http.ResponseWriter, r *http.Request) bool {
	if _, pattern := mux.Handler(r); pattern == "" {
		return false
	}
	mux.ServeHTTP(w, r)
	return true
}

type result struct {
	status int
	body   any
}

// handle answers a hosting route's value as JSON and its error as the 400
// {error} body of the onError handler in apps/api/src/hosting/routes.ts.
func (h *hosted) handle(f func(http.ResponseWriter, *http.Request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, err := f(w, r)
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeText(w, http.StatusRequestEntityTooLarge, "Payload Too Large")
		case errors.Is(err, errInput):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "输入格式无效。"})
		case err != nil:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "操作未完成。请检查设置后重试。"})
		default:
			if res, isResult := value.(result); isResult {
				writeJSON(w, res.status, res.body)
			} else {
				writeJSON(w, http.StatusOK, value)
			}
		}
	})
}

func (h *hosted) withLogins(f func(Logins, *http.Request) (any, error)) func(http.ResponseWriter, *http.Request) (any, error) {
	return func(_ http.ResponseWriter, r *http.Request) (any, error) {
		if h.cfg.Logins == nil {
			return nil, errors.New("server: model logins are not configured")
		}
		return f(h.cfg.Logins, r)
	}
}

func ok(err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

func (h *hosted) setCookie(w http.ResponseWriter, token string, expires int64) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", Expires: unixMilli(expires),
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteStrictMode})
}

func (h *hosted) exchange(w http.ResponseWriter, r *http.Request) (any, error) {
	input, err := readObject(r)
	if err != nil {
		return nil, err
	}
	code, err := stringField(input, "code", 1, 80, false)
	if err != nil {
		return nil, err
	}
	label, err := stringField(input, "label", 1, 80, true)
	if err != nil {
		return nil, err
	}
	token, dev, valid, err := h.cfg.Sessions.Exchange(code, label)
	if err != nil {
		return nil, err
	}
	if !valid {
		return result{http.StatusUnauthorized, map[string]string{"error": "配对码无效或已过期。"}}, nil
	}
	h.setCookie(w, token, dev.Expires)
	if dev.Scope == "admin" {
		if err := h.cfg.Sessions.RemoveBootstrap(); err != nil {
			return nil, err
		}
	}
	return map[string]Device{"device": dev}, nil
}

func (h *hosted) desktop(_ http.ResponseWriter, r *http.Request) (any, error) {
	if h.cfg.DesktopCredentialFile == "" {
		return result{http.StatusNotFound, struct{}{}}, nil
	}
	data, err := os.ReadFile(h.cfg.DesktopCredentialFile)
	if err != nil {
		return nil, err
	}
	token := rpc.Trim(string(data))
	got, want := sha256.Sum256([]byte(r.Header.Get("Authorization"))), sha256.Sum256([]byte("Bearer "+token))
	if len(utf16.Encode([]rune(token))) < 32 || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return result{http.StatusUnauthorized, struct{}{}}, nil
	}
	pairing, err := h.cfg.Sessions.Pairing("admin")
	if err != nil {
		return nil, err
	}
	session, dev, valid, err := h.cfg.Sessions.Exchange(pairing.Code, "Persona 桌面")
	if err == nil && !valid {
		err = errors.New("桌面连接未完成。")
	}
	if err != nil {
		return nil, err
	}
	if err := h.cfg.Sessions.RemoveBootstrap(); err != nil {
		return nil, err
	}
	return map[string]any{"token": session, "expires": dev.Expires}, nil
}

func (h *hosted) logout(w http.ResponseWriter, r *http.Request) (any, error) {
	id := device(r).ID
	if h.cfg.Logins != nil {
		h.cfg.Logins.Cancel(id)
	}
	h.cfg.Sessions.Logout(id)
	if err := h.cfg.Sessions.Bootstrap(); err != nil {
		return nil, err
	}
	h.setCookie(w, "", 0)
	return ok(nil)
}

func (h *hosted) pairing(_ http.ResponseWriter, r *http.Request) (any, error) {
	input, err := readObject(r)
	if err != nil {
		return nil, err
	}
	scope, err := enumField(input, "scope", "admin", "client")
	if err != nil {
		return nil, err
	}
	return h.cfg.Sessions.Pairing(scope)
}

func (h *hosted) revoke(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	if err := h.cfg.Sessions.Revoke(id); err != nil {
		return nil, err
	}
	if h.cfg.Logins != nil {
		h.cfg.Logins.Cancel(id)
	}
	return ok(nil)
}

func (h *hosted) startLogin(l Logins, r *http.Request) (any, error) {
	input, err := readObject(r)
	if err != nil {
		return nil, err
	}
	provider, err := stringField(input, "provider", 0, -1, false)
	if err != nil {
		return nil, err
	}
	authType, err := enumField(input, "type", "api_key", "oauth")
	if err != nil {
		return nil, err
	}
	return l.Start(device(r).ID, provider, authType)
}

// uuidPattern is Zod 4's z.uuid().
var uuidPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)

func (h *hosted) answerLogin(l Logins, r *http.Request) (any, error) {
	input, err := readObject(r)
	if err != nil {
		return nil, err
	}
	promptID, err := stringField(input, "promptId", 0, -1, false)
	if err != nil {
		return nil, err
	}
	value, err := stringField(input, "value", 0, 32000, false)
	if err != nil {
		return nil, err
	}
	if !uuidPattern.MatchString(promptID) {
		return nil, errInput
	}
	stale, err := l.Answer(r.PathValue("id"), device(r).ID, promptID, value)
	if err != nil {
		return nil, err
	}
	if stale {
		return result{http.StatusConflict, map[string]string{"error": "输入请求已更新。"}}, nil
	}
	return ok(nil)
}

// readObject parses a JSON body; a syntax error is an ordinary failure and a
// non-object a validation failure, as with c.req.json() and z.object.
func readObject(r *http.Request) (map[string]any, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	object, isObject := value.(map[string]any)
	if !isObject {
		return nil, errInput
	}
	return object, nil
}

// stringField reads a string with Zod's UTF-16 length bounds; maxLen < 0 means unbounded.
func stringField(input map[string]any, key string, minLen, maxLen int, trim bool) (string, error) {
	s, isString := input[key].(string)
	if !isString {
		return "", errInput
	}
	if trim {
		s = rpc.Trim(s)
	}
	n := len(utf16.Encode([]rune(s)))
	if n < minLen || maxLen >= 0 && n > maxLen {
		return "", errInput
	}
	return s, nil
}

func enumField(input map[string]any, key string, values ...string) (string, error) {
	s, _ := input[key].(string)
	for _, v := range values {
		if s == v {
			return s, nil
		}
	}
	return "", errInput
}

// headerWriter sets one header just before the response starts.
type headerWriter struct {
	http.ResponseWriter
	header, value string
	wrote         bool
}

func (w *headerWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		w.Header().Set(w.header, w.value)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *headerWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *headerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
