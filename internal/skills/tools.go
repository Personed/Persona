package skills

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Personed/Persona/internal/tools"
)

// Register adds the skill_read product tool to a registry.
func Register(registry tools.Registry, service *Service) error {
	return registry.Register(readTool{service})
}

type readTool struct{ service *Service }

func (readTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        "skill_read",
		Description: "Read an enabled skill from the catalog by stable ID.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}
}

func (t readTool) Run(ctx context.Context, call tools.Call) (tools.Result, error) {
	var args struct {
		ID *string `json:"id"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil || args.ID == nil {
		return failure("参数无效：需要 id。"), nil
	}
	loaded, err := t.service.Read(ctx, call.BotID, *args.ID)
	if skillError := (*Error)(nil); errors.As(err, &skillError) {
		return failure(skillError.Message), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	text, err := json.Marshal(loaded)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: []tools.Content{{Type: "text", Text: string(text)}}}, nil
}

func failure(message string) tools.Result {
	return tools.Result{IsError: true, Content: []tools.Content{{Type: "text", Text: message}}}
}
