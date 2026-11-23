package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/apitest"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/store"
)

// fixtureSkips are recorded exchanges a registered procedure cannot reproduce
// yet, by "file#index", with the reason.
var fixtureSkips = map[string]string{
	// Recorded after send ran a faux turn; send is not served yet, so the chat is still empty.
	"bots-conversation#3": "needs send",
	// The recorded ID 15 follows the IDs send minted for its turn (10 to 14).
	"bots-conversation#5": "needs send",
	// Recorded with the native engine runtime, whose view adds engine, runtime and error.
	"native-conversation#0": "needs the engine runtime",
	"native-conversation#2": "needs the engine runtime",
}

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
	procedures := newRPC(t)
	RegisterAgent(procedures, a)
	h, err := New(Config{RPC: procedures, Files: AgentFiles(a.Files)})
	if err != nil {
		t.Fatal(err)
	}
	return h, procedures
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
