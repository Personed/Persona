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

// NativeModelsDoc keeps each Bot's last ready engine catalog across restarts,
// the persona.native-models document of packages/agent/src/harness/model-catalog.ts.
var NativeModelsDoc = Doc[nativeModels]{Kind: "persona.native-models", Scope: "session",
	Initial: func() nativeModels { return nativeModels{Items: []nativeModel{}} }}

type nativeModels struct {
	Items []nativeModel `json:"items"`
}

type nativeModel struct {
	BotID      int64         `json:"botId"`
	ComputerID *string       `json:"computerId"`
	Provider   string        `json:"provider"`
	Models     []EngineModel `json:"models"`
	Version    *string       `json:"version,omitempty"`
}

// Inspect checks the runtime's engine on the service computer for bot. Only Pi
// runs in the Go service, and it is authenticated when it lists a model, as the
// Pi provider in packages/agent/src/harness/providers/pi.ts. A ready result is
// saved as the Bot's catalog, as inspect in packages/agent/src/harness/connections.ts.
func (r *Runtime) Inspect(ctx context.Context, bot int64, config EngineConfig) (EngineInspection, error) {
	result := r.inspect(ctx, config)
	r.inspectMu.Lock()
	if r.inspected == nil {
		r.inspected = map[string]EngineInspection{}
	}
	r.inspected[config.Provider] = result
	r.inspectMu.Unlock()
	if !result.Ready {
		return result, nil
	}
	return result, r.h.Commit(ctx, func(tx *Tx) error {
		doc, err := Edit(tx, NativeModelsDoc, 0)
		if err != nil {
			return err
		}
		doc.Items = slices.DeleteFunc(doc.Items, func(item nativeModel) bool {
			return item.BotID == bot && item.ComputerID == nil && item.Provider == config.Provider
		})
		doc.Items = append(doc.Items, nativeModel{BotID: bot, Provider: config.Provider, Models: result.Models, Version: result.Version})
		return nil
	})
}

// Inspection returns the provider's last Inspect result in this process.
func (r *Runtime) Inspection(provider string) (EngineInspection, bool) {
	r.inspectMu.Lock()
	defer r.inspectMu.Unlock()
	result, ok := r.inspected[provider]
	return result, ok
}

// Catalog is the provider's models for bot: the last Inspect in this process,
// else the Bot's saved catalog, as catalog in packages/agent/src/harness/connections.ts.
func (r *Runtime) Catalog(ctx context.Context, bot int64, provider string) ([]EngineModel, *string, error) {
	if inspected, ok := r.Inspection(provider); ok {
		return inspected.Models, inspected.Version, nil
	}
	doc, _, err := Snapshot(ctx, r.h, NativeModelsDoc, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, item := range doc.Items {
		if item.BotID == bot && item.ComputerID == nil && item.Provider == provider {
			return item.Models, item.Version, nil
		}
	}
	return []EngineModel{}, nil, nil
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
	connection, err := r.Inspect(ctx, bot, current)
	if err != nil {
		return err
	}
	if !connection.offers(model) {
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
	connection, err := r.Inspect(ctx, bot, in.Config)
	if err != nil {
		return err
	}
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
		// switchSession in packages/agent/src/harness/sessions.ts keeps the
		// native session when only the model changes.
		if doc.Config.Provider != in.Config.Provider {
			doc.SessionID = nil
		}
		reset := doc.Config.Provider != in.Config.Provider || !equalModel(doc.Config.Model, in.Config.Model)
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
