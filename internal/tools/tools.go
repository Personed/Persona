// Package tools defines the product tool registry shared by every engine.
package tools

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrUnknownTool is returned by Call for a name the registry does not hold.
var ErrUnknownTool = errors.New("tools: unknown tool")

// Definition is a tool as declared to the model. Schemas are JSON Schema 2020-12.
type Definition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	// Annotations are preserved unchanged from native catalogs (readOnlyHint, ...).
	Annotations map[string]any `json:"annotations,omitempty"`
	// Mutates routes the call through the shared approval callback.
	Mutates bool `json:"-"`
	// Automatic lets the Bot's "auto" approval mode allow a mutating call
	// without asking, like the TypeScript computer approval callback.
	Automatic bool `json:"-"`
}

// Content is one block of a tool result: "text", "image" or "resource".
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// Result is a tool result. Handle addresses the full persisted result for bounded reads.
type Result struct {
	Content    []Content       `json:"content"`
	Structured json.RawMessage `json:"structuredContent,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
	Handle     string          `json:"handle,omitempty"`
}

// Call is one invocation. CallID is the engine's tool call ID and is stable across
// recovery, so a call whose outcome is uncertain is never replayed.
type Call struct {
	BotID  int64
	TaskID int64
	CallID string
	Name   string
	Input  json.RawMessage
}

// Approver decides whether a mutating call may run. It blocks until the user answers or ctx ends.
type Approver func(ctx context.Context, call Call, definition Definition) (bool, error)

// Tool executes one definition.
type Tool interface {
	Definition() Definition
	Run(ctx context.Context, call Call) (Result, error)
}

// Registry holds the tools available to Bots and applies grants and approval before each call.
type Registry interface {
	Register(tool Tool) error
	// List returns the tools granted to a Bot, in a stable order.
	List(ctx context.Context, botID int64) ([]Definition, error)
	Call(ctx context.Context, call Call, approve Approver) (Result, error)
}
