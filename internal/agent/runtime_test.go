package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/engine"
	"github.com/Personed/Persona/internal/store"
)

// scriptEngine runs each prompt's frames, and holds the prompt open until
// release is closed when hold is set.
type scriptEngine struct {
	frames   []string
	hold     chan struct{}
	started  chan *scriptSession
	steerErr error
}

func (scriptEngine) Info(context.Context) (engine.Info, error)      { return engine.Info{}, nil }
func (scriptEngine) Models(context.Context) ([]engine.Model, error) { return nil, nil }
func (e scriptEngine) Start(_ context.Context, options engine.SessionOptions) (engine.Session, error) {
	s := &scriptSession{e: e, options: options, events: make(chan engine.Event, 32), aborted: make(chan struct{})}
	if e.started != nil {
		e.started <- s
	}
	return s, nil
}

type scriptSession struct {
	e       scriptEngine
	events  chan engine.Event
	aborted chan struct{}
	once    sync.Once
	abort   sync.Once
	steered []string
	answers []string
	options engine.SessionOptions
	closed  bool
	mu      sync.Mutex
}

func (s *scriptSession) ID() string { return "session" }
func (s *scriptSession) Prompt(context.Context, engine.Prompt) error {
	go func() {
		if s.e.hold != nil {
			select {
			case <-s.e.hold:
			case <-s.aborted:
				return
			}
		}
		for _, frame := range s.e.frames {
			s.mu.Lock()
			if !s.closed {
				s.events <- engine.Event{Type: "frame", Data: json.RawMessage(frame)}
			}
			s.mu.Unlock()
		}
	}()
	return nil
}
func (s *scriptSession) Steer(_ context.Context, p engine.Prompt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steered = append(s.steered, p.Text)
	return s.e.steerErr
}
func (s *scriptSession) Abort(context.Context) error {
	s.abort.Do(func() { close(s.aborted) })
	return nil
}
func (s *scriptSession) Answer(_ context.Context, id string, value json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = append(s.answers, id+" "+string(value))
	return nil
}
func (s *scriptSession) SetModel(context.Context, engine.Model, string) error { return nil }
func (s *scriptSession) Compact(context.Context, string) error                { return nil }
func (s *scriptSession) State(context.Context) (engine.State, error)          { return engine.State{}, nil }
func (s *scriptSession) Usage(context.Context) (engine.Usage, error)          { return engine.Usage{}, nil }
func (s *scriptSession) Messages(context.Context, int, int) ([]json.RawMessage, error) {
	return nil, nil
}
func (s *scriptSession) Events() <-chan engine.Event { return s.events }
func (s *scriptSession) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.events)
		s.mu.Unlock()
	})
	return nil
}

const (
	answerFrames = `{"type":"message_start","message":{"role":"assistant","timestamp":7}}
{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Hi"}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Hi there"}],"stopReason":"stop","timestamp":7}}
{"type":"agent_settled"}`
	failFrames = `{"type":"message_start","message":{"role":"assistant","timestamp":8}}
{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"rate limited","timestamp":8}}
{"type":"agent_settled"}`
)

func runtimeFor(t *testing.T, native bool, e scriptEngine) (*Agent, *Runtime) {
	t.Helper()
	a := open(t)
	return a, NewRuntime(a, RuntimeOptions{Engine: e, Native: native})
}

func idle(t *testing.T, rt *Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.Idle(ctx); err != nil {
		t.Fatal(err)
	}
}

func engineView(t *testing.T, rt *Runtime, bot int64) EngineView {
	t.Helper()
	view, err := rt.View(context.Background(), bot, "")
	if err != nil {
		t.Fatal(err)
	}
	return view.(EngineView)
}

func encoded(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRuntimeAnswersAndDedupes(t *testing.T) {
	ctx := context.Background()
	a, rt := runtimeFor(t, true, scriptEngine{frames: strings.Split(answerFrames, "\n")})
	bot := int64(a.Root)
	in := SendInput{BotID: bot, Text: "hello", RequestID: "r1"}
	first, err := rt.Submit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	idle(t, rt)
	again, err := rt.Submit(ctx, in)
	if err != nil || again != first {
		t.Fatalf("retry = %d, %v; want %d", again, err, first)
	}
	idle(t, rt)
	view := engineView(t, rt, bot)
	got := encoded(t, view.Messages)
	if len(view.Messages) != 2 || !strings.Contains(got, `"Hi there"`) || view.Busy || view.Failed {
		t.Fatalf("view = %s busy=%v failed=%v", got, view.Busy, view.Failed)
	}
	sub, err := a.Harness.Store().Submission(ctx, first)
	if err != nil || sub.Status != store.SubmissionDone || sub.Answer == nil {
		t.Fatalf("submission = %+v, %v", sub, err)
	}
	message, err := rt.Message(ctx, bot, messageID(view.Messages[1]), view.Messages[1].(storedMessage).cursor)
	if err != nil || !strings.Contains(encoded(t, message), "Hi there") {
		t.Fatalf("message = %v, %v", message, err)
	}
}

func TestRuntimeFailureKeepsDiagnostics(t *testing.T) {
	ctx := context.Background()
	a, rt := runtimeFor(t, true, scriptEngine{frames: strings.Split(failFrames, "\n")})
	bot := int64(a.Root)
	id, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "hello", RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	idle(t, rt)
	view := engineView(t, rt, bot)
	if !view.Failed || view.Error == nil || *view.Error != "rate limited" {
		t.Fatalf("failed=%v error=%v", view.Failed, view.Error)
	}
	if got := encoded(t, view.Messages); !strings.Contains(got, `rate limited〔`) {
		t.Fatalf("messages = %s", got)
	}
	if got := encoded(t, view.Runs); !strings.Contains(got, `"status":"failed"`) {
		t.Fatalf("runs = %s", got)
	}
	sub, _ := a.Harness.Store().Submission(ctx, id)
	if sub.Status != store.SubmissionUnanswered {
		t.Fatalf("submission = %+v", sub)
	}
}

func TestRuntimeStopRevokesTurn(t *testing.T) {
	ctx := context.Background()
	started := make(chan *scriptSession, 2)
	hold := make(chan struct{})
	a, rt := runtimeFor(t, true, scriptEngine{frames: strings.Split(answerFrames, "\n"), hold: hold, started: started})
	bot := int64(a.Root)
	id, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "hello", RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "again", RequestID: "r2", WhenBusy: "reject"}); err == nil ||
		!strings.Contains(err.Error(), "Bot 正在工作") {
		t.Fatalf("busy submit = %v", err)
	}
	if err := rt.Stop(ctx, bot); err != nil {
		t.Fatal(err)
	}
	idle(t, rt)
	view := engineView(t, rt, bot)
	if view.Busy || view.Failed || view.Error != nil {
		t.Fatalf("busy=%v failed=%v error=%v", view.Busy, view.Failed, view.Error)
	}
	if got := encoded(t, view.Runs); !strings.Contains(got, `"status":"stopped"`) {
		t.Fatalf("runs = %s", got)
	}
	sub, _ := a.Harness.Store().Submission(ctx, id)
	if sub.Status != store.SubmissionUnanswered {
		t.Fatalf("submission = %+v", sub)
	}
	close(hold)
	if _, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "next", RequestID: "r3"}); err != nil {
		t.Fatalf("submit after stop = %v", err)
	}
	idle(t, rt)
}

func TestRuntimeSteersIntoRunningTurn(t *testing.T) {
	ctx := context.Background()
	started := make(chan *scriptSession, 2)
	hold := make(chan struct{})
	a, rt := runtimeFor(t, true, scriptEngine{frames: strings.Split(answerFrames, "\n"), hold: hold, started: started})
	bot := int64(a.Root)
	if _, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "hello", RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	session := <-started
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		doc, _, _ := Snapshot(ctx, a.Harness, EngineDoc, bot)
		if doc.Dispatch == "accepted" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	second, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "also this", RequestID: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		session.mu.Lock()
		n := len(session.steered)
		session.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(hold)
	idle(t, rt)
	sub, _ := a.Harness.Store().Submission(ctx, second)
	if sub.Status != store.SubmissionDone {
		t.Fatalf("steered submission = %+v, steered %v", sub, session.steered)
	}
	if len(started) != 0 {
		t.Fatal("steered input started a second turn")
	}
}

func TestChatRuntimeStoresModelMessages(t *testing.T) {
	ctx := context.Background()
	a, rt := runtimeFor(t, false, scriptEngine{frames: strings.Split(failFrames, "\n")})
	bot := int64(a.Root)
	if _, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "hello", RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	idle(t, rt)
	view, err := a.Harness.ReadChat(ctx, bot, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := encoded(t, view); !view.Failed || !strings.Contains(got, "回复失败，原因待确认〔") || view.Busy {
		t.Fatalf("view = %s", got)
	}
}

func TestRuntimeNeverReplaysTurnAfterRestart(t *testing.T) {
	ctx := context.Background()
	started := make(chan *scriptSession, 1)
	a, before := runtimeFor(t, true, scriptEngine{frames: strings.Split(answerFrames, "\n"), hold: make(chan struct{}), started: started})
	bot := int64(a.Root)
	first, err := before.Submit(ctx, SendInput{BotID: bot, Text: "hello", RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	accepted(t, a, bot)
	after := NewRuntime(a, RuntimeOptions{Engine: scriptEngine{frames: strings.Split(answerFrames, "\n")}, Native: true})
	if err := after.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	idle(t, after)
	sub, _ := a.Harness.Store().Submission(ctx, first)
	if sub.Status != store.SubmissionUnanswered {
		t.Fatalf("interrupted submission = %+v", sub)
	}
	if view := engineView(t, after, bot); view.Busy {
		t.Fatal("recovered Bot stays busy")
	}
	if _, err := after.Submit(ctx, SendInput{BotID: bot, Text: "again", RequestID: "r2"}); err != nil {
		t.Fatal(err)
	}
	idle(t, after)
	view := engineView(t, after, bot)
	if got := encoded(t, view.Runs); !strings.Contains(got, `"status":"uncertain"`) || !strings.Contains(got, `"status":"ended"`) {
		t.Fatalf("runs = %s", got)
	}
}

func accepted(t *testing.T, a *Agent, bot int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if doc, _, _ := Snapshot(context.Background(), a.Harness, EngineDoc, bot); doc.Dispatch == "accepted" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("turn was never accepted")
}

func TestRuntimeRejectsEmptyAndUnsupportedInput(t *testing.T) {
	ctx := context.Background()
	a, rt := runtimeFor(t, true, scriptEngine{frames: strings.Split(answerFrames, "\n")})
	bot := int64(a.Root)
	for _, in := range []SendInput{
		{BotID: bot, Text: " \n\t\u00a0", RequestID: "r1"},
		{BotID: bot, Text: "hi", RequestID: "r2", Skills: []json.RawMessage{json.RawMessage(`"skill"`)}},
		{BotID: bot, Text: "hi", RequestID: "r3", Context: []json.RawMessage{json.RawMessage(`{}`)}},
	} {
		var refused *WorkError
		if _, err := rt.Submit(ctx, in); !errors.As(err, &refused) || refused.Code != "BAD_REQUEST" {
			t.Fatalf("submit %q = %v", in.RequestID, err)
		}
	}
	page, err := a.Harness.Store().Submissions(ctx, store.SubmissionQuery{ConversationID: store.ConversationID(bot)}, store.Scan{Limit: 8})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("submissions = %+v, %v", page.Items, err)
	}
}

func TestRuntimeStopNeverDispatchesQueuedInput(t *testing.T) {
	ctx := context.Background()
	started := make(chan *scriptSession, 2)
	hold := make(chan struct{})
	a, rt := runtimeFor(t, true, scriptEngine{frames: strings.Split(answerFrames, "\n"), hold: hold, started: started})
	bot := int64(a.Root)
	if _, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "hello", RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	<-started
	queued, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "later", RequestID: "r2", WhenBusy: "followUp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Stop(ctx, bot); err != nil {
		t.Fatal(err)
	}
	close(hold)
	idle(t, rt)
	if len(started) != 0 {
		t.Fatal("a stopped Bot started its queued input")
	}
	sub, _ := a.Harness.Store().Submission(ctx, queued)
	if sub.Status != store.SubmissionUnanswered {
		t.Fatalf("queued submission = %+v", sub)
	}
}

func TestRuntimeNeverResendsUnconfirmedSteer(t *testing.T) {
	ctx := context.Background()
	started := make(chan *scriptSession, 2)
	hold := make(chan struct{})
	a, rt := runtimeFor(t, true, scriptEngine{frames: strings.Split(answerFrames, "\n"), hold: hold, started: started,
		steerErr: errors.New("write failed")})
	bot := int64(a.Root)
	if _, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "hello", RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	session := <-started
	accepted(t, a, bot)
	second, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "also this", RequestID: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "and this", RequestID: "r3"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		n := len(session.steered)
		session.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(hold)
	idle(t, rt)
	session.mu.Lock()
	steered := slices.Clone(session.steered)
	session.mu.Unlock()
	if len(steered) != 1 || steered[0] != "also this" {
		t.Fatalf("steered = %q", steered)
	}
	sub, _ := a.Harness.Store().Submission(ctx, second)
	if sub.Status != store.SubmissionUnanswered || sub.Reason != followUnconfirmed {
		t.Fatalf("unconfirmed submission = %+v", sub)
	}
	if n := len(started); n != 1 {
		t.Fatalf("%d more turns started; want only the deferred input", n)
	}
	if prompted := <-started; len(prompted.steered) != 0 {
		t.Fatalf("deferred turn steered %q", prompted.steered)
	}
}

func TestRuntimeSessionGetsInstructionsAndDeniesApprovals(t *testing.T) {
	ctx := context.Background()
	confirm := `{"type":"extension_ui_request","id":"ui-1","method":"confirm","title":"Computer operation","message":"{\"command\":\"rm -rf /\"}"}`
	started := make(chan *scriptSession, 1)
	a, rt := runtimeFor(t, true, scriptEngine{frames: append([]string{confirm}, strings.Split(answerFrames, "\n")...), started: started})
	bot := int64(a.Root)
	if _, err := a.Bots.Update(ctx, bot, UpdateBot{Name: "Persona", Instructions: "Answer in French.", RequestID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Submit(ctx, SendInput{BotID: bot, Text: "hello", RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	session := <-started
	idle(t, rt)
	if session.options.SystemPrompt != "Answer in French." || session.options.Env["PERSONA_PI_MCP_URL"] == "" {
		t.Fatalf("options = %+v", session.options)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		answers := slices.Clone(session.answers)
		session.mu.Unlock()
		if len(answers) == 1 {
			if answers[0] != `ui-1 {"cancelled":true}` {
				t.Fatalf("answers = %q", answers)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("approval request was never answered")
}
