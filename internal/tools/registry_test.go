package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Personed/Persona/internal/tools"
)

type stubTool struct {
	definition tools.Definition
	runs       int
}

func (s *stubTool) Definition() tools.Definition { return s.definition }

func (s *stubTool) Run(context.Context, tools.Call) (tools.Result, error) {
	s.runs++
	return tools.Result{Content: []tools.Content{{Type: "text", Text: "ok"}}}, nil
}

func TestRegistryListsInOrderAndRejectsDuplicates(t *testing.T) {
	registry := tools.NewRegistry()
	for _, name := range []string{"b", "a"} {
		if err := registry.Register(&stubTool{definition: tools.Definition{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Register(&stubTool{definition: tools.Definition{Name: "a"}}); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
	definitions, err := registry.List(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 || definitions[0].Name != "b" || definitions[1].Name != "a" {
		t.Fatalf("List = %+v", definitions)
	}
}

func TestRegistryCallAppliesApproval(t *testing.T) {
	ctx := context.Background()
	registry := tools.NewRegistry()
	read := &stubTool{definition: tools.Definition{Name: "read"}}
	write := &stubTool{definition: tools.Definition{Name: "write", Mutates: true}}
	for _, tool := range []tools.Tool{read, write} {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := registry.Call(ctx, tools.Call{Name: "missing"}, nil); !errors.Is(err, tools.ErrUnknownTool) {
		t.Fatalf("unknown tool error = %v", err)
	}
	if _, err := registry.Call(ctx, tools.Call{Name: "read"}, nil); err != nil || read.runs != 1 {
		t.Fatalf("read call: err=%v runs=%d", err, read.runs)
	}
	if _, err := registry.Call(ctx, tools.Call{Name: "write"}, nil); !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("write without approver = %v", err)
	}
	deny := func(context.Context, tools.Call, tools.Definition) (bool, error) { return false, nil }
	if _, err := registry.Call(ctx, tools.Call{Name: "write"}, deny); !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("denied write = %v", err)
	}
	allow := func(_ context.Context, call tools.Call, definition tools.Definition) (bool, error) {
		return call.Name == definition.Name, nil
	}
	if _, err := registry.Call(ctx, tools.Call{Name: "write"}, allow); err != nil || write.runs != 1 {
		t.Fatalf("approved write: err=%v runs=%d", err, write.runs)
	}
}
