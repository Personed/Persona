package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/Personed/Persona/internal/engine"
)

type modelEngine struct{ scriptEngine }

func (modelEngine) Info(context.Context) (engine.Info, error) {
	return engine.Info{ID: "pi", Version: "1.0.0"}, nil
}

func (modelEngine) Models(context.Context) ([]engine.Model, error) {
	return []engine.Model{{Provider: "ollama", ModelID: "m1"}, {Provider: "ollama", ModelID: "m2"}}, nil
}

func TestConfigurePreparesInspectedModel(t *testing.T) {
	ctx := context.Background()
	a := open(t)
	rt := NewRuntime(a, RuntimeOptions{Engine: modelEngine{}, Native: true})
	bot := int64(a.Root)
	model := "ollama/m1"
	if err := rt.Configure(ctx, bot, EngineChange{Config: EngineConfig{Provider: "pi", Model: &model}, RequestID: "a"}); err != nil {
		t.Fatal(err)
	}
	doc, _, err := Snapshot(ctx, a.Harness, EngineDoc, bot)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Revision != 1 || !doc.Prepared || *doc.Config.Model != model {
		t.Fatalf("doc = %+v", doc)
	}
	if inspected, ok := rt.Inspection("pi"); !ok || len(inspected.Models) != 2 || !inspected.Authenticated {
		t.Fatalf("inspection = %+v", inspected)
	}
	var work *WorkError
	if err := rt.Configure(ctx, bot, EngineChange{Config: EngineConfig{Provider: "pi", Model: &model}, RequestID: "b"}); !errors.As(err, &work) || work.Code != "CONFLICT" {
		t.Fatalf("stale revision: %v", err)
	}
	missing := "ollama/none"
	if err := rt.Select(ctx, bot, doc.Config, &missing, nil, 1, "c"); !errors.As(err, &work) || work.Code != "BAD_REQUEST" {
		t.Fatalf("unknown model: %v", err)
	}
	session := "native-1"
	if err := a.Harness.Commit(ctx, func(tx *Tx) error {
		doc, err := Edit(tx, EngineDoc, bot)
		doc.SessionID = &session
		return err
	}); err != nil {
		t.Fatal(err)
	}
	other := "ollama/m2"
	if err := rt.Select(ctx, bot, doc.Config, &other, nil, 1, "d"); err != nil {
		t.Fatal(err)
	}
	if doc, _, _ = Snapshot(ctx, a.Harness, EngineDoc, bot); doc.Revision != 2 || *doc.Config.Model != other {
		t.Fatalf("selected doc = %+v", doc)
	}
	if doc.SessionID == nil || *doc.SessionID != session {
		t.Fatalf("a model change dropped the native session: %v", doc.SessionID)
	}
	restarted := NewRuntime(a, RuntimeOptions{Engine: modelEngine{}, Native: true})
	if models, version, err := restarted.Catalog(ctx, bot, "pi"); err != nil || len(models) != 2 || version == nil || *version != "1.0.0" {
		t.Fatalf("saved catalog = %v %v %v", models, version, err)
	}
	if models, _, err := restarted.Catalog(ctx, bot+1, "pi"); err != nil || len(models) != 0 {
		t.Fatalf("another Bot's catalog = %v %v", models, err)
	}
}
