package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Personed/Persona/internal/tools"
)

// Register adds the file delivery tool to the registry.
func Register(registry tools.Registry, service *Service) error {
	return registry.Register(attachFile{service})
}

type attachFile struct{ service *Service }

func (attachFile) Definition() tools.Definition {
	return tools.Definition{
		Name:        "attach_file",
		Description: "Deliver a requested file to the user. Provide a workspace path or the exact saved screenshot id. Evidence archive paths are not workspace files. Routine screenshot evidence is already saved; attach it only when the user requests image delivery.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"id":{"type":"string"}}}`),
	}
}

func (t attachFile) Run(ctx context.Context, call tools.Call) (tools.Result, error) {
	file, err := t.attach(ctx, call)
	if failure := (*Error)(nil); errors.As(err, &failure) {
		return tools.Result{IsError: true, Content: []tools.Content{{Type: "text", Text: failure.Message}}}, nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: []tools.Content{{Type: "text", Text: fmt.Sprintf("Attached %s (%d bytes)", file.Name, file.Size)}}}, nil
}

func (t attachFile) attach(ctx context.Context, call tools.Call) (Attachment, error) {
	var args struct {
		Path *string `json:"path"`
		ID   *string `json:"id"`
	}
	if len(call.Input) != 0 {
		if err := json.Unmarshal(call.Input, &args); err != nil {
			return Attachment{}, &Error{"Invalid arguments: " + err.Error()}
		}
	}
	var file Attachment
	var err error
	switch {
	case args.ID != nil && args.Path == nil:
		if file, err = t.service.Metadata(call.BotID, *args.ID); err == nil {
			err = t.service.verify(ctx, call.BotID, file)
		}
	case args.Path != nil && args.ID == nil:
		file, err = t.service.Publish(ctx, call.BotID, *args.Path)
	default:
		err = &Error{"Provide either a workspace path or a saved screenshot id."}
	}
	if err != nil {
		return Attachment{}, err
	}
	return file, t.service.Deliver(ctx, call, Message{Attachments: []Attachment{file}})
}
