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
