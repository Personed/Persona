package server

import (
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
	s.Handle("engines.usage", func(ctx context.Context, input json.RawMessage) (any, error) {
		if !native {
			return []any{}, nil
		}
		return r.usage(ctx, input)
	})
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
