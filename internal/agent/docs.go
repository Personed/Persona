package agent

import (
	"encoding/json"

	"github.com/Personed/Persona/internal/memory"
)

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

// ThinkingLevel returns the configured thinking level, "off" when unset.
func (a Config) ThinkingLevel() string {
	level := "off"
	if raw, ok := a["thinkingLevel"]; ok {
		_ = json.Unmarshal(raw, &level)
	}
	return level
}

// Change configures a conversation as pi-durable's configure: a nil field
// keeps the current value. An empty ThinkingLevel removes it, as null does.
type Change struct {
	Model         *ModelRef
	Instructions  *string
	ThinkingLevel *string
}

func (a Config) apply(change Change) {
	if change.Model != nil {
		a["model"], _ = json.Marshal(change.Model)
	}
	if change.Instructions != nil {
		a["instructions"], _ = json.Marshal(*change.Instructions)
	}
	switch {
	case change.ThinkingLevel == nil:
	case *change.ThinkingLevel == "":
		delete(a, "thinkingLevel")
	default:
		a["thinkingLevel"], _ = json.Marshal(*change.ThinkingLevel)
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
	Config             EngineConfig     `json:"config"`
	Revision           int              `json:"revision"`
	RequestID          string           `json:"requestId"`
	SessionID          *string          `json:"sessionId"`
	ComputerID         *string          `json:"computerId"`
	TaskID             *int64           `json:"taskId"`
	Tasks              []int64          `json:"tasks"`
	ActiveID           *int64           `json:"activeId"`
	ActiveTurn         *string          `json:"activeTurn"`
	TurnID             *string          `json:"turnId"`
	Dispatch           string           `json:"dispatch"`
	Drafts             []*Draft         `json:"drafts"`
	Error              *string          `json:"error"`
	Runtime            map[string]any   `json:"runtime,omitempty"`
	Run                *EngineRunRecord `json:"run,omitempty"`
	Prepared           bool             `json:"prepared"`
	PreparedComputerID *string          `json:"preparedComputerId,omitempty"`
	Prompt             json.RawMessage  `json:"prompt"`
}

// EngineConfig is the Bot's engine selection, EngineConfig in types.ts.
type EngineConfig struct {
	Provider string         `json:"provider"`
	Model    *string        `json:"model"`
	Options  map[string]any `json:"options"`
}

// EngineRunRecord is one native run, EngineRun in types.ts.
type EngineRunRecord struct {
	TurnID    string  `json:"turnId"`
	Provider  string  `json:"provider"`
	Model     *string `json:"model"`
	StartedAt int64   `json:"startedAt"`
	Status    string  `json:"status"`
	NativeID  string  `json:"nativeId,omitempty"`
	EndedAt   *int64  `json:"endedAt,omitempty"`
	// InputTurns are the turns of additional input the native turn accepted.
	InputTurns []string `json:"inputTurns,omitempty"`
}

// Draft is a chat message the engine runtime writes, StoredMessage in
// task-types.ts: kept in the engine document while its turn runs and stored
// as a persona.engine-message entry after it.
type Draft struct {
	ID          string           `json:"id"`
	TurnID      string           `json:"turnId"`
	RequestID   string           `json:"requestId,omitempty"`
	Role        string           `json:"role"`
	Content     []map[string]any `json:"content"`
	Stopped     bool             `json:"stopped"`
	Timestamp   int64            `json:"timestamp"`
	ReceivedAt  *int64           `json:"receivedAt,omitempty"`
	UpdatedAt   *int64           `json:"updatedAt,omitempty"`
	Pending     *bool            `json:"pending,omitempty"`
	EndedAt     *int64           `json:"endedAt,omitempty"`
	StopReason  string           `json:"stopReason,omitempty"`
	Error       string           `json:"error,omitempty"`
	ToolCallID  string           `json:"toolCallId,omitempty"`
	ToolName    string           `json:"toolName,omitempty"`
	IsError     *bool            `json:"isError,omitempty"`
	ToolStatus  string           `json:"toolStatus,omitempty"`
	Hidden      bool             `json:"hidden,omitempty"`
	Routine     json.RawMessage  `json:"routine,omitempty"`
	Native      json.RawMessage  `json:"native,omitempty"`
	ParentID    string           `json:"parentId,omitempty"`
	Activity    json.RawMessage  `json:"activity,omitempty"`
	Attachments json.RawMessage  `json:"attachments,omitempty"`
	Materials   json.RawMessage  `json:"materials,omitempty"`
}

func initialEngine() EngineState {
	return EngineState{Config: EngineConfig{Provider: "pi", Options: map[string]any{}}, Tasks: []int64{},
		Dispatch: "ready", Drafts: []*Draft{}, Runtime: map[string]any{}, Prompt: json.RawMessage("null")}
}

// EngineInput is the delivery state of one engine task's input,
// EngineInputDoc in state.ts.
type EngineInput struct {
	Status string `json:"status"`
	Owner  *int64 `json:"owner"`
}

// MemoryReview is the Bot's background memory review state.
type MemoryReview struct {
	Enabled   bool   `json:"enabled"`
	Reviewing bool   `json:"reviewing"`
	Turns     int    `json:"turns"`
	Through   *int64 `json:"through"`
}

// MemoryItems is the persona.memory document that internal/memory owns.
type MemoryItems struct {
	Items []memory.Entry `json:"items"`
}

// MemorySnapshot is the profile text an engine task was started with.
type MemorySnapshot struct {
	Text       string `json:"text"`
	Dispatched bool   `json:"dispatched,omitempty"`
}

// TaskItems holds an engine task's usage or compaction records by ID.
type TaskItems struct {
	Items map[string]json.RawMessage `json:"items"`
}

func emptyItems() TaskItems { return TaskItems{Items: map[string]json.RawMessage{}} }

// Documents of pi-durable and packages/agent, by kind.
var (
	AgentDoc = Doc[Config]{Kind: "pi.agent", Scope: "conversation", History: "rewindable", Fork: "asOf",
		Initial: func() Config { return Config{} }}
	LiveDoc   = Doc[LiveState]{Kind: "pi.live", Scope: "conversation", History: "latest", Fork: "initial"}
	EngineDoc = Doc[EngineState]{Kind: "persona.engine", Scope: "conversation", History: "latest", Fork: "initial",
		Initial: initialEngine}
	EngineInputDoc = Doc[EngineInput]{Kind: "persona.engine-input", Scope: "task",
		Initial: func() EngineInput { return EngineInput{Status: "queued"} }}
	MemoryReviewDoc = Doc[MemoryReview]{Kind: "persona.memory-review", Scope: "conversation", History: "latest", Fork: "initial",
		Initial: func() MemoryReview { return MemoryReview{Enabled: true} }}
	MemoryDoc         = Doc[MemoryItems]{Kind: memory.DocKind, Scope: "session"}
	MemorySnapshotDoc = Doc[MemorySnapshot]{Kind: memory.SnapshotDocKind, Scope: "task"}
	UsageTaskDoc      = Doc[TaskItems]{Kind: "persona.engine-usage", Scope: "task", Initial: emptyItems}
	CompactionTaskDoc = Doc[TaskItems]{Kind: "persona.engine-compaction", Scope: "task", Initial: emptyItems}
	RequestsDoc       = Doc[ComputerRequests]{Kind: "persona.computer-requests", Scope: "conversation", History: "latest", Fork: "initial"}
	RoutinesDoc       = Doc[RoutinesState]{Kind: "persona.routines", Scope: "session"}
	SkillsDoc         = Doc[SkillsState]{Kind: "persona.skills", Scope: "session"}
)

// SkillItem is the part of a persona.skills record the prompt section reads.
type SkillItem struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	EnabledBots []int64 `json:"enabledBots"`
}

// SkillsState is the part of persona.skills this package reads.
type SkillsState struct {
	Items []SkillItem `json:"items"`
}

// RoutineRun is the run receipt fields the runtime and chat projection read,
// RoutineRun in packages/agent/src/work/routine-state.ts.
type RoutineRun struct {
	ID          string `json:"id"`
	RoutineID   string `json:"routineId"`
	BotID       int64  `json:"botId"`
	Title       string `json:"title"`
	Mode        string `json:"mode"`
	ScheduledAt int64  `json:"scheduledAt"`
	Status      string `json:"status"`
	RequestID   string `json:"requestId"`
	Delivery    string `json:"delivery,omitempty"`
	TurnID      string `json:"turnId,omitempty"`
}

// RoutinesState is the part of persona.routines this package reads.
type RoutinesState struct {
	Runs []RoutineRun `json:"runs"`
}

// ComputerRequests is the persona.computer-requests document; internal/approval
// owns its writes and the chat shows its pending approvals.
type ComputerRequests struct {
	Approvals []json.RawMessage `json:"approvals"`
}

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
