package computers

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/tools"
	"github.com/Personed/Persona/internal/worker"
)

// nativeServices are the computer services whose native tools Bots may call.
var nativeServices = []string{"browser", "desktop"}

// exposedName names a native service tool as the TypeScript computer tools do:
// browser tools are prefixed, desktop tools keep their native names.
func exposedName(service, name string) string {
	if service == "desktop" {
		return name
	}
	return service + "_" + name
}

// nativeTools exposes each enabled computer service's native catalog through
// the shared tools.Registry that serves Pi's MCP gateway, as the TypeScript
// computer.tools registry does. Calls run through the worker's journaled
// computer call path and non-observation calls wait on the shared approver.
type nativeTools struct {
	base tools.Registry
	c    *Catalog
}

// NativeTools wraps base so Pi sees both product tools and the Bot's enabled
// computer service tools.
func NativeTools(base tools.Registry, c *Catalog) tools.Registry {
	return &nativeTools{base: base, c: c}
}

func (r *nativeTools) Register(tool tools.Tool) error { return r.base.Register(tool) }

// catalog lists a service's exposed tools; a Bot without the enabled service
// gets none.
func (r *nativeTools) catalog(ctx context.Context, botID int64, service string) ([]tools.Definition, error) {
	w, dir, err := r.c.svcs.workerFor(ctx, botID, service)
	if err != nil {
		var rpcErr *rpc.Error
		if errors.As(err, &rpcErr) {
			return nil, nil
		}
		return nil, err
	}
	native, err := w.ComputerCatalog(ctx, dir, service)
	if err != nil {
		return nil, err
	}
	definitions := make([]tools.Definition, 0, len(native))
	for _, tool := range native {
		if tool == nil || (service == "browser" && tool.Name == "agent_browser_wait_ms") {
			continue
		}
		definition := tools.Definition{
			Name:        exposedName(service, tool.Name),
			Description: tool.Description,
			Mutates:     !worker.Observation(service, tool.Name),
			// Computer calls respect the binding's "auto" approval mode.
			Automatic: true,
		}
		if tool.InputSchema != nil {
			if schema, err := json.Marshal(tool.InputSchema); err == nil {
				definition.InputSchema = schema
			}
		}
		if tool.OutputSchema != nil {
			if schema, err := json.Marshal(tool.OutputSchema); err == nil {
				definition.OutputSchema = schema
			}
		}
		if tool.Annotations != nil {
			if annotations, err := json.Marshal(tool.Annotations); err == nil {
				_ = json.Unmarshal(annotations, &definition.Annotations)
			}
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func (r *nativeTools) List(ctx context.Context, botID int64) ([]tools.Definition, error) {
	definitions, err := r.base.List(ctx, botID)
	if err != nil {
		return nil, err
	}
	for _, service := range nativeServices {
		native, err := r.catalog(ctx, botID, service)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, native...)
	}
	return definitions, nil
}

func (r *nativeTools) Call(ctx context.Context, call tools.Call, approve tools.Approver) (tools.Result, error) {
	for _, service := range nativeServices {
		native, err := r.catalog(ctx, call.BotID, service)
		if err != nil {
			return tools.Result{}, err
		}
		for _, definition := range native {
			if definition.Name != call.Name {
				continue
			}
			return r.callService(ctx, service, call, definition, approve)
		}
	}
	return r.base.Call(ctx, call, approve)
}

func (r *nativeTools) callService(ctx context.Context, service string, call tools.Call, definition tools.Definition, approve tools.Approver) (tools.Result, error) {
	if definition.Mutates {
		if approve == nil {
			return tools.Result{}, tools.ErrDenied
		}
		allowed, err := approve(ctx, call, definition)
		if err != nil {
			return tools.Result{}, err
		}
		if !allowed {
			return tools.Result{}, tools.ErrDenied
		}
	}
	w, dir, err := r.c.svcs.workerFor(ctx, call.BotID, service)
	if err != nil {
		return tools.Result{}, err
	}
	name := definition.Name
	if service != "desktop" {
		name = name[len(service)+1:]
	}
	result, err := w.ComputerRequest(ctx, worker.ComputerInput{
		Action: "call", Service: service, Dir: dir,
		TaskID: call.TaskID, CallID: call.CallID,
		Name: name, Arguments: call.Input,
	})
	if err != nil {
		return tools.Result{}, err
	}
	var raw struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
			Resource *struct {
				URI      string `json:"uri"`
				MimeType string `json:"mimeType"`
				Text     string `json:"text"`
				Data     string `json:"blob"`
			} `json:"resource"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if len(result.Result) > 0 {
		if err := json.Unmarshal(result.Result, &raw); err != nil {
			return tools.Result{}, err
		}
	}
	out := tools.Result{Structured: raw.Structured, IsError: raw.IsError}
	for _, content := range raw.Content {
		block := tools.Content{
			Type: content.Type, Text: content.Text,
			MimeType: content.MimeType, Data: content.Data,
		}
		if content.Resource != nil {
			block.URI = content.Resource.URI
			block.MimeType = content.Resource.MimeType
			block.Text = content.Resource.Text
			block.Data = content.Resource.Data
		}
		out.Content = append(out.Content, block)
	}
	return out, nil
}
