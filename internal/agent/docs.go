package agent

import "encoding/json"

// ModelRef selects a model of a provider.
type ModelRef struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// Config is the pi.agent document of a conversation. Fields this package does not change are
// kept as written.
type Config map[string]json.RawMessage

// Model returns the configured model, or nil.
func (a Config) Model() *ModelRef {
	var model *ModelRef
	if raw, ok := a["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	return model
}

// Instructions returns the standing instructions, or "".
func (a Config) Instructions() string {
	var text string
	if raw, ok := a["instructions"]; ok {
		_ = json.Unmarshal(raw, &text)
	}
	return text
}

// Change configures a conversation as pi-durable's configure: a nil field
// keeps the current value.
type Change struct {
	Model        *ModelRef
	Instructions *string
}

func (a Config) apply(change Change) {
	if change.Model != nil {
		a["model"], _ = json.Marshal(change.Model)
	}
	if change.Instructions != nil {
		a["instructions"], _ = json.Marshal(*change.Instructions)
	}
}

// LiveState is the part of the pi.live document the chat projection reads.
type LiveState struct {
	Run        json.RawMessage `json:"run,omitempty"`
	Generation *struct {
		Message json.RawMessage `json:"message,omitempty"`
	} `json:"generation,omitempty"`
	Tools []struct {
		CallID string `json:"callId"`
		Status string `json:"status"`
		Output string `json:"output,omitempty"`
	} `json:"tools,omitempty"`
}

func (l LiveState) running() bool { return len(l.Run) > 0 && string(l.Run) != "null" }

func (l LiveState) runTask() int64 {
	var run struct {
		TaskID int64 `json:"taskId"`
	}
	_ = json.Unmarshal(l.Run, &run)
	return run.TaskID
}

// EngineState is the part of the persona.engine document this package reads.
type EngineState struct {
	Tasks []int64 `json:"tasks"`
}

// Documents of pi-durable and packages/agent, by kind.
var (
	AgentDoc = Doc[Config]{Kind: "pi.agent", Scope: "conversation", History: "rewindable", Fork: "asOf",
		Initial: func() Config { return Config{} }}
	LiveDoc   = Doc[LiveState]{Kind: "pi.live", Scope: "conversation", History: "latest", Fork: "initial"}
	EngineDoc = Doc[EngineState]{Kind: "persona.engine", Scope: "conversation", History: "latest", Fork: "initial"}
)

// Entry kinds read by the chat projection.
const (
	FileInput     = "persona.file-input"
	RoutineInput  = "persona.routine-input"
	EngineMessage = "persona.engine-message"
	EngineRun     = "persona.engine-run"
)

func hiddenEntry(kind string) bool {
	return kind == "persona.context-history" || kind == "pi.compaction" || kind == "pi.reset"
}
