package client

import (
	"encoding/json"
	"strings"
)

// Bot is one entry of bots.list.
type Bot struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	Avatar          string `json:"avatar"`
	WelcomeComplete bool   `json:"welcomeComplete"`
	Busy            bool   `json:"busy"`
	Preview         string `json:"preview"`
}

// ModelRef names a provider model.
type ModelRef struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// Content is one block of a chat message.
type Content struct {
	Type      string          `json:"type"`
	Key       string          `json:"key,omitempty"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	MimeType  string          `json:"mimeType,omitempty"`
}

// MessageApproval is the approval card stored on a chat message.
type MessageApproval struct {
	Request Approval        `json:"request"`
	State   string          `json:"state"`
	Answer  json.RawMessage `json:"answer,omitempty"`
}

// Message is a displayed chat message.
type Message struct {
	ID         string           `json:"id"`
	TurnID     string           `json:"turnId,omitempty"`
	RequestID  string           `json:"requestId,omitempty"`
	Role       string           `json:"role"`
	Content    []Content        `json:"content"`
	Stopped    bool             `json:"stopped,omitempty"`
	Pending    bool             `json:"pending,omitempty"`
	Hidden     bool             `json:"hidden,omitempty"`
	Cursor     string           `json:"cursor,omitempty"`
	ToolName   string           `json:"toolName,omitempty"`
	ToolStatus string           `json:"toolStatus,omitempty"`
	Error      string           `json:"error,omitempty"`
	Approval   *MessageApproval `json:"approval,omitempty"`
	Notice     *struct {
		Text string `json:"text"`
	} `json:"notice,omitempty"`
	Routine *struct {
		RunID string `json:"runId"`
		Title string `json:"title"`
	} `json:"routine,omitempty"`
}

// Text joins the text blocks of m.
func (m Message) Text() string {
	var parts []string
	for _, c := range m.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Option is one choice of an approval or question.
type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one question of a question approval.
type Question struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Options    []Option `json:"options,omitempty"`
	Multiple   bool     `json:"multiple,omitempty"`
	AllowOther bool     `json:"allowOther,omitempty"`
	Multiline  bool     `json:"multiline,omitempty"`
	Required   bool     `json:"required,omitempty"`
}

// Approval is a pending permission request or question from the engine.
type Approval struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Detail       string `json:"detail,omitempty"`
	ConfirmLabel string `json:"confirmLabel,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Fields       []struct {
		Label string `json:"label"`
		Value string `json:"value"`
	} `json:"fields,omitempty"`
	Questions []Question `json:"questions,omitempty"`
	Options   []Option   `json:"options,omitempty"`
}

// ChatView is the chat state returned by get and streamed by changes.
type ChatView struct {
	ID           int       `json:"id"`
	Model        *ModelRef `json:"model"`
	Instructions string    `json:"instructions"`
	Messages     []Message `json:"messages"`
	ActiveTurnID *string   `json:"activeTurnId"`
	// Draft is the reply still being generated on the path without a native Harness.
	Draft      *Message   `json:"draft,omitempty"`
	Busy       bool       `json:"busy"`
	Processing bool       `json:"processing"`
	Failed     bool       `json:"failed"`
	Approvals  []Approval `json:"approvals"`
	Error      string     `json:"error,omitempty"`
	Before     string     `json:"before,omitempty"`
}

// Change is one event of the changes stream: a full snapshot, then partial updates.
type Change struct {
	Type     string                     `json:"type"`
	View     *ChatView                  `json:"view,omitempty"`
	State    map[string]json.RawMessage `json:"state,omitempty"`
	Messages []Message                  `json:"messages,omitempty"`
	Removed  []string                   `json:"removed,omitempty"`
}

// Apply folds c into v. A snapshot replaces v; an update merges changed
// state fields, replaces changed messages by ID, and drops removed IDs.
func (v *ChatView) Apply(c Change) error {
	if c.Type == "snapshot" {
		if c.View != nil {
			*v = *c.View
		}
		return nil
	}
	if len(c.State) > 0 {
		state, err := json.Marshal(c.State)
		if err != nil {
			return err
		}
		messages := v.Messages
		if err := json.Unmarshal(state, v); err != nil {
			return err
		}
		v.Messages = messages
		if string(c.State["error"]) == "null" {
			v.Error = ""
		}
	}
	removed := make(map[string]bool, len(c.Removed))
	for _, id := range c.Removed {
		removed[id] = true
	}
	index := make(map[string]int, len(v.Messages))
	kept := v.Messages[:0:0]
	for _, m := range v.Messages {
		if !removed[m.ID] {
			index[m.ID] = len(kept)
			kept = append(kept, m)
		}
	}
	for _, m := range c.Messages {
		if i, ok := index[m.ID]; ok {
			kept[i] = m
		} else {
			index[m.ID] = len(kept)
			kept = append(kept, m)
		}
	}
	v.Messages = kept
	return nil
}
