package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/rpc"
)

// refreshTimeout bounds models.refresh, as the TypeScript router does.
const refreshTimeout = 15 * time.Second

// engineRuntimeProcedures need the native engine runtime's connections and
// controls. Until they are composed they answer as apps/api/src/engines.ts
// does without a runtime: NOT_FOUND for a missing Bot, else NOT_IMPLEMENTED.
var engineRuntimeProcedures = []string{
	"engines.answer", "engines.answerLogin", "engines.available", "engines.cancelLogin", "engines.compact",
	"engines.configure", "engines.controls", "engines.inspect", "engines.install", "engines.login",
	"engines.loginState", "engines.models", "engines.select", "engines.takeRequest",
}

// RegisterEngines registers the model and engine procedures that
// RegisterAgent does not, as models.ts and engines.ts in apps/api/src.
// models.select and models.thinking configure the Bot's pi.agent document;
// with a native runtime they select through engine controls instead, so they
// stay unregistered there.
func RegisterEngines(s *rpc.Server, a *agent.Agent, rt *agent.Runtime) {
	r := agentRoutes{a: a, rt: rt}
	native := rt != nil && rt.Native()
	if !native {
		s.Handle("models.select", r.selectModel)
		s.Handle("models.thinking", r.thinking)
	}
	s.Handle("models.refresh", r.refresh)
	for _, path := range engineRuntimeProcedures {
		s.Handle(path, r.withoutEngine)
	}
	if rt != nil && rt.Native() {
		s.Handle("engines.inspect", r.inspect)
		s.Handle("engines.configure", r.configure)
		s.Handle("engines.select", r.selectEngineModel)
	}
	s.Handle("engines.usage", func(ctx context.Context, input json.RawMessage) (any, error) {
		if !native {
			return []any{}, nil
		}
		return r.usage(ctx, input)
	})
}

// engineProfile is profile in packages/agent/src/runtime before any engine
// connection is inspected: without a computer runtime no Bot has a bound work
// computer, so the catalog has no versions or models and there is no
// connection.
func (r agentRoutes) engineProfile(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	return r.profileOf(ctx, id)
}

// engineConfig is the Bot's engine configuration, or the initial Pi
// configuration with its pi.agent model before the first engine change.
func (r agentRoutes) engineConfig(ctx context.Context, id int64) (agent.EngineState, agent.EngineConfig, error) {
	doc, ok, err := agent.Snapshot(ctx, r.a.Harness, agent.EngineDoc, id)
	if err != nil || ok {
		return doc, doc.Config, err
	}
	settings, _, err := agent.Snapshot(ctx, r.a.Harness, agent.AgentDoc, id)
	if err != nil {
		return doc, agent.EngineConfig{}, err
	}
	config := agent.EngineConfig{Provider: "pi"}
	if selection := settings.Model(); selection != nil {
		value := selection.Provider + "/" + selection.ModelID
		config.Model = &value
	}
	return doc, config, nil
}

// profileOf is profile in packages/agent/src/runtime. Without a computer
// runtime no Bot has a bound work computer, so the catalog and connection come
// from the last engine inspection on the service computer.
func (r agentRoutes) profileOf(ctx context.Context, id int64) (any, error) {
	doc, config, err := r.engineConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	engines := agent.EngineCatalog()
	catalog := make([]map[string]any, len(engines))
	var connection, model any
	for i, e := range engines {
		entry := map[string]any{"id": e.ID, "name": e.Name, "version": nil, "models": []agent.EngineModel{}}
		if inspected, ok := r.rt.Inspection(e.ID); ok {
			entry["version"], entry["models"] = inspected.Version, inspected.Models
			if e.ID == config.Provider {
				connection = inspected
				for _, m := range inspected.Models {
					if config.Model != nil && m.ID == *config.Model {
						model = m
					}
				}
			}
		}
		catalog[i] = entry
	}
	var runtime any
	if doc.Runtime != nil && doc.ComputerID == nil {
		runtime = doc.Runtime
	}
	view := map[string]any{"provider": config.Provider, "model": config.Model}
	if config.Options != nil {
		view["options"] = config.Options
	}
	return map[string]any{
		"config":     view,
		"computerId": nil,
		"catalog":    catalog,
		"connection": connection,
		"model":      model,
		"revision":   doc.Revision,
		"runtime":    runtime,
		"busy":       len(doc.Tasks) > 0,
		"error":      doc.Error,
		"prepared":   doc.Prepared && cmp.Or(doc.PreparedComputerID, doc.ComputerID) == nil,
		"providers":  engines,
	}, nil
}

func (r agentRoutes) inspect(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var in struct {
		Config *agent.EngineConfig `json:"config"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, rpc.NewError("BAD_REQUEST", "Input validation failed")
	}
	if in.Config == nil {
		_, config, err := r.engineConfig(ctx, id)
		if err != nil {
			return nil, err
		}
		in.Config = &config
	}
	return r.rt.Inspect(ctx, *in.Config), nil
}

func (r agentRoutes) configure(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var in agent.EngineChange
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, rpc.NewError("BAD_REQUEST", "Input validation failed")
	}
	if err := r.rt.Configure(ctx, id, in); err != nil {
		return nil, rpcError(err)
	}
	return r.profileOf(ctx, id)
}

func (r agentRoutes) selectEngineModel(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var in struct {
		Model     *string        `json:"model"`
		Options   map[string]any `json:"options"`
		Revision  int            `json:"revision"`
		RequestID string         `json:"requestId"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, rpc.NewError("BAD_REQUEST", "Input validation failed")
	}
	_, config, err := r.engineConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := r.rt.Select(ctx, id, config, in.Model, in.Options, in.Revision, in.RequestID); err != nil {
		return nil, rpcError(err)
	}
	return r.profileOf(ctx, id)
}

func (r agentRoutes) selectModel(ctx context.Context, input json.RawMessage) (any, error) {
	var in struct {
		agent.ModelRef
		BotID int64 `json:"botId"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	model, ok, err := agent.ConfiguredModel(ctx, r.a.Models, &in.ModelRef)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, rpc.NewError("PRECONDITION_FAILED", "")
	}
	if _, err := r.bot(ctx, input); err != nil {
		return nil, err
	}
	config, _, err := agent.Snapshot(ctx, r.a.Harness, agent.AgentDoc, in.BotID)
	if err != nil {
		return nil, err
	}
	level := ""
	if current := config.ThinkingLevel(); slices.Contains(agent.ThinkingLevels(model), current) {
		level = current
	}
	selected := in.ModelRef
	if err := r.a.Configure(ctx, in.BotID, agent.Change{Model: &selected, ThinkingLevel: &level}); err != nil {
		return nil, err
	}
	return selected, nil
}

func (r agentRoutes) thinking(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var in struct {
		ThinkingLevel string `json:"thinkingLevel"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	config, _, err := agent.Snapshot(ctx, r.a.Harness, agent.AgentDoc, id)
	if err != nil {
		return nil, err
	}
	selection := config.Model()
	var model *agent.Model
	if selection != nil && r.a.Models != nil {
		available, err := r.a.Models.Available(ctx, "")
		if err != nil {
			return nil, err
		}
		if i := slices.IndexFunc(available, func(m agent.Model) bool {
			return m.Provider == selection.Provider && m.ID == selection.ModelID
		}); i >= 0 {
			model = &available[i]
		}
	}
	if model == nil {
		return nil, rpc.NewError("BAD_REQUEST", "")
	}
	level := ""
	if slices.Contains(agent.ThinkingLevels(*model), in.ThinkingLevel) {
		level = in.ThinkingLevel
	}
	if in.ThinkingLevel != "" && level == "" {
		return nil, rpc.NewError("BAD_REQUEST", "")
	}
	if err := r.a.Configure(ctx, id, agent.Change{ThinkingLevel: &level}); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

func (r agentRoutes) refresh(ctx context.Context, input json.RawMessage) (any, error) {
	var in struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	known := false
	if r.a.Models != nil {
		providers, err := r.a.Models.Providers(ctx)
		if err != nil {
			return nil, err
		}
		known = slices.ContainsFunc(providers, func(p agent.Provider) bool { return p.ID == in.Provider })
	}
	if !known {
		return nil, rpc.NewError("BAD_REQUEST", "")
	}
	bounded, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if err := r.a.Models.Refresh(bounded, in.Provider); err != nil {
		if ctx.Err() != nil && !errors.Is(err, context.DeadlineExceeded) {
			return nil, ctx.Err()
		}
		return nil, rpc.NewError("BAD_GATEWAY", "模型目录刷新失败。请重试。")
	}
	return agent.ModelCatalog(ctx, r.a.Models)
}

func (r agentRoutes) usage(ctx context.Context, input json.RawMessage) (any, error) {
	var filter agent.UsageFilter
	if err := json.Unmarshal(input, &filter); err != nil {
		return nil, err
	}
	bots, err := r.a.Bots.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(bots))
	for i, bot := range bots {
		ids[i] = bot.ID
	}
	return r.a.Usage(ctx, ids, filter)
}
