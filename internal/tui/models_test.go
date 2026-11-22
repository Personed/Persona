package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Personed/Persona/internal/client"
	"github.com/Personed/Persona/internal/client/clienttest"
)

const engineJSON = `{"config":{"provider":"pi","model":"faux/faux-1","options":{"keep":"1"}},"revision":4,
	"catalog":[{"id":"pi","name":"Pi","models":[{"id":"stale","name":"Stale"}]}],
	"connection":{"ready":true,"models":[
		{"id":"faux/faux-1","name":"Faux one","effort":{"id":"thinkingLevel","value":"","options":[{"id":"off","label":"off"}]}},
		{"id":"faux/faux-2","name":"Faux two","effort":{"id":"thinkingLevel","value":"","options":[{"id":"off"},{"id":"low"},{"id":"high"}]}}]}}`

func TestModelPicker(t *testing.T) {
	server := clienttest.Serve(t)
	var m tea.Model = openChat(t, &client.Client{BaseURL: server.URL})
	m, _ = m.(Model).Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	var engine client.Engine
	if err := json.Unmarshal([]byte(engineJSON), &engine); err != nil {
		t.Fatal(err)
	}
	m, _ = m.Update(engineMsg{botID: 1, engine: engine})
	got := content(m)
	if !strings.Contains(got, "> Faux one  faux/faux-1  (current)") || !strings.Contains(got, "Faux two  faux/faux-2  ‹ off ›") || strings.Contains(got, "Stale") {
		t.Fatalf("picker:\n%s", got)
	}
	m = press(t, m, key(tea.KeyDown, ""), key(tea.KeyRight, ""), key(tea.KeyRight, ""))
	if got := content(m); !strings.Contains(got, "‹ high ›") {
		t.Fatalf("effort not changed:\n%s", got)
	}
	m = press(t, m, key(tea.KeyEnter, ""))
	if body := lastBody(t, server, "/rpc/engines/select"); !strings.Contains(body, `"model":"faux/faux-2"`) || !strings.Contains(body, `"options":{"thinkingLevel":"high"}`) || !strings.Contains(body, `"revision":4`) {
		t.Fatalf("select body %s", body)
	}
	if strings.Contains(content(m), "Model · pi") {
		t.Fatal("picker still open after save")
	}
}

func TestEngineModelsFallback(t *testing.T) {
	e := client.Engine{Config: client.EngineConfig{Provider: "codex"}, Catalog: []client.Harness{{ID: "pi", Models: []client.EngineModel{{ID: "a"}}}, {ID: "codex", Models: []client.EngineModel{{ID: "b"}}}}}
	if models := e.Models(); len(models) != 1 || models[0].ID != "b" {
		t.Fatalf("Models() = %+v", models)
	}
}

func pickerWith(t *testing.T, height int) tea.Model {
	t.Helper()
	server := clienttest.Serve(t)
	var m tea.Model = openChat(t, &client.Client{BaseURL: server.URL})
	m, _ = m.(Model).Update(tea.WindowSizeMsg{Width: 100, Height: height})
	var engine client.Engine
	if err := json.Unmarshal([]byte(engineJSON), &engine); err != nil {
		t.Fatal(err)
	}
	m, _ = m.Update(engineMsg{botID: 1, engine: engine})
	return m
}

func TestSameModelKeepsOptions(t *testing.T) {
	server := clienttest.Serve(t)
	var m tea.Model = openChat(t, &client.Client{BaseURL: server.URL})
	var engine client.Engine
	_ = json.Unmarshal([]byte(engineJSON), &engine)
	m, _ = m.Update(engineMsg{botID: 1, engine: engine})
	press(t, m, key(tea.KeyEnter, ""))
	if body := lastBody(t, server, "/rpc/engines/select"); !strings.Contains(body, `"options":{"keep":"1","thinkingLevel":"off"}`) {
		t.Fatalf("select body %s", body)
	}
}

func TestSendWaitsForModelSave(t *testing.T) {
	m := pickerWith(t, 30)
	m, cmd := m.Update(key(tea.KeyEnter, ""))
	if cmd == nil || !m.(Model).chat.saving || !strings.Contains(content(m), "saving model") {
		t.Fatal("model save not pending")
	}
	m = typeText(m, "next question")
	m, cmd = m.Update(key(tea.KeyEnter, ""))
	if cmd != nil || m.(Model).chat.input.Value() != "next question" {
		t.Fatalf("sent while the model save was pending: draft %q", m.(Model).chat.input.Value())
	}
	m, _ = m.Update(modelSavedMsg{botID: 1})
	if _, cmd = m.Update(key(tea.KeyEnter, "")); cmd == nil {
		t.Fatal("send still blocked after the model saved")
	}
}

func TestPickerWindowFollowsCursor(t *testing.T) {
	m := pickerWith(t, 30)
	p := m.(Model).chat.picker
	for i := range 30 {
		p.models = append(p.models, client.EngineModel{ID: "m" + itoa(i), Name: "Model " + itoa(i)})
	}
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	for range 31 {
		m = press(t, m, key(tea.KeyDown, ""))
	}
	got := content(m)
	if !strings.Contains(got, "> Model 29") || strings.Contains(got, "Faux one") {
		t.Fatalf("picker window:\n%s", got)
	}
	if lines := strings.Count(got, "\n") + 1; lines > 20 {
		t.Fatalf("picker used %d lines of 20", lines)
	}
}
