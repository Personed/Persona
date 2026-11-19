package tui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
)

// pairingServer accepts code 1234, then requires the issued token on bots.list.
func pairingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hosting/exchange":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"code":"1234"`) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"bad code"}`)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "persona_session", Value: "tok"})
			_, _ = io.WriteString(w, `{"device":{"id":"d","label":"x","scope":"admin"}}`)
		case "/hosting/logout":
			w.WriteHeader(http.StatusNoContent)
		case "/rpc/bots/list":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"Dispositivo não autorizado."}`)
				return
			}
			_, _ = io.WriteString(w, `{"json":[{"id":1,"name":"Ada"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPairingLogin(t *testing.T) {
	server := pairingServer(t)
	path := filepath.Join(t.TempDir(), "persona", "tui.json")
	saved, err := loadServers(path)
	if err != nil {
		t.Fatal(err)
	}
	m := New(t.Context(), &client.Client{BaseURL: server.URL})
	m.servers = saved
	next := drive(t, m, m.Init())
	if got := content(next); !strings.Contains(got, "Pair this terminal") {
		t.Fatalf("no login screen:\n%s", got)
	}
	next = press(t, typeText(next, "999"), key(tea.KeyEnter, ""))
	if got := content(next); !strings.Contains(got, "bad code") {
		t.Fatalf("bad code not shown:\n%s", got)
	}
	for range 3 {
		next, _ = next.Update(key(tea.KeyBackspace, ""))
	}
	next = press(t, typeText(next, "1234"), key(tea.KeyEnter, ""))
	if got := content(next); !strings.Contains(got, "> Ada") {
		t.Fatalf("Bots not shown after pairing:\n%s", got)
	}
	reread, err := loadServers(path)
	if err != nil || reread.Current != server.URL || reread.Tokens[server.URL] != "tok" {
		t.Fatalf("saved %+v, %v", reread, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("servers file mode %v, %v", info.Mode(), err)
	}

	next = press(t, next, key('L', "L"))
	if got := content(next); !strings.Contains(got, "Pair this terminal") {
		t.Fatalf("logout did not return to login:\n%s", got)
	}
	if reread, _ := loadServers(path); reread.Tokens[server.URL] != "" {
		t.Fatalf("token kept after logout: %+v", reread)
	}
}

func TestPairingRefusesPlainHTTP(t *testing.T) {
	m := New(t.Context(), &client.Client{BaseURL: "http://persona.example.com"})
	next, _ := m.openLogin()
	next = press(t, typeText(next, "1234"), key(tea.KeyEnter, ""))
	if got := content(next); !strings.Contains(got, "plain HTTP") {
		t.Fatalf("insecure pairing not refused:\n%s", got)
	}
}

func TestPairingCodePaste(t *testing.T) {
	m := New(t.Context(), &client.Client{BaseURL: "http://127.0.0.1:1"})
	next, _ := m.openLogin()
	next, _ = next.Update(tea.PasteMsg{Content: "1234"})
	if got := next.(Model).login.code.Value(); got != "1234" {
		t.Fatalf("pasted code = %q", got)
	}
}

func TestUnsavedPairingStaysVisible(t *testing.T) {
	server := pairingServer(t)
	dir := t.TempDir()
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(t.Context(), &client.Client{BaseURL: server.URL})
	m.servers = &servers{path: filepath.Join(blocked, "tui.json"), Tokens: map[string]string{}}
	next := drive(t, m, m.Init())
	next = press(t, typeText(next, "1234"), key(tea.KeyEnter, ""))
	if got := content(next); !strings.Contains(got, "> Ada") || !strings.Contains(got, "pairing not saved") {
		t.Fatalf("unsaved pairing hidden:\n%s", got)
	}
	model := next.(Model)
	model.servers.path = filepath.Join(dir, "tui.json")
	next = press(t, model, key('r', "r"))
	if got := content(next); strings.Contains(got, "pairing not saved") {
		t.Fatalf("retry did not save:\n%s", got)
	}
	if saved, _ := loadServers(filepath.Join(dir, "tui.json")); saved.Tokens[server.URL] != "tok" {
		t.Fatalf("saved %+v", saved)
	}
}

func TestRememberKeepsOtherTerminals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui.json")
	a, _ := loadServers(path)
	b, _ := loadServers(path)
	if err := a.remember("https://a.example", "ta"); err != nil {
		t.Fatal(err)
	}
	if err := b.remember("https://b.example", "tb"); err != nil {
		t.Fatal(err)
	}
	saved, _ := loadServers(path)
	if saved.Tokens["https://a.example"] != "ta" || saved.Tokens["https://b.example"] != "tb" || saved.Current != "https://b.example" {
		t.Fatalf("saved %+v", saved)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "tui-*.json")); len(left) > 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestFailedLogoutKeepsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui.json")
	saved, _ := loadServers(path)
	_ = saved.remember("http://127.0.0.1:1", "tok")
	m := New(t.Context(), &client.Client{BaseURL: "http://127.0.0.1:1", Token: "tok"})
	m.servers = saved
	next := press(t, m, key('L', "L"))
	if got := content(next); next.(Model).client.Token != "tok" || !strings.Contains(got, "still paired") || strings.Contains(got, "Pair this terminal") {
		t.Fatalf("failed logout dropped the token:\n%s", got)
	}
	if reread, _ := loadServers(path); reread.Tokens["http://127.0.0.1:1"] != "tok" {
		t.Fatalf("saved token removed: %+v", reread)
	}
}
