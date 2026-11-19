package pi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/engine"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := New(Config{
		Command:     exe,
		SessionDir:  t.TempDir(),
		StateDir:    t.TempDir(),
		Env:         append(os.Environ(), fakeMode+"=rpc"),
		IdleTimeout: 50 * time.Millisecond,
		StopGrace:   2 * time.Second,
	})
	t.Cleanup(e.Close)
	return e
}

func nextEvent(t *testing.T, s engine.Session) engine.Event {
	t.Helper()
	select {
	case event, ok := <-s.Events():
		if !ok {
			t.Fatal("events closed")
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return engine.Event{}
}

func eventsUntil(t *testing.T, s engine.Session, last string) []string {
	t.Helper()
	var types []string
	for {
		event := nextEvent(t, s)
		types = append(types, event.Type)
		if event.Type == last {
			return types
		}
	}
}

func rawState(t *testing.T, s engine.Session) map[string]any {
	t.Helper()
	data, err := s.(*session).conn.client.Request(context.Background(), "get_state", nil)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func pid(s engine.Session) int { return s.(*session).conn.proc.PID() }

func TestEngineSessionOverRPC(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	work := t.TempDir()
	s, err := e.Start(ctx, engine.SessionOptions{
		BotID:         1,
		WorkDir:       work,
		Model:         engine.Model{Provider: "anthropic", ModelID: "claude"},
		ThinkingLevel: "high",
		SystemPrompt:  "Be Persona.",
		Env:           map[string]string{"PERSONA_TEST_ENV": "bot-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.HasSuffix(s.ID(), ".jsonl") {
		t.Fatalf("ID = %q", s.ID())
	}
	state, err := s.State(ctx)
	if err != nil || state.SessionID != s.ID() || state.Model == nil || *state.Model != (engine.Model{Provider: "anthropic", ModelID: "claude"}) || state.ThinkingLevel != "high" {
		t.Fatalf("State = %+v, %v", state, err)
	}
	raw := rawState(t, s)
	var argv []string
	for _, arg := range raw["argv"].([]any) {
		argv = append(argv, arg.(string))
	}
	i := slices.Index(argv, "--append-system-prompt")
	if !slices.Contains(argv, "--session-dir") || i < 0 || raw["env"] != "bot-1" {
		t.Fatalf("launch argv %q env %v", argv, raw["env"])
	}
	if prompt, err := os.ReadFile(argv[i+1]); err != nil || string(prompt) != "Be Persona." {
		t.Fatalf("system prompt file = %q, %v", prompt, err)
	}
	if cwd, _ := filepath.EvalSymlinks(raw["cwd"].(string)); cwd != mustEval(t, work) {
		t.Fatalf("cwd = %v, want %s", raw["cwd"], work)
	}

	if err := s.Prompt(ctx, engine.Prompt{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if got := eventsUntil(t, s, "agent_settled"); !slices.Equal(got, []string{"agent_start", "message_update", "agent_end", "agent_settled"}) {
		t.Fatalf("events = %v", got)
	}

	if err := s.Prompt(ctx, engine.Prompt{Text: "ask"}); err != nil {
		t.Fatal(err)
	}
	var request struct{ ID, Method string }
	for request.Method == "" {
		if event := nextEvent(t, s); event.Type == "extension_ui_request" {
			_ = json.Unmarshal(event.Data, &request)
		}
	}
	if err := s.Answer(ctx, request.ID, json.RawMessage(`{"value":"b"}`)); err != nil {
		t.Fatal(err)
	}
	answered := nextEvent(t, s)
	if answered.Type != "answered" || !strings.Contains(string(answered.Data), `"id":"ui-1"`) || !strings.Contains(string(answered.Data), `"value":"b"`) {
		t.Fatalf("answer event = %s %s", answered.Type, answered.Data)
	}
	nextEvent(t, s)

	usage, err := s.Usage(ctx)
	if err != nil || usage != (engine.Usage{Input: 10, Output: 5, CacheRead: 2, CacheWrite: 1, Cost: 0.25}) {
		t.Fatalf("Usage = %+v, %v", usage, err)
	}
	if messages, err := s.Messages(ctx, 1, 10); err != nil || len(messages) != 2 {
		t.Fatalf("Messages = %d, %v", len(messages), err)
	}
	if messages, _ := s.Messages(ctx, 5, 10); len(messages) != 0 {
		t.Fatal("Messages past the end returned entries")
	}
	if err := s.Steer(ctx, engine.Prompt{Text: "also"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(ctx, "keep code"); err != nil {
		t.Fatal(err)
	}
	pid := s.(*session).conn.proc
	if err := s.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pid.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Abort left the Bot's Pi running")
	}
	if _, ok := <-s.Events(); ok {
		t.Fatal("events still open after Abort")
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestEngineReusesProcessAndResumes(t *testing.T) {
	e := newTestEngine(t)
	e.config.IdleTimeout = time.Hour
	ctx := context.Background()
	options := engine.SessionOptions{BotID: 1, SystemPrompt: "a"}
	first, err := e.Start(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Start(ctx, options); !errors.Is(err, ErrBusy) {
		t.Fatalf("second open session = %v", err)
	}
	firstPID, firstID := pid(first), first.ID()
	_ = first.Close()
	if _, ok := <-first.Events(); ok {
		t.Fatal("events open after Close")
	}

	fresh, err := e.Start(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if pid(fresh) != firstPID || fresh.ID() == firstID {
		t.Fatalf("fresh session: pid %d id %q", pid(fresh), fresh.ID())
	}
	_ = fresh.Close()

	options.SessionID = firstID
	resumed, err := e.Start(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if pid(resumed) != firstPID || resumed.ID() != firstID {
		t.Fatalf("resumed: pid %d id %q", pid(resumed), resumed.ID())
	}
	_ = resumed.Close()

	options.SystemPrompt = "b"
	relaunched, err := e.Start(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer relaunched.Close()
	if pid(relaunched) == firstPID || relaunched.ID() != firstID {
		t.Fatalf("changed launch settings kept pid %d", firstPID)
	}
}

func TestEngineInfoAndModelsLeaveNoProcess(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	info, err := e.Info(ctx)
	if err != nil || info.ID != "pi" || info.Version != "1.5.2-fake" || !info.Capabilities["recovery"] {
		t.Fatalf("Info = %+v, %v", info, err)
	}
	models, err := e.Models(ctx)
	if err != nil || len(models) != 1 || models[0] != (engine.Model{Provider: "anthropic", ModelID: "claude"}) {
		t.Fatalf("Models = %+v, %v", models, err)
	}
	if e.Live() != 0 {
		t.Fatal("catalog left a Bot process")
	}
}

func TestEngineRequestRejected(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.Start(context.Background(), engine.SessionOptions{BotID: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var rejected *RequestError
	if err := s.(*session).call(context.Background(), "reject", nil, nil); !errors.As(err, &rejected) {
		t.Fatalf("err = %v", err)
	}
}

// TestEngineIdleBotsHaveNoProcess is the G2 resource check: after every Bot's
// session closes and the idle timeout passes, no Pi process remains.
func TestEngineIdleBotsHaveNoProcess(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	var procs []*Process
	for botID := int64(1); botID <= 3; botID++ {
		s, err := e.Start(ctx, engine.SessionOptions{BotID: botID})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Prompt(ctx, engine.Prompt{Text: "hi"}); err != nil {
			t.Fatal(err)
		}
		eventsUntil(t, s, "agent_settled")
		procs = append(procs, s.(*session).conn.proc)
		_ = s.Close()
	}
	waitFor(t, "idle Pi processes to exit", func() bool {
		for _, p := range procs {
			select {
			case <-p.Done():
			default:
				return false
			}
		}
		return e.Live() == 0
	})
}

func TestWriteToolsFile(t *testing.T) {
	dir := t.TempDir()
	path, err := writeTools(dir, 7, engine.SessionOptions{
		Env:   map[string]string{"PERSONA_PI_MCP_URL": "http://127.0.0.1:9/mcp"},
		Tools: []engine.ToolSpec{{Name: "memory_search", Description: "Search", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil || !filepath.IsAbs(path) {
		t.Fatalf("path %q, %v", path, err)
	}
	data, _ := os.ReadFile(path)
	var file struct {
		URL   string            `json:"url"`
		Tools []engine.ToolSpec `json:"tools"`
	}
	if json.Unmarshal(data, &file) != nil || file.URL != "http://127.0.0.1:9/mcp" || len(file.Tools) != 1 || file.Tools[0].Name != "memory_search" {
		t.Fatalf("tools file %s", data)
	}
}
