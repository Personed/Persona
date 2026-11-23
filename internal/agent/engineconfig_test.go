package agent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Personed/Persona/internal/engine"
	"github.com/Personed/Persona/internal/engine/pi"
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
	other := "ollama/m2"
	if err := rt.Select(ctx, bot, doc.Config, &other, nil, 1, "d"); err != nil {
		t.Fatal(err)
	}
	if doc, _, _ = Snapshot(ctx, a.Harness, EngineDoc, bot); doc.Revision != 2 || *doc.Config.Model != other {
		t.Fatalf("selected doc = %+v", doc)
	}
}

type catalogModelEngine struct{ modelEngine }

func (catalogModelEngine) Catalog(context.Context) ([]pi.CatalogModel, error) {
	return []pi.CatalogModel{{Provider: "faux", ID: "r1", Name: "推理模型", Reasoning: true, Input: []string{"text"}}}, nil
}

func TestSelectAcceptsOfferedEffort(t *testing.T) {
	ctx := context.Background()
	a := open(t)
	rt := NewRuntime(a, RuntimeOptions{Engine: catalogModelEngine{}, Native: true})
	bot := int64(a.Root)
	model := "faux/r1"
	config := EngineConfig{Provider: "pi", Model: &model}
	inspected := rt.Inspect(ctx, config)
	if len(inspected.Models) != 1 {
		t.Fatalf("models = %+v", inspected.Models)
	}
	m := inspected.Models[0]
	if m.Name != "推理模型" || m.Images == nil || *m.Images || m.Effort == nil ||
		!slices.ContainsFunc(m.Effort.Options, func(o Option) bool { return o.ID == "high" }) {
		t.Fatalf("model = %+v", m)
	}
	var work *WorkError
	if err := rt.Select(ctx, bot, config, &model, map[string]any{"thinkingLevel": "extreme"}, 0, "a"); !errors.As(err, &work) || work.Code != "BAD_REQUEST" {
		t.Fatalf("unoffered effort: %v", err)
	}
	if err := rt.Select(ctx, bot, config, &model, map[string]any{"thinkingLevel": "high"}, 0, "b"); err != nil {
		t.Fatal(err)
	}
	if doc, _, _ := Snapshot(ctx, a.Harness, EngineDoc, bot); doc.Config.Options["thinkingLevel"] != "high" {
		t.Fatalf("doc = %+v", doc)
	}
}
