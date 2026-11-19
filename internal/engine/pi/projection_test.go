package pi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/engine"
)

var at = time.UnixMilli(1700000000000)

func payloads(t *testing.T, events []engine.Event) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		var p map[string]any
		if err := json.Unmarshal(e.Data, &p); err != nil {
			t.Fatal(err)
		}
		if p["type"] != e.Type {
			t.Fatalf("type %q, payload %v", e.Type, p)
		}
		delete(p, "receivedAt")
		out = append(out, p)
	}
	return out
}

func project(t *testing.T, p *Projection, frames ...string) []map[string]any {
	t.Helper()
	var events []engine.Event
	for _, f := range frames {
		events = append(events, p.Project(json.RawMessage(f), at)...)
	}
	return payloads(t, events)
}

func TestProjectAssistantTurn(t *testing.T) {
	var p Projection
	got := project(t, &p,
		`{"type":"agent_start"}`,
		`{"type":"message_start","message":{"role":"assistant","timestamp":42}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"hm"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":1,"delta":"Hi <b>"}}`,
		`{"type":"message_end","message":{"role":"assistant","timestamp":42,"provider":"anthropic","model":"m","stopReason":"toolUse",
			"usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.5}},
			"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"Hi <b>"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]}}`,
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"ls"}}`,
		`{"type":"tool_execution_update","toolCallId":"c1","toolName":"bash","partialResult":{"content":[{"type":"text","text":"a"}]}}`,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","isError":true,"result":{"content":[{"type":"text","text":"ab"}]}}`,
		`{"type":"agent_settled"}`,
	)
	want := []map[string]any{
		{"type": "runtime", "state": map[string]any{"processing": true}},
		{"type": "thinking", "id": "pi:42:0", "text": "hm"},
		{"type": "text", "id": "pi:42", "text": "Hi <b>"},
		{"type": "usage", "usage": map[string]any{"id": "pi:42", "source": "pi.message_end", "provider": "anthropic", "model": "m",
			"input": 10.0, "output": 5.0, "cacheRead": 0.0, "cacheWrite": 0.0, "costUsd": 0.5}},
		{"type": "thinking", "id": "pi:42:0", "text": "hm", "replace": true},
		{"type": "message", "id": "pi:42", "text": "Hi <b>"},
		{"type": "tool", "id": "c1", "name": "bash", "input": map[string]any{"command": "ls"}},
		{"type": "tool", "id": "c1", "name": "bash", "input": map[string]any{"command": "ls"}},
		{"type": "tool_result", "id": "c1", "name": "bash", "text": "a", "failed": false, "partial": true,
			"native": map[string]any{"content": []any{map[string]any{"type": "text", "text": "a"}}}},
		{"type": "tool_result", "id": "c1", "name": "bash", "text": "ab", "failed": true, "partial": false,
			"native": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ab"}}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
	if !p.Settled() || p.Failure() != "" {
		t.Fatalf("settled %v failure %q", p.Settled(), p.Failure())
	}
}

func TestProjectFailuresAndCompaction(t *testing.T) {
	var p Projection
	got := project(t, &p,
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"rate limited","content":[{"type":"text","text":"partial"}]}}`,
	)
	if p.Failure() != "rate limited" || len(got) != 1 {
		t.Fatalf("failure %q events %v", p.Failure(), got)
	}
	project(t, &p, `{"type":"message_end","message":{"role":"assistant","stopReason":"aborted"}}`)
	if p.Failure() != "工作已停止。" {
		t.Fatalf("failure %q", p.Failure())
	}
	got = project(t, &p,
		`{"type":"compaction_start","reason":"manual"}`,
		`{"type":"compaction_end","aborted":true,"errorMessage":"too big"}`,
		`{"type":"compaction_start","reason":"threshold"}`,
		`{"type":"compaction_end"}`,
		`not json`,
	)
	want := []map[string]any{
		{"type": "compaction", "status": "running", "trigger": "manual"},
		{"type": "compaction", "status": "failed", "error": "too big"},
		{"type": "compaction", "status": "running", "trigger": "auto"},
		{"type": "compaction", "status": "completed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestProjectExtensionUI(t *testing.T) {
	var p Projection
	got := project(t, &p,
		`{"type":"extension_ui_request","id":"n1","method":"notify","message":"hello"}`,
		`{"type":"extension_ui_request","id":"s1","method":"setStatus","statusKey":"k","statusText":"busy"}`,
		`{"type":"extension_ui_request","id":"w1","method":"setWidget","widgetKey":"w","widgetLines":["a","b"]}`,
		`{"type":"extension_ui_request","id":"e1","method":"set_editor_text","text":"draft"}`,
		`{"type":"extension_ui_request","id":"c1","method":"confirm","title":"Computer operation","message":"rm -rf x"}`,
		`{"type":"extension_ui_request","id":"q1","method":"select","title":"Pick","options":["A","B"]}`,
		`{"type":"extension_ui_request","id":"q2","method":"editor","title":"Edit","prefill":"x"}`,
	)
	want := []map[string]any{
		{"type": "runtime", "state": map[string]any{"notices": map[string]any{"n1": "hello"}}},
		{"type": "runtime", "state": map[string]any{"notices": map[string]any{"k": "busy"}}},
		{"type": "runtime", "state": map[string]any{"notices": map[string]any{"w": "a\nb"}}},
		{"type": "runtime", "state": map[string]any{"editorText": "draft"}},
		{"type": "approval", "approval": map[string]any{"id": "c1", "title": "Computer operation", "detail": "rm -rf x", "kind": "permission", "automatic": true}},
		{"type": "approval", "approval": map[string]any{"id": "q1", "title": "Pick", "detail": "", "kind": "question", "questions": []any{
			map[string]any{"id": "q1", "title": "Pick", "allowOther": true, "options": []any{
				map[string]any{"id": "A", "label": "A"}, map[string]any{"id": "B", "label": "B"}}}}}},
		{"type": "approval", "approval": map[string]any{"id": "q2", "title": "Edit", "detail": "", "kind": "question", "questions": []any{
			map[string]any{"id": "q2", "title": "Edit", "value": "x", "multiline": true}}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
	if !IsDialog(json.RawMessage(`{"id":"c","method":"confirm"}`)) || IsDialog(json.RawMessage(`{"id":"n","method":"notify"}`)) {
		t.Fatal("IsDialog")
	}
}

func TestUIResponse(t *testing.T) {
	cases := []struct {
		request string
		answer  Answer
		want    map[string]any
	}{
		{`{"id":"c","method":"confirm"}`, Answer{Allow: true}, map[string]any{"confirmed": true}},
		{`{"id":"c","method":"confirm"}`, Answer{}, map[string]any{"cancelled": true}},
		{`{"id":"q","method":"select"}`, Answer{Allow: true, Answers: map[string][]string{"q": {"B"}}, Text: "t"}, map[string]any{"value": "B"}},
		{`{"id":"q","method":"input"}`, Answer{Allow: true, Text: "typed"}, map[string]any{"value": "typed"}},
	}
	for _, c := range cases {
		got, err := UIResponse(json.RawMessage(c.request), c.answer)
		if err != nil {
			t.Fatal(err)
		}
		var id struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(c.request), &id)
		c.want["type"], c.want["id"] = "extension_ui_response", id.ID
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %v want %v", c.request, got, c.want)
		}
	}
	closed := payloads(t, []engine.Event{ApprovalClosed("q")})[0]
	if closed["id"] != "q" {
		t.Fatalf("closed %v", closed)
	}
}

func TestRecoveredEntries(t *testing.T) {
	entries := []json.RawMessage{
		json.RawMessage(`{"type":"model_change","id":"e0"}`),
		json.RawMessage(`{"type":"message","id":"e1","message":{"role":"user","content":"hi"}}`),
		json.RawMessage(`{"type":"message","id":"e2","message":{"role":"assistant","timestamp":7,"stopReason":"stop","content":[{"type":"text","text":"yo"}]}}`),
		json.RawMessage(`{"type":"message","id":"e3","message":{"role":"assistant","stopReason":"aborted","content":[{"type":"text","text":"cut"}]}}`),
		json.RawMessage(`{"type":"message","id":"e4","message":{"role":"toolResult","toolCallId":"c","toolName":"read","isError":false,"content":[{"type":"text","text":"file"}]}}`),
	}
	got := payloads(t, Recovered(entries, at))
	want := []map[string]any{
		{"type": "message", "id": "pi:7", "text": "yo", "recovered": true, "timestamp": 7.0},
		{"type": "tool_result", "id": "c", "name": "read", "text": "file", "failed": false, "recovered": true,
			"native": map[string]any{"content": []any{map[string]any{"type": "text", "text": "file"}}, "details": nil}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
}

func TestContextReading(t *testing.T) {
	window := 1000
	got := payloads(t, []engine.Event{
		Context(json.RawMessage(`{"contextUsage":{"tokens":12,"contextWindow":200}}`), &window, "a/b"),
		Context(json.RawMessage(`{}`), &window, ""),
	})
	if got[0]["tokens"] != 12.0 || got[0]["window"] != 200.0 || got[0]["model"] != "a/b" || got[0]["source"] != "pi.get_session_stats" {
		t.Fatalf("got %v", got[0])
	}
	if got[1]["tokens"] != nil || got[1]["window"] != 1000.0 || got[1]["model"] != nil {
		t.Fatalf("got %v", got[1])
	}
}
