package server

import (
	"context"
	"encoding/json"

	"github.com/Personed/Persona/internal/approval"
	"github.com/Personed/Persona/internal/computers"
	"github.com/Personed/Persona/internal/rpc"
)

// RegisterComputer registers the computer.* procedures, the Go port of
// apps/api/src/computer.ts backed by the computers catalog.
func RegisterComputer(s *rpc.Server, c *computers.Catalog) {
	s.Handle("computer.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return c.List(ctx)
	})
	s.Handle("computer.get", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID int64 `json:"botId"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Binding(ctx, in.BotID)
	})
	s.Handle("computer.assign", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID int64 `json:"botId"`
			computers.AssignInput
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Assign(ctx, in.BotID, in.AssignInput)
	})
	s.Handle("computer.approval", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID      int64  `json:"botId"`
			ComputerID string `json:"computerId"`
			Revision   int64  `json:"revision"`
			Mode       string `json:"mode"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Approval(ctx, in.BotID, in.ComputerID, in.Revision, in.Mode)
	})
	s.Handle("computer.add", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in computers.AddInput
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Add(ctx, in)
	})
	s.Handle("computer.authorize", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			ID         string `json:"id"`
			Authorized bool   `json:"authorized"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Authorize(ctx, in.ID, in.Authorized)
	})
	s.Handle("computer.default", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			ID *string `json:"id"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.SetDefault(ctx, in.ID)
	})
	s.Handle("computer.manage", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			ID     string `json:"id"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Manage(ctx, in.ID, in.Action)
	})
	s.Handle("computer.service", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID   int64  `json:"botId"`
			Service string `json:"service"`
			Enabled bool   `json:"enabled"`
			Install bool   `json:"install"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.ConfigureService(ctx, in.BotID, in.Service, in.Enabled, in.Install)
	})
	s.Handle("computer.services", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID int64 `json:"botId"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Services(ctx, in.BotID)
	})
	s.Handle("computer.browserMode", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID          int64  `json:"botId"`
			Mode           string `json:"mode"`
			ConfirmRestart bool   `json:"confirmRestart"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.BrowserMode(ctx, in.BotID, in.Mode)
	})
	s.Handle("computer.setup", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return c.Setup(ctx)
	})
	s.Handle("computer.prepare", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID     int64  `json:"botId"`
			Mode      string `json:"mode"`
			Revision  int64  `json:"revision"`
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Prepare(ctx, in.BotID, in.Mode, in.Revision, in.RequestID)
	})
	s.Handle("computer.configure", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID     int64  `json:"botId"`
			Mode      string `json:"mode"`
			Revision  int64  `json:"revision"`
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.ConfigureMode(ctx, in.BotID, in.Mode, in.Revision, in.RequestID)
	})
	s.Handle("computer.assistant", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID int64 `json:"botId"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Assistant(ctx, in.BotID)
	})
	s.Handle("computer.assistantConfigure", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID      int64  `json:"botId"`
			ComputerID string `json:"computerId"`
			Revision   int64  `json:"revision"`
			Enabled    bool   `json:"enabled"`
			Provider   string `json:"provider"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.AssistantConfigure(ctx, in.BotID, in.ComputerID, in.Revision, in.Enabled, in.Provider)
	})
	s.Handle("computer.assistantKey", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID    int64  `json:"botId"`
			Provider string `json:"provider"`
			Key      string `json:"key"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.AssistantKey(ctx, in.BotID, in.Provider, in.Key)
	})
	s.Handle("computer.assistantTest", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID    int64  `json:"botId"`
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.AssistantTest(ctx, in.BotID, in.Provider)
	})
	s.Handle("computer.input", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID  int64           `json:"botId"`
			View   string          `json:"view"`
			Action json.RawMessage `json:"action"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return c.Input(ctx, in.BotID, in.View, in.Action)
	})
	s.HandleStream("computer.watch", func(ctx context.Context, input json.RawMessage, send func(any) error) (any, error) {
		var in struct {
			BotID   int64  `json:"botId"`
			Service string `json:"service"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		return nil, c.Watch(ctx, in.BotID, in.Service, send)
	})
}

// RegisterComputerTakeover wires the takeover card procedures the engines
// slice leaves unimplemented: engines.takeRequest grants a viewer control of
// a pending takeover card, and engines.answer settles the stored approval —
// finishing the live request before resolving it, as runtime.ts
// execution.answer does.
func RegisterComputerTakeover(s *rpc.Server, c *computers.Catalog, approvals *approval.Service) {
	s.Handle("engines.takeRequest", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID int64  `json:"botId"`
			ID    string `json:"id"`
			View  string `json:"view"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		if err := c.TakeRequest(ctx, in.BotID, in.ID, in.View, approvals); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	s.Handle("engines.answer", func(ctx context.Context, input json.RawMessage) (any, error) {
		var in struct {
			BotID    int64               `json:"botId"`
			ID       string              `json:"id"`
			Allow    bool                `json:"allow"`
			Text     string              `json:"text"`
			Answers  map[string][]string `json:"answers"`
			OptionID string              `json:"optionId"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, rpc.NewError("BAD_REQUEST", "entrada inválida")
		}
		if in.Allow {
			pending, err := approvals.Pending(ctx, in.BotID)
			if err != nil {
				return nil, err
			}
			for i := range pending {
				if pending[i].ID == in.ID && pending[i].Takeover != nil {
					if err := c.FinishRequest(ctx, in.BotID, in.ID); err != nil {
						return nil, err
					}
					break
				}
			}
		}
		if err := approvals.Answer(in.BotID, in.ID, approval.Answer{
			Allow: in.Allow, Text: in.Text, Answers: in.Answers, OptionID: in.OptionID,
		}); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
}
