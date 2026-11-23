package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/computers"
	"github.com/Personed/Persona/internal/apitest"
	"github.com/Personed/Persona/internal/engine"
	"github.com/Personed/Persona/internal/mcp"
	"github.com/Personed/Persona/internal/memory"
	"github.com/Personed/Persona/internal/routines"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/skills"
	"github.com/Personed/Persona/internal/store"
)

// fixtureSkips are recorded exchanges a registered procedure cannot reproduce
// yet, by "file#index", with the reason.
var fixtureSkips = map[string]string{}

// fauxModels is the faux provider of apps/api/test/contract.test.ts.
type fauxModels struct{}

func (fauxModels) Available(_ context.Context, provider string) ([]agent.Model, error) {
	if provider != "" && provider != "faux" {
		return nil, nil
	}
	return []agent.Model{{Provider: "faux", ID: "faux-1", Name: "Faux Model", Input: []string{"text", "image"}}}, nil
}

func (fauxModels) Providers(context.Context) ([]agent.Provider, error) {
	return []agent.Provider{{ID: "faux", Name: "faux"}}, nil
}

func (fauxModels) Configured(context.Context, string) (bool, error) { return true, nil }

func (fauxModels) Refresh(context.Context, string) error { return nil }

// recordedIDs is the last record ID the TypeScript runtime mints while it
// opens (the root conversation, its documents and extension documents), so
// the first Bot created in a fixture is ID 10.
const recordedIDs = 9

// fixtureService is setup() of contract.test.ts: a fresh store with the root
// Bot "Persona" on the faux model, or without a model for the native
// conversation, which the recorder opens with a work computer.
func fixtureService(t *testing.T, file string) (http.Handler, *rpc.Server) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	records, err := store.Open(ctx, filepath.Join(dir, "persona-go.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = records.Close() })
	opts := agent.Options{Models: fauxModels{}, FilesRoot: filepath.Join(dir, "agent.sqlite.files")}
	if file != "native-conversation" {
		opts.Agent.Model = &agent.ModelRef{Provider: "faux", ModelID: "faux-1"}
	}
	a, err := agent.Open(ctx, records, opts)
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(0); id < recordedIDs; {
		if id, err = records.MintID(ctx); err != nil {
			t.Fatal(err)
		}
	}
	answer := fakeEngine{text: "Hello from Persona.", provider: "faux", model: "faux-1"}
	if file == "native-conversation" {
		answer = fakeEngine{text: "Hello from Pi.", provider: "pi", model: "test-model", native: true}
	}
	rt := agent.NewRuntime(a, agent.RuntimeOptions{Engine: answer, Native: answer.native})
	procedures := newRPC(t)
	RegisterAgent(procedures, a, rt, computers.New(computers.ServiceConfig{DataDir: dir, Deployment: "native", Computer: computers.Config{Kind: "process"}}, a.Harness))
	RegisterComputer(procedures, computers.New(computers.ServiceConfig{DataDir: dir, Deployment: "native", Computer: computers.Config{Kind: "process"}}, a.Harness))
	RegisterEngines(procedures, a, rt)
	RegisterWork(procedures, a, rt, Work{Memory: memory.NewService(records, nil), Routines: routines.NewService(records, nil),
		Skills: skills.NewService(records, os.DirFS(filepath.Join(opts.FilesRoot, "skills")), nil)})
	RegisterMCP(procedures, a, mcp.NewService(records, nil, nil), "")
	h, err := New(Config{RPC: procedures, Files: AgentFiles(a.Files)})
	if err != nil {
		t.Fatal(err)
	}
	// contract.test.ts waits for the turn send starts before the next request.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if r.URL.Path == "/rpc/send" {
			if err := rt.Idle(r.Context()); err != nil {
				t.Error(err)
			}
		}
	}), procedures
}

// fakeEngine answers every prompt with one recorded Pi turn.
type fakeEngine struct {
	text, provider, model string
	native                bool
}

func (fakeEngine) Info(context.Context) (engine.Info, error)      { return engine.Info{}, nil }
func (fakeEngine) Models(context.Context) ([]engine.Model, error) { return nil, nil }

func (e fakeEngine) Start(context.Context, engine.SessionOptions) (engine.Session, error) {
	return &fakeSession{e: e, events: make(chan engine.Event, 16)}, nil
}

type fakeSession struct {
	e      fakeEngine
	events chan engine.Event
	close  sync.Once
}

func (s *fakeSession) frame(v map[string]any) {
	data, _ := json.Marshal(v)
	s.events <- engine.Event{Type: "frame", Data: data}
}

func (s *fakeSession) ID() string { return "/native/session" }

func (s *fakeSession) Prompt(context.Context, engine.Prompt) error {
	message := map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": s.e.text}},
		"provider": s.e.provider, "model": s.e.model, "stopReason": "stop", "timestamp": 100,
		"usage": map[string]any{"input": 1, "output": 1, "cacheRead": 0, "cacheWrite": 0, "cost": map[string]any{"total": 0}}}
	if !s.e.native {
		message["timestamp"] = time.Now().UnixMilli()
	}
	s.frame(map[string]any{"type": "message_start", "message": message})
	s.frame(map[string]any{"type": "message_end", "message": message})
	s.frame(map[string]any{"type": "agent_settled"})
	return nil
}

func (s *fakeSession) Checkpoint(context.Context) (string, error) { return "previous", nil }
func (s *fakeSession) SessionStats(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (s *fakeSession) Steer(context.Context, engine.Prompt) error { return nil }
func (s *fakeSession) Abort(context.Context) error                { return nil }
func (s *fakeSession) Answer(context.Context, string, json.RawMessage) error {
	return nil
}
func (s *fakeSession) SetModel(context.Context, engine.Model, string) error { return nil }
func (s *fakeSession) Compact(context.Context, string) error                { return nil }
func (s *fakeSession) State(context.Context) (engine.State, error)          { return engine.State{}, nil }
func (s *fakeSession) Usage(context.Context) (engine.Usage, error)          { return engine.Usage{}, nil }
func (s *fakeSession) Messages(context.Context, int, int) ([]json.RawMessage, error) {
	return nil, nil
}
func (s *fakeSession) Events() <-chan engine.Event { return s.events }
func (s *fakeSession) Close() error {
	s.close.Do(func() { close(s.events) })
	return nil
}

// TestContractFixtures replays contracts/api against the Go service. Every
// exchange of a registered procedure and every validation exchange must
// match; unregistered procedures answer NOT_IMPLEMENTED.
func TestContractFixtures(t *testing.T) {
	servers := map[string]*rpc.Server{}
	results, err := apitest.Replay(filepath.Join("..", "..", "contracts", "api"), func(file string) http.Handler {
		h, procedures := fixtureService(t, file)
		servers[file] = procedures
		return h
	})
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, r := range results {
		key := fmt.Sprintf("%s#%d", r.File, r.Index)
		_, skipped := fixtureSkips[key]
		switch {
		case r.Match() && skipped:
			t.Errorf("%s matches; remove its skip", key)
		case r.Match():
			matched++
		case strings.Contains(r.WantBody, `"Input validation failed"`):
			t.Errorf("validation: %s", r)
		case skipped:
		case servers[r.File].Registered(r.Procedure):
			t.Errorf("registered procedure: %s", r)
		case r.Status != http.StatusNotImplemented || !strings.Contains(r.Body, `"NOT_IMPLEMENTED"`):
			t.Errorf("unregistered procedure: %s", r)
		}
	}
	t.Logf("%d of %d fixture exchanges match, %d skipped", matched, len(results), len(fixtureSkips))
}
