package tui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
	"github.com/Personed/Persona/internal/client/clienttest"
)

func init() { reconnectDelay = time.Millisecond }

func openChat(t *testing.T, c *client.Client) Model {
	t.Helper()
	m := New(t.Context(), c)
	m.width, m.height = 80, 12
	next, _ := m.openChat(client.Bot{ID: 1, Name: "Ada"})
	return next.(Model)
}

func typeText(m tea.Model, text string) tea.Model {
	for _, r := range text {
		m, _ = m.Update(key(r, string(r)))
	}
	return m
}

func TestSendFailureKeepsDraft(t *testing.T) {
	server := clienttest.Serve(t)
	var m tea.Model = openChat(t, &client.Client{BaseURL: server.URL})
	m, cmd := typeText(m, "hello").Update(key(tea.KeyEnter, ""))
	m, _ = m.Update(cmd())
	got := m.(Model)
	if got.chat.input.Value() != "hello" || got.err == nil || got.chat.retryID != "" {
		t.Fatalf("rejected send: input=%q err=%v retry=%q", got.chat.input.Value(), got.err, got.chat.retryID)
	}

	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	m = openChat(t, &client.Client{BaseURL: down.URL})
	m, cmd = typeText(m, "again").Update(key(tea.KeyEnter, ""))
	m, _ = m.Update(cmd())
	first := m.(Model).chat.retryID
	if first == "" || m.(Model).chat.input.Value() != "again" {
		t.Fatalf("unknown outcome: retry=%q input=%q", first, m.(Model).chat.input.Value())
	}
	_, cmd = m.Update(key(tea.KeyEnter, ""))
	if msg := cmd().(sendMsg); msg.requestID != first {
		t.Fatalf("retry used %q, want %q", msg.requestID, first)
	}
}

func TestOlderPage(t *testing.T) {
	var before string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		before = string(body)
		_, _ = io.WriteString(w, `{"json":{"id":1,"messages":[{"id":"old","role":"user","content":[{"type":"text","text":"first ever"}]},{"id":"m0","role":"user","content":[]}],"before":""}}`)
	}))
	defer server.Close()
	m := openChat(t, &client.Client{BaseURL: server.URL})
	view := &client.ChatView{ID: 1, Before: "10:2"}
	for i := range 20 {
		view.Messages = append(view.Messages, client.Message{ID: "m" + strconv.Itoa(i), Role: "user", Content: []client.Content{{Type: "text", Text: "line " + strconv.Itoa(i)}}})
	}
	var next tea.Model = m
	next, _ = next.Update(changeMsg{botID: 1, change: client.Change{Type: "snapshot", View: view}})
	next, cmd := next.Update(key(tea.KeyPgUp, ""))
	for range 10 {
		if cmd != nil {
			break
		}
		next, cmd = next.Update(key(tea.KeyPgUp, ""))
	}
	if cmd == nil {
		t.Fatal("no older page requested at the top")
	}
	next, _ = next.Update(cmd())
	got := next.(Model)
	if !strings.Contains(before, `"before":"10:2"`) || len(got.chat.view.Messages) != 21 || got.chat.view.Messages[0].ID != "old" || got.chat.view.Before != "" {
		t.Fatalf("request %s, messages %d, before %q", before, len(got.chat.view.Messages), got.chat.view.Before)
	}
	if cmd := got.loadOlder(); cmd != nil {
		t.Fatal("loaded past the first message")
	}
}

func TestReconnect(t *testing.T) {
	server := clienttest.New(t, fixtures)
	m := openChat(t, &client.Client{BaseURL: server.URL})
	next, cmd := m.Update(changeMsg{botID: 1, err: io.EOF})
	if cmd == nil {
		t.Fatal("no reconnect after the stream ended")
	}
	next, cmd = next.Update(cmd())
	if _, ok := cmd().(streamMsg); !ok {
		t.Fatal("reconnect did not resubscribe")
	}
	if _, cmd := next.Update(changeMsg{botID: 1, err: context.Canceled}); cmd != nil {
		t.Fatal("reconnecting a closed chat")
	}
}

func TestSecure(t *testing.T) {
	for server, want := range map[string]bool{
		"https://persona.example.com": true, "http://127.0.0.1:4310": true, "http://localhost:4310": true, "http://[::1]:4310": true,
		"http://persona.example.com": false, "http://192.168.1.5:4310": false, "ftp://localhost": false,
	} {
		if got := secure(server); got != want {
			t.Errorf("secure(%q) = %v", server, got)
		}
	}
}
