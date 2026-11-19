package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
	"github.com/Personed/Persona/internal/client/clienttest"
)

func chatWith(t *testing.T, approvals ...client.Approval) (tea.Model, *clienttest.Server) {
	t.Helper()
	server := clienttest.Serve(t)
	m := New(t.Context(), &client.Client{BaseURL: server.URL})
	m.width, m.height = 100, 30
	next, _ := m.openChat(client.Bot{ID: 1, Name: "Ada"})
	next, _ = next.Update(changeMsg{botID: 1, change: client.Change{Type: "snapshot", View: &client.ChatView{ID: 1, Busy: true, Approvals: approvals}}})
	return next, server
}

func lastAnswer(t *testing.T, server *clienttest.Server) string {
	t.Helper()
	requests := server.Requests()
	if len(requests) == 0 || requests[len(requests)-1].Path != "/rpc/engines/answer" {
		t.Fatalf("no engines.answer request: %+v", requests)
	}
	var body struct{ JSON json.RawMessage }
	_ = json.Unmarshal(requests[len(requests)-1].Body, &body)
	return string(body.JSON)
}

func press(t *testing.T, m tea.Model, keys ...tea.KeyPressMsg) tea.Model {
	t.Helper()
	for _, k := range keys {
		var cmd tea.Cmd
		m, cmd = m.Update(k)
		m = drive(t, m, cmd)
	}
	return m
}

func TestPermissionApproval(t *testing.T) {
	m, server := chatWith(t, client.Approval{ID: "p1", Title: "Run shell command", Kind: "permission", ConfirmLabel: "Run",
		Fields: []struct {
			Label string `json:"label"`
			Value string `json:"value"`
		}{{Label: "Command", Value: "ls -la"}}})
	got := content(m)
	if !strings.Contains(got, "? Run shell command") || !strings.Contains(got, "Command: ls -la") || !strings.Contains(got, "> Run") || !strings.Contains(got, "y allow") {
		t.Fatalf("permission card:\n%s", got)
	}
	press(t, m, key('y', "y"))
	if got := lastAnswer(t, server); got != `{"allow":true,"botId":1,"id":"p1"}` {
		t.Fatalf("answer = %s", got)
	}

	m, server = chatWith(t, client.Approval{ID: "p2", Title: "Open file"})
	press(t, m, key(tea.KeyDown, ""), key(tea.KeyEnter, ""))
	if got := lastAnswer(t, server); got != `{"allow":false,"botId":1,"id":"p2"}` {
		t.Fatalf("deny answer = %s", got)
	}
}

func TestQuestionApproval(t *testing.T) {
	m, server := chatWith(t, client.Approval{ID: "q", Title: "Questions", Kind: "question", Questions: []client.Question{
		{ID: "color", Title: "Pick a color", Options: []client.Option{{ID: "red", Label: "Red"}, {ID: "blue", Label: "Blue"}}},
		{ID: "tags", Title: "Pick tags", Multiple: true, Options: []client.Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}},
		{ID: "note", Title: "Anything else?", Required: true, AllowOther: true},
	}})
	if got := content(m); !strings.Contains(got, "Pick a color") || !strings.Contains(got, "(1/3)") {
		t.Fatalf("question card:\n%s", got)
	}
	m = press(t, m, key(tea.KeyDown, ""), key(tea.KeyEnter, ""))
	m = press(t, m, key(tea.KeySpace, " "), key(tea.KeyDown, ""), key(tea.KeySpace, " "))
	if got := content(m); !strings.Contains(got, "[x] A") || !strings.Contains(got, "[x] B") {
		t.Fatalf("multiple choice:\n%s", got)
	}
	m = press(t, m, key(tea.KeyEnter, ""))
	m = press(t, m, key(tea.KeyEnter, ""))
	if got := content(m); !strings.Contains(got, "Anything else?") {
		t.Fatalf("required question skipped:\n%s", got)
	}
	m = press(t, m, key(tea.KeyTab, ""))
	for _, r := range "ship it" {
		m = press(t, m, key(r, string(r)))
	}
	press(t, m, key(tea.KeyEnter, ""))
	if got := lastAnswer(t, server); got != `{"allow":true,"answers":{"color":["blue"],"note":["ship it"],"tags":["a","b"]},"botId":1,"id":"q"}` {
		t.Fatalf("answers = %s", got)
	}
}

func TestDeclineQuestion(t *testing.T) {
	m, server := chatWith(t, client.Approval{ID: "q", Title: "Q", Kind: "question", Questions: []client.Question{{ID: "x", Title: "X?"}}})
	press(t, m, tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	if got := lastAnswer(t, server); got != `{"allow":false,"botId":1,"id":"q"}` {
		t.Fatalf("decline = %s", got)
	}
}

func TestApprovalRefocusesInput(t *testing.T) {
	m, _ := chatWith(t, client.Approval{ID: "p1", Title: "Open file"})
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"json":{}}`) }))
	t.Cleanup(ok.Close)
	model := m.(Model)
	model.client = &client.Client{BaseURL: ok.URL}
	m = press(t, model, key(tea.KeyTab, ""), key(tea.KeyTab, ""), key(tea.KeyEnter, ""))
	if c := m.(Model).chat; c.answer != nil || !c.input.Focused() {
		t.Fatalf("answer %v, input focused %v, err %v", c.answer, c.input.Focused(), m.(Model).err)
	}
}

func TestFailedAnswerKeepsCard(t *testing.T) {
	m, _ := chatWith(t, client.Approval{ID: "q", Title: "Q", Kind: "question", Questions: []client.Question{
		{ID: "x", Title: "X?", Options: []client.Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}}}})
	model := m.(Model)
	model.client = &client.Client{BaseURL: "http://127.0.0.1:1"}
	m = press(t, model, key(tea.KeyDown, ""), key(tea.KeyEnter, ""))
	c := m.(Model).chat
	if c.answer == nil || c.answer.picked["x"][0] != "b" || m.(Model).err == nil {
		t.Fatalf("card not restored after failed send: %+v, err %v", c.answer, m.(Model).err)
	}
}

func TestClosedQuestionRefusesText(t *testing.T) {
	m, _ := chatWith(t, client.Approval{ID: "q", Title: "Q", Kind: "question", Questions: []client.Question{
		{ID: "x", Title: "X?", Options: []client.Option{{ID: "a", Label: "A"}}}}})
	m = press(t, m, key(tea.KeyTab, ""))
	if a := m.(Model).chat.answer; a.typing || strings.Contains(content(m), "tab type answer") {
		t.Fatalf("closed question accepted text:\n%s", content(m))
	}
}
