package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
	"github.com/Personed/Persona/internal/client/clienttest"
)

var fixtures = filepath.Join("..", "..", "contracts", "api")

// drive runs cmd and feeds its messages back into m until no command is left.
// Cursor blinks and reconnects are dropped, so the end of a recorded stream ends the loop.
func drive(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 100 {
			t.Fatal("too many steps")
		}
		cmd, queue = queue[0], queue[1:]
		if cmd == nil {
			continue
		}
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if _, ok := msg.(reconnectMsg); ok || msg == nil || strings.HasPrefix(fmt.Sprintf("%T", msg), "cursor.") {
			continue
		}
		var next tea.Cmd
		m, next = m.Update(msg)
		queue = append(queue, next)
	}
	return m
}

func key(code rune, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Text: text}
}

func content(m tea.Model) string { return m.View().Content }

func TestBotsAndChat(t *testing.T) {
	server := clienttest.New(t, fixtures)
	ctx := t.Context()
	var m tea.Model = New(ctx, &client.Client{BaseURL: server.URL})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = drive(t, m, m.Init())
	if got := content(m); !strings.Contains(got, "> Persona") {
		t.Fatalf("bots view:\n%s", got)
	}

	m, cmd := m.Update(key(tea.KeyEnter, ""))
	m = drive(t, m, cmd)
	got := content(m)
	if !strings.Contains(got, "You") || !strings.Contains(got, "hello") {
		t.Fatalf("chat view after snapshot:\n%s", got)
	}

	for _, r := range "hi there" {
		m, _ = m.Update(key(r, string(r)))
	}
	m, cmd = m.Update(key(tea.KeyEnter, ""))
	m = drive(t, m, cmd)
	requests := server.Requests()
	last := requests[len(requests)-1]
	var body struct {
		JSON struct {
			BotID     int    `json:"botId"`
			Text      string `json:"text"`
			RequestID string `json:"requestId"`
		} `json:"json"`
	}
	if err := json.Unmarshal(last.Body, &body); err != nil || last.Path != "/rpc/send" || body.JSON.Text != "hi there" || body.JSON.BotID != 1 || len(body.JSON.RequestID) != 36 {
		t.Fatalf("send request %s %s", last.Path, last.Body)
	}
	if strings.Contains(content(m), "> hi there") {
		t.Fatal("input not cleared after send")
	}

	m, cmd = m.Update(key(tea.KeyEscape, ""))
	m = drive(t, m, cmd)
	if got := content(m); !strings.Contains(got, "Persona · Bots") {
		t.Fatalf("esc did not return to Bots:\n%s", got)
	}
}

func TestChatStreamsUpdates(t *testing.T) {
	m := New(context.Background(), &client.Client{})
	m.width, m.height = 80, 20
	next, _ := m.openChat(client.Bot{ID: 1, Name: "Ada"})
	m = next.(Model)
	apply := func(change client.Change) {
		next, _ := m.Update(changeMsg{botID: 1, change: change, stream: nil})
		m = next.(Model)
	}
	apply(client.Change{Type: "snapshot", View: &client.ChatView{ID: 1, Messages: []client.Message{
		{ID: "u", Role: "user", Content: []client.Content{{Type: "text", Text: "ping"}}},
	}}})
	apply(client.Change{Type: "update", State: map[string]json.RawMessage{"busy": json.RawMessage("true")}, Messages: []client.Message{
		{ID: "a", Role: "assistant", Content: []client.Content{{Type: "text", Text: "po"}}},
	}})
	if got := content(m); !strings.Contains(got, "po") || !strings.Contains(got, "working") || !strings.Contains(got, "ctrl+x stop") {
		t.Fatalf("partial reply:\n%s", got)
	}
	apply(client.Change{Type: "update", State: map[string]json.RawMessage{"busy": json.RawMessage("false")}, Messages: []client.Message{
		{ID: "a", Role: "assistant", Content: []client.Content{{Type: "text", Text: "pong"}, {Type: "toolCall", Name: "shell", Arguments: json.RawMessage(`{ "cmd": "ls" }`)}}},
		{ID: "t", Role: "toolResult", ToolName: "shell", ToolStatus: "completed"},
	}})
	got := content(m)
	if !strings.Contains(got, "pong") || strings.Contains(got, "working") || !strings.Contains(got, `⏺ shell {"cmd":"ls"}`) || !strings.Contains(got, "↳ shell completed") {
		t.Fatalf("final reply:\n%s", got)
	}
	if next, _ := m.Update(changeMsg{botID: 2, change: client.Change{Type: "snapshot", View: &client.ChatView{}}}); !strings.Contains(content(next), "pong") {
		t.Fatal("change for another Bot replaced the chat")
	}
}

func TestErrorsShown(t *testing.T) {
	server := clienttest.Serve(t)
	var m tea.Model = New(t.Context(), &client.Client{BaseURL: server.URL})
	m = drive(t, m, m.Init())
	if got := content(m); !strings.Contains(got, "NOT_FOUND") {
		t.Fatalf("error not shown:\n%s", got)
	}
}

func TestRequestID(t *testing.T) {
	id := requestID()
	if len(id) != 36 || id[14] != '4' || strings.Count(id, "-") != 4 || id == requestID() {
		t.Fatalf("requestID = %q", id)
	}
}

func TestTruncateWide(t *testing.T) {
	if got := truncate("已收到：hello", 6); got != "已收…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate = %q", got)
	}
}
