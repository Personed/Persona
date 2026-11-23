package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/rpc"
)

type botTarget struct {
	BotID int64 `json:"botId"`
}

// RegisterAgent registers the procedures the Bot domain serves, as
// apps/api/src/router.ts does. With a runtime it also serves chat input (send,
// stop, changes, subscribe) and full message reads; engine selection stays
// unregistered until engine controls are composed.
func RegisterAgent(s *rpc.Server, a *agent.Agent, rt *agent.Runtime) {
	r := agentRoutes{a: a, rt: rt}
	if rt != nil {
		s.Handle("send", r.send)
		s.Handle("stop", r.stop)
		s.HandleStream("subscribe", r.subscribe)
		s.HandleStream("changes", r.changes)
	}
	s.Handle("bots.list", func(ctx context.Context, _ json.RawMessage) (any, error) { return wrap(a.Bots.List(ctx)) })
	s.Handle("bots.create", r.createBot)
	s.Handle("bots.completeWelcome", r.completeWelcome)
	s.HandleStream("bots.subscribe", r.subscribeBots)
	s.Handle("get", r.get)
	s.Handle("message", r.message)
	s.Handle("files.upload", r.upload)
	s.Handle("files.discard", r.discard)
	s.Handle("files.download", r.download)
	s.Handle("models.list", func(ctx context.Context, _ json.RawMessage) (any, error) { return agent.ModelCatalog(ctx, a.Models) })
	s.Handle("models.providers", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return agent.ModelProviders(ctx, a.Models)
	})
	s.Handle("models.visibility", func(ctx context.Context, _ json.RawMessage) (any, error) { return a.Bots.ModelVisibility(ctx) })
	s.Handle("models.setVisibility", r.setVisibility)
	s.Handle("engines.catalog", func(context.Context, json.RawMessage) (any, error) { return agent.EngineCatalog(), nil })
	if rt != nil && rt.Native() {
		s.Handle("engines.get", r.engineProfile)
	} else {
		s.Handle("engines.get", r.withoutEngine)
	}
	s.Handle("work.profile", r.profile)
	s.Handle("work.update", r.update)
	s.Handle("mcp.catalog", func(context.Context, json.RawMessage) (any, error) { return agent.MCPApps(), nil })
	s.Handle("computer.list", func(context.Context, json.RawMessage) (any, error) {
		return nil, rpc.NewError("SERVICE_UNAVAILABLE", "电脑接入尚未配置。")
	})
}

type agentRoutes struct {
	a  *agent.Agent
	rt *agent.Runtime
}

// wrap converts a WorkError into its oRPC error, as the TypeScript routers do.
func wrap[T any](value T, err error) (any, error) {
	if err != nil {
		return nil, rpcError(err)
	}
	return value, nil
}

func rpcError(err error) error {
	var work *agent.WorkError
	if errors.As(err, &work) {
		return rpc.NewError(work.Code, work.Message)
	}
	return err
}

// bot decodes the botId of input and answers NOT_FOUND unless it is a Bot, as botConversation.
func (r agentRoutes) bot(ctx context.Context, input json.RawMessage) (int64, error) {
	var target botTarget
	if err := json.Unmarshal(input, &target); err != nil {
		return 0, err
	}
	ok, err := r.a.Bots.Exists(ctx, target.BotID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, rpc.NewError("NOT_FOUND", "")
	}
	return target.BotID, nil
}

func (r agentRoutes) createBot(ctx context.Context, input json.RawMessage) (any, error) {
	var create agent.CreateBot
	if err := json.Unmarshal(input, &create); err != nil {
		return nil, err
	}
	if create.Model != nil {
		_, ok, err := agent.ConfiguredModel(ctx, r.a.Models, create.Model)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, rpc.NewError("PRECONDITION_FAILED", "")
		}
	}
	return wrap(r.a.Bots.Create(ctx, create))
}

func (r agentRoutes) completeWelcome(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := r.a.Bots.CompleteWelcome(ctx, id); err != nil {
		return nil, rpcError(err)
	}
	return rpc.Void, nil
}

func (r agentRoutes) subscribeBots(ctx context.Context, _ json.RawMessage, send func(any) error) (any, error) {
	commits, err := r.a.Harness.Watch(ctx)
	if err != nil {
		return nil, err
	}
	return rpc.WatchValues(ctx, commits, func(ctx context.Context) (any, error) {
		items, err := r.a.Bots.List(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items, "computers": []any{}, "defaultComputerId": nil, "bindings": map[string]any{}}, nil
	}, send)
}

func (r agentRoutes) get(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var page struct {
		Before string `json:"before"`
	}
	if err := json.Unmarshal(input, &page); err != nil {
		return nil, err
	}
	if r.rt == nil {
		return r.a.Harness.ReadChat(ctx, id, page.Before)
	}
	view, err := r.rt.View(ctx, id, page.Before)
	if err != nil || !r.rt.Native() {
		return wrap(view, err)
	}
	return agent.Slim(view)
}

// withoutEngine answers procedures that need the native engine runtime.
func (r agentRoutes) withoutEngine(ctx context.Context, input json.RawMessage) (any, error) {
	if _, err := r.bot(ctx, input); err != nil {
		return nil, err
	}
	return nil, rpc.NewError("NOT_IMPLEMENTED", "")
}

func (r agentRoutes) message(ctx context.Context, input json.RawMessage) (any, error) {
	if r.rt == nil {
		return r.withoutEngine(ctx, input)
	}
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var target struct {
		ID     string `json:"id"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(input, &target); err != nil {
		return nil, err
	}
	return wrap(r.rt.Message(ctx, id, target.ID, target.Cursor))
}

func (r agentRoutes) send(ctx context.Context, input json.RawMessage) (any, error) {
	if _, err := r.bot(ctx, input); err != nil {
		return nil, err
	}
	var in agent.SendInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	id, err := r.rt.Submit(ctx, in)
	if err != nil {
		return nil, rpcError(err)
	}
	return map[string]any{"id": id}, nil
}

func (r agentRoutes) stop(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := r.rt.Stop(ctx, id); err != nil {
		return nil, rpcError(err)
	}
	return map[string]bool{"ok": true}, nil
}

func (r agentRoutes) subscribe(ctx context.Context, input json.RawMessage, send func(any) error) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	commits, err := r.a.Harness.Watch(ctx)
	if err != nil {
		return nil, err
	}
	return rpc.WatchValues(ctx, commits, func(ctx context.Context) (any, error) { return wrap(r.rt.View(ctx, id, "")) }, send)
}

func (r agentRoutes) changes(ctx context.Context, input json.RawMessage, send func(any) error) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	return nil, rpcError(r.rt.Updates(ctx, id, send))
}

func (r agentRoutes) upload(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var upload struct {
		File      rpc.Blob `json:"file"`
		RequestID string   `json:"requestId"`
	}
	if err := json.Unmarshal(input, &upload); err != nil {
		return nil, err
	}
	var file agent.Attachment
	err = r.a.Files.Exclusive(id, func() error {
		var err error
		file, err = r.a.Files.Upload(id, upload.File.Name, upload.File.Data, upload.RequestID)
		return err
	})
	if err != nil {
		var work *agent.WorkError
		if errors.As(err, &work) {
			return nil, rpc.NewError("BAD_REQUEST", work.Message)
		}
		return nil, rpc.NewError("BAD_REQUEST", upload.File.Name+"：文件未送达工作电脑。请检查电脑连接后重试。")
	}
	return file, nil
}

func (r agentRoutes) discard(ctx context.Context, input json.RawMessage) (any, error) {
	botID, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var target struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(input, &target); err != nil {
		return nil, err
	}
	err = r.a.Files.Exclusive(botID, func() error {
		chat, err := r.a.Harness.ReadHistory(ctx, botID)
		if err != nil {
			return err
		}
		if slices.Contains(chat.Attachments(), target.ID) {
			return nil
		}
		return r.a.Files.Discard(botID, target.ID)
	})
	if err != nil {
		return nil, err
	}
	return rpc.Void, nil
}

func (r agentRoutes) download(ctx context.Context, input json.RawMessage) (any, error) {
	botID, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var target struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(input, &target); err != nil {
		return nil, err
	}
	file, data, err := r.a.Files.Download(botID, target.ID)
	if err != nil {
		return nil, rpc.NewError("NOT_FOUND", "")
	}
	return &rpc.Blob{Name: file.Name, Type: file.MimeType, Data: data}, nil
}

func (r agentRoutes) setVisibility(ctx context.Context, input json.RawMessage) (any, error) {
	var visibility agent.Visibility
	if err := json.Unmarshal(input, &visibility); err != nil {
		return nil, err
	}
	return r.a.Bots.SetModelVisibility(ctx, visibility)
}

func (r agentRoutes) profile(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	return wrap(r.a.Bots.Profile(ctx, id))
}

func (r agentRoutes) update(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var update agent.UpdateBot
	if err := json.Unmarshal(input, &update); err != nil {
		return nil, err
	}
	return wrap(r.a.Bots.Update(ctx, id, update))
}

// AgentFiles serves stored Bot files on /files/<botId>/<id>.
func AgentFiles(files *agent.Files) Files { return agentFiles{files} }

type agentFiles struct{ files *agent.Files }

func (f agentFiles) Open(_ context.Context, botID int64, id string) (File, error) {
	file, data, err := f.files.Download(botID, id)
	return File{Type: file.MimeType, Data: data}, err
}
