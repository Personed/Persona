package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/engine"
	"github.com/Personed/Persona/internal/memory"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/store/storetest"
	"github.com/Personed/Persona/internal/tools"
)

// fakeEngine stands in for Pi: each session connects to the Persona MCP gateway it
// was given, lists the tools, and plays turn when prompted.
type fakeEngine struct {
	engine.Engine
	turn func(ctx context.Context, model *mcp.ClientSession) []engine.Event

	started []engine.SessionOptions
	listed  []string
	prompts []engine.Prompt
	aborted atomic.Bool
}

func (e *fakeEngine) Start(ctx context.Context, options engine.SessionOptions) (engine.Session, error) {
	e.started = append(e.started, options)
	client := mcp.NewClient(&mcp.Implementation{Name: "pi-test", Version: "0"}, nil)
	model, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: options.Env["PERSONA_PI_MCP_URL"], MaxRetries: -1}, nil)
	if err != nil {
		return nil, err
	}
	listed, err := model.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	e.listed = nil
	for _, tool := range listed.Tools {
		e.listed = append(e.listed, tool.Name)
	}
	session := &fakeSession{engine: e, model: model, events: make(chan engine.Event, 4)}
	session.ctx, session.stop = context.WithCancel(context.Background())
	return session, nil
}

type fakeSession struct {
	engine.Session
	engine *fakeEngine
	model  *mcp.ClientSession
	events chan engine.Event
	ctx    context.Context
	stop   context.CancelFunc
	turns  sync.WaitGroup
}

func (s *fakeSession) ID() string { return "native-review" }

func (s *fakeSession) Prompt(_ context.Context, prompt engine.Prompt) error {
	s.engine.prompts = append(s.engine.prompts, prompt)
	s.turns.Go(func() {
		for _, event := range s.engine.turn(s.ctx, s.model) {
			s.events <- event
		}
	})
	return nil
}

func (s *fakeSession) Abort(context.Context) error {
	s.engine.aborted.Store(true)
	s.stop()
	return nil
}

func (s *fakeSession) Events() <-chan engine.Event { return s.events }

func (s *fakeSession) Close() error {
	s.stop()
	s.turns.Wait()
	return s.model.Close()
}

func settled(stopReason string) []engine.Event {
	return []engine.Event{
		{Type: "message_end", Data: json.RawMessage(fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","stopReason":%q,"errorMessage":"model failed"}}`, stopReason))},
		{Type: "agent_settled", Data: json.RawMessage(`{"type":"agent_settled"}`)},
	}
}

type review struct {
	service *memory.Service
	records *storetest.Store
	engine  *fakeEngine
	options memory.ReviewOptions
	other   *countingTool
	history []string
}

type countingTool struct{ runs atomic.Int64 }

func (*countingTool) Definition() tools.Definition {
	return tools.Definition{Name: "attach_file", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (c *countingTool) Run(context.Context, tools.Call) (tools.Result, error) {
	c.runs.Add(1)
	return tools.Result{}, nil
}

// newReview prepares Bot 1 with 11 counted turns and a registry that also holds a
// tool a review must not reach.
func newReview(t *testing.T, turn func(ctx context.Context, model *mcp.ClientSession) []engine.Event) *review {
	t.Helper()
	ctx := context.Background()
	service, records := newService(t)
	r := &review{service: service, records: records, engine: &fakeEngine{turn: turn}, other: &countingTool{}}
	registry := tools.NewRegistry()
	if err := errors.Join(memory.Register(registry, service), registry.Register(r.other)); err != nil {
		t.Fatal(err)
	}
	for range memory.ReviewTurns + 1 {
		if _, err := service.TurnCompleted(ctx, 1, false); err != nil {
			t.Fatal(err)
		}
	}
	r.options = memory.ReviewOptions{
		Engine:   r.engine,
		Registry: registry,
		BotID:    1,
		TaskID:   7,
		Through:  42,
		History: func(_ context.Context, after store.EntryID, budget int) (string, error) {
			r.history = append(r.history, fmt.Sprintf("%d/%d", after, budget))
			return `{"messages":[]}`, nil
		},
		Session: engine.SessionOptions{WorkDir: "/work", Model: engine.Model{Provider: "local", ModelID: "m"}, Env: map[string]string{"KEEP": "1"}},
	}
	return r
}

func (r *review) state(t *testing.T) memory.ReviewState {
	t.Helper()
	ctx := context.Background()
	meta, err := r.records.FindDocument(ctx, memory.ReviewDocKind, store.Scope{Kind: "conversation", ConversationID: 1}, "", store.Current)
	if err != nil {
		t.Fatal(err)
	}
	if meta.History != "latest" || meta.Fork != "initial" {
		t.Fatalf("review document = %+v", meta)
	}
	value, err := r.records.Document(ctx, meta.ID, store.Current)
	if err != nil {
		t.Fatal(err)
	}
	var state memory.ReviewState
	if err := json.Unmarshal(value.Value, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func call(ctx context.Context, model *mcp.ClientSession, name string, arguments map[string]any) (string, bool) {
	result, err := model.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return err.Error(), true
	}
	var text strings.Builder
	for _, content := range result.Content {
		if block, ok := content.(*mcp.TextContent); ok {
			text.WriteString(block.Text)
		}
	}
	return text.String(), result.IsError
}

func TestReviewSchedule(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	if enabled, err := service.ReviewEnabled(ctx, 1); err != nil || !enabled {
		t.Fatalf("ReviewEnabled = %v, %v", enabled, err)
	}
	for turn := 1; turn < memory.ReviewTurns; turn++ {
		if due, err := service.TurnCompleted(ctx, 1, true); err != nil || due {
			t.Fatalf("turn %d: due = %v, %v", turn, due, err)
		}
	}
	if due, err := service.TurnCompleted(ctx, 1, false); err != nil || due {
		t.Fatalf("review due while another task runs: %v, %v", due, err)
	}
	if due, err := service.TurnCompleted(ctx, 1, true); err != nil || !due {
		t.Fatalf("review not due after %d turns: %v, %v", memory.ReviewTurns+1, due, err)
	}
	if due, _ := service.TurnCompleted(ctx, 2, true); due {
		t.Fatal("another Bot's turns counted")
	}
	if err := service.SetReviewEnabled(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if due, err := service.TurnCompleted(ctx, 1, true); err != nil || due {
		t.Fatalf("disabled review due: %v, %v", due, err)
	}
	if err := service.SetReviewEnabled(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if due, _ := service.TurnCompleted(ctx, 1, true); due {
		t.Fatal("disabling kept the counted turns")
	}
}

func TestReviewSavesWithinLimits(t *testing.T) {
	ctx := context.Background()
	var r *review
	var user memory.Entry
	var saved, edit, remove, other, last string
	var calls int
	r = newReview(t, func(ctx context.Context, model *mcp.ClientSession) []engine.Event {
		other, _ = call(ctx, model, "attach_file", nil)
		saved, _ = call(ctx, model, "memory_save", map[string]any{"text": "Deploys with make ship", "category": "work", "revision": 0})
		edit, _ = call(ctx, model, "memory_save", map[string]any{"id": user.ID, "text": "Prefers English", "revision": 1})
		remove, _ = call(ctx, model, "memory_delete", map[string]any{"id": user.ID, "revision": 1})
		for calls = 3; ; calls++ {
			var failed bool
			if last, failed = call(ctx, model, "memory_list", nil); failed {
				return settled("stop")
			}
		}
	})
	var err error
	user, err = r.service.Save(ctx, 1, memory.Input{Text: "Prefers Chinese replies", Category: memory.Profile, RequestID: "r1"}, memory.UserSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.service.Review(ctx, r.options); err != nil {
		t.Fatal(err)
	}

	// query_result is the gateway's own read of results it shortened, as in TypeScript.
	if got := strings.Join(r.engine.listed, ","); got != "memory_delete,memory_list,memory_save,memory_search,query_result" {
		t.Fatalf("review tools = %s", got)
	}
	if r.other.runs.Load() != 0 || other == "" {
		t.Fatalf("review reached a non-memory tool: %q", other)
	}
	if !strings.Contains(saved, "Deploys with make ship") {
		t.Fatalf("memory_save = %s", saved)
	}
	if edit != "后台回顾不能修改用户添加的记忆。" || remove != "后台回顾不能删除用户添加的记忆。" {
		t.Fatalf("user record changes = %q, %q", edit, remove)
	}
	if calls != memory.ReviewCalls || !strings.Contains(last, "本次回顾已达到工具调用上限。") {
		t.Fatalf("call %d = %q, want the limit after %d calls", calls+1, last, memory.ReviewCalls)
	}
	items, _ := r.service.List(ctx, 1)
	if len(items) != 2 || items[0].Text != "Prefers Chinese replies" || items[1].Text != "Deploys with make ship" {
		t.Fatalf("memories = %+v", items)
	}

	started := r.engine.started[0]
	if started.BotID != 1 || started.SessionID != "" || started.WorkDir != "/work" || started.Model.ModelID != "m" || started.Env["KEEP"] != "1" || len(started.Tools) != 4 {
		t.Fatalf("session options = %+v", started)
	}
	if !strings.HasPrefix(started.SystemPrompt, "Review the recent conversation.") || !strings.Contains(started.SystemPrompt, "Prefers Chinese replies") {
		t.Fatalf("system prompt = %q", started.SystemPrompt)
	}
	if prompt := r.engine.prompts[0]; prompt.RequestID != "memory-review:7" || prompt.Text != "Quoted conversation data for memory review:\n"+`{"messages":[]}` {
		t.Fatalf("prompt = %+v", prompt)
	}
	if got := strings.Join(r.history, ","); got != fmt.Sprintf("0/%d", memory.ReviewHistoryBytes) {
		t.Fatalf("history reads = %s", got)
	}
	state := r.state(t)
	if !state.Enabled || state.Reviewing || state.Turns != 1 || state.Through == nil || *state.Through != 42 {
		t.Fatalf("state after review = %+v", state)
	}
	if r.engine.aborted.Load() {
		t.Fatal("settled review was aborted")
	}

	// The same task never sends its prompt twice.
	if err := r.service.Review(ctx, r.options); !errors.Is(err, memory.ErrReviewUnconfirmed) {
		t.Fatalf("repeated review = %v", err)
	}
	if state := r.state(t); len(r.engine.prompts) != 1 || state.Reviewing || state.Turns != 1 {
		t.Fatalf("repeated review: %d prompts, state %+v", len(r.engine.prompts), state)
	}
	// The next review reads only what the last one did not cover.
	r.options.TaskID, r.options.Through = 8, 60
	r.engine.turn = func(context.Context, *mcp.ClientSession) []engine.Event { return settled("stop") }
	if err := r.service.Review(ctx, r.options); err != nil {
		t.Fatal(err)
	}
	if state := r.state(t); r.history[1] != fmt.Sprintf("42/%d", memory.ReviewHistoryBytes) || state.Turns != 0 || *state.Through != 60 {
		t.Fatalf("second review: history %v, state %+v", r.history, state)
	}
}

func TestReviewStopsWithoutProgress(t *testing.T) {
	blocked := func(ctx context.Context, _ *mcp.ClientSession) []engine.Event {
		<-ctx.Done()
		return nil
	}
	cases := []struct {
		name    string
		turn    func(context.Context, *mcp.ClientSession) []engine.Event
		timeout time.Duration
		cancel  bool
		want    error
		aborted bool
	}{
		{name: "timeout", turn: blocked, timeout: 50 * time.Millisecond, want: memory.ErrReviewTimeout, aborted: true},
		{name: "new user input", turn: blocked, cancel: true, want: context.Canceled, aborted: true},
		{name: "model error", turn: func(context.Context, *mcp.ClientSession) []engine.Event { return settled("error") }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newReview(t, test.turn)
			r.options.SetTimeout(test.timeout)
			if test.cancel {
				turn := r.engine.turn
				r.engine.turn = func(ctx context.Context, model *mcp.ClientSession) []engine.Event {
					cancel()
					return turn(ctx, model)
				}
			}
			err := r.service.Review(ctx, r.options)
			if err == nil || (test.want != nil && !errors.Is(err, test.want)) || (test.want == nil && !strings.Contains(err.Error(), "model failed")) {
				t.Fatalf("Review = %v", err)
			}
			if r.engine.aborted.Load() != test.aborted {
				t.Fatalf("aborted = %v", r.engine.aborted.Load())
			}
			if state := r.state(t); state.Reviewing || state.Turns != memory.ReviewTurns+1 || state.Through != nil {
				t.Fatalf("state = %+v", state)
			}
		})
	}
}

func TestReviewDisabledDoesNotRun(t *testing.T) {
	ctx := context.Background()
	r := newReview(t, nil)
	if err := r.service.SetReviewEnabled(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := r.service.Review(ctx, r.options); err != nil || len(r.engine.started) != 0 {
		t.Fatalf("disabled review: %v, %d sessions", err, len(r.engine.started))
	}
	if state := r.state(t); state.Reviewing {
		t.Fatalf("state = %+v", state)
	}
}
