package agent

import (
	"context"
	"slices"
)

// EngineModel is EngineModel in packages/agent/src/harness/types.ts.
type EngineModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// EngineInspection is a native engine connection check, the result of
// inspect in packages/agent/src/harness/connections.ts.
type EngineInspection struct {
	Ready         bool          `json:"ready"`
	Version       *string       `json:"version"`
	Path          *string       `json:"path"`
	Authenticated bool          `json:"authenticated"`
	Models        []EngineModel `json:"models"`
	Vision        *bool         `json:"vision"`
	Message       string        `json:"message"`
}

// Inspect checks the runtime's engine on the service computer. Only Pi runs in
// the Go service, and it is authenticated when it lists a model, as the Pi
// provider in packages/agent/src/harness/providers/pi.ts.
func (r *Runtime) Inspect(ctx context.Context, config EngineConfig) EngineInspection {
	result := r.inspect(ctx, config)
	r.inspectMu.Lock()
	defer r.inspectMu.Unlock()
	if r.inspected == nil {
		r.inspected = map[string]EngineInspection{}
	}
	r.inspected[config.Provider] = result
	return result
}

// Inspection returns the provider's last Inspect result.
func (r *Runtime) Inspection(provider string) (EngineInspection, bool) {
	r.inspectMu.Lock()
	defer r.inspectMu.Unlock()
	result, ok := r.inspected[provider]
	return result, ok
}

func (r *Runtime) inspect(ctx context.Context, config EngineConfig) EngineInspection {
	missing := EngineInspection{Models: []EngineModel{}, Message: "这台工作电脑还未安装此引擎。"}
	if config.Provider != "pi" || r.engine == nil {
		return missing
	}
	info, err := r.engine.Info(ctx)
	if err != nil {
		return missing
	}
	result := EngineInspection{Ready: true, Models: []EngineModel{}, Message: "引擎已就绪。"}
	if info.Version != "" {
		result.Version = &info.Version
	}
	models, err := r.engine.Models(ctx)
	if err != nil {
		return result
	}
	for _, m := range models {
		id := m.Provider + "/" + m.ModelID
		result.Models = append(result.Models, EngineModel{ID: id, Name: m.ModelID})
	}
	result.Authenticated = len(result.Models) > 0
	return result
}

func (i EngineInspection) offers(model *string) bool {
	return i.Authenticated && (model == nil || slices.ContainsFunc(i.Models, func(m EngineModel) bool { return m.ID == *model }))
}

// EngineChange is the input of engines.configure.
type EngineChange struct {
	Config    EngineConfig `json:"config"`
	Revision  int          `json:"revision"`
	RequestID string       `json:"requestId"`
}

// Select changes the Bot's model within its configured engine, as select in
// packages/agent/src/runtime/index.ts.
func (r *Runtime) Select(ctx context.Context, bot int64, current EngineConfig, model *string, options map[string]any, revision int, requestID string) error {
	if !r.Inspect(ctx, current).offers(model) {
		return &WorkError{Code: "BAD_REQUEST", Message: "请选择工作电脑上可用的模型。"}
	}
	config := EngineConfig{Provider: current.Provider, Model: model, Options: options}
	return r.Configure(ctx, bot, EngineChange{Config: config, Revision: revision, RequestID: requestID})
}

// Configure replaces the Bot's engine configuration, as configure in
// packages/agent/src/runtime/index.ts. Native option controls are not read
// yet, so a configuration with options is refused.
func (r *Runtime) Configure(ctx context.Context, bot int64, in EngineChange) error {
	if in.Config.Provider != "pi" {
		return &WorkError{Code: "NOT_IMPLEMENTED", Message: "Go 服务目前只支持 Pi 引擎。"}
	}
	connection := r.Inspect(ctx, in.Config)
	return r.h.Commit(ctx, func(tx *Tx) error {
		doc, err := Create(tx, EngineDoc, bot)
		if err != nil {
			return err
		}
		if doc.RequestID == in.RequestID {
			return nil
		}
		live, err := Edit(tx, LiveDoc, bot)
		if err != nil {
			return err
		}
		if len(doc.Tasks) > 0 || live.running() {
			return &WorkError{Code: "CONFLICT", Message: "请先停止 Bot 的工作。"}
		}
		if doc.Revision != in.Revision {
			return &WorkError{Code: "CONFLICT", Message: "配置已更新。请刷新后重试。"}
		}
		if len(in.Config.Options) > 0 {
			return &WorkError{Code: "BAD_REQUEST", Message: "请先读取此模型的原生配置，再选择可用值。"}
		}
		reset := doc.Config.Provider != in.Config.Provider || !equalModel(doc.Config.Model, in.Config.Model)
		if reset {
			doc.SessionID = nil
		}
		doc.Config = in.Config
		doc.Revision++
		doc.RequestID = in.RequestID
		if !reset {
			delete(doc.Runtime, "controls")
			return nil
		}
		doc.Runtime = map[string]any{}
		doc.TurnID = nil
		doc.Dispatch = "ready"
		doc.Error = nil
		doc.Prepared = connection.offers(in.Config.Model)
		doc.PreparedComputerID = nil
		doc.Prompt = nil
		return nil
	})
}

func equalModel(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
