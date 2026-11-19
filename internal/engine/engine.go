// Package engine defines the model-loop engine. Pi over `pi --mode rpc` is the only
// implementation (D1); the interface keeps room for an ACP engine later.
package engine

import (
	"context"
	"encoding/json"
)

// Model selects a provider model, matching the API's {provider, modelId}.
type Model struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// SessionOptions start or resume one native session for a Bot task.
type SessionOptions struct {
	BotID int64
	// SessionID resumes an existing native session when set.
	SessionID string
	// WorkDir is on the Bot's bound work computer, not the service computer.
	WorkDir       string
	Model         Model
	ThinkingLevel string
	SystemPrompt  string
	// Tools are product tools declared to the engine (Pi reaches them through native MCP).
	Tools []ToolSpec
	Env   map[string]string
}

// ToolSpec declares one product tool to the engine.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Image is base64 image content attached to a prompt.
type Image struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// Prompt is one user input. RequestID stays stable across retries.
type Prompt struct {
	RequestID string
	Text      string
	Images    []Image
}

// Event is one engine event. Type follows Pi's RPC event names (agent_start,
// message_update, tool_execution_end, agent_end, extension_ui_request, ...);
// Data is the event payload as received.
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// State is the engine's view of the session, from Pi's get_state.
type State struct {
	SessionID     string `json:"sessionId"`
	Model         *Model `json:"model,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	Streaming     bool   `json:"isStreaming"`
	Compacting    bool   `json:"isCompacting"`
	PendingInputs int    `json:"pendingMessageCount"`
}

// Usage is cumulative token and cost accounting for a session.
type Usage struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Cost       float64 `json:"cost"`
}

// Session is one running native session. Methods are safe for concurrent use.
type Session interface {
	ID() string
	// Prompt starts a turn, or queues the input as a follow-up while a turn runs.
	Prompt(ctx context.Context, prompt Prompt) error
	// Steer injects input into the running turn.
	Steer(ctx context.Context, prompt Prompt) error
	// Abort stops the running turn and its process tree.
	Abort(ctx context.Context) error
	// Answer replies to an extension_ui_request (questions, approvals) by its ID.
	Answer(ctx context.Context, requestID string, value json.RawMessage) error
	SetModel(ctx context.Context, model Model, thinkingLevel string) error
	Compact(ctx context.Context, instructions string) error
	State(ctx context.Context) (State, error)
	Usage(ctx context.Context) (Usage, error)
	// Messages reads native history for recovery, in pages.
	Messages(ctx context.Context, offset, limit int) ([]json.RawMessage, error)
	// Events streams events until the session closes. One consumer per session.
	Events() <-chan Event
	// Close ends the session and its process. An idle Bot has no engine process.
	Close() error
}

// Info describes an installed engine, matching the API's engines.catalog items.
type Info struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Version      string          `json:"version,omitempty"`
	Capabilities map[string]bool `json:"capabilities"`
}

// Engine starts native sessions.
type Engine interface {
	Info(ctx context.Context) (Info, error)
	Models(ctx context.Context) ([]Model, error)
	Start(ctx context.Context, options SessionOptions) (Session, error)
}
