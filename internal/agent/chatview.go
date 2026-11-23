package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Personed/Persona/internal/store"
)

const (
	windowSize   = 50
	pageEntries  = 500
	pageMessages = 200
	// enginePage is the page the get procedure and chat updates read for a
	// native runtime.
	enginePage     = 200
	maxSafeInteger = 1<<53 - 1
)

// ChatMessage is one projected chat message, ChatMessage in messages.ts.
type ChatMessage struct {
	ID          string           `json:"id"`
	TurnID      *string          `json:"turnId,omitempty"`
	RequestID   string           `json:"requestId,omitempty"`
	Role        string           `json:"role"`
	Timestamp   json.RawMessage  `json:"timestamp,omitempty"`
	Attachments json.RawMessage  `json:"attachments,omitempty"`
	Materials   json.RawMessage  `json:"materials,omitempty"`
	StopReason  string           `json:"stopReason,omitempty"`
	Content     []map[string]any `json:"content"`
	Error       string           `json:"error,omitempty"`
	Stopped     bool             `json:"stopped"`
	ToolCallID  *string          `json:"toolCallId,omitempty"`
	ToolName    *string          `json:"toolName,omitempty"`
	IsError     *bool            `json:"isError,omitempty"`
	Diagnostics json.RawMessage  `json:"diagnostics,omitempty"`
	ToolStatus  string           `json:"toolStatus,omitempty"`
	Delivery    json.RawMessage  `json:"delivery,omitempty"`
	Notice      json.RawMessage  `json:"notice,omitempty"`
	Cursor      string           `json:"cursor,omitempty"`
}

// storedMessage is a persona.engine-message entry: a ChatMessage the engine
// runtime wrote, shown as stored with its cursor.
type storedMessage struct {
	fields map[string]json.RawMessage
	cursor string
}

func (m storedMessage) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(m.fields)+1)
	for key, value := range m.fields {
		out[key] = value
	}
	out["cursor"], _ = json.Marshal(m.cursor)
	return json.Marshal(out)
}

// LiveTool is a tool call of the current round.
type LiveTool struct {
	CallID string `json:"callId"`
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
}

// ChatView is a Bot's chat as the get procedure returns it without a native
// engine runtime, chatView in chat-view.ts.
type ChatView struct {
	Before       *string           `json:"before,omitempty"`
	ID           int64             `json:"id"`
	Model        *ModelRef         `json:"model"`
	Instructions string            `json:"instructions"`
	Messages     []any             `json:"messages"`
	Runs         []json.RawMessage `json:"runs"`
	Draft        *ChatMessage      `json:"draft"`
	ActiveTurnID *string           `json:"activeTurnId"`
	Busy         bool              `json:"busy"`
	Processing   bool              `json:"processing"`
	Failed       bool              `json:"failed"`
	Approvals    []json.RawMessage `json:"approvals"`
	Tools        []LiveTool        `json:"tools"`
}

type conversationView struct {
	id      int64
	entries []store.Entry
	before  store.EntryID
	live    LiveState
	agent   Config
}

func (h *Harness) conversationDocs(ctx context.Context, id int64) (conversationView, error) {
	view := conversationView{id: id}
	var err error
	if view.live, _, err = Snapshot(ctx, h, LiveDoc, id); err != nil {
		return view, err
	}
	view.agent, _, err = Snapshot(ctx, h, AgentDoc, id)
	return view, err
}

// entries scans up to limit entries from the newest, at or before max when
// set, and returns them oldest first with whether older entries remain.
func (h *Harness) newest(ctx context.Context, id int64, maxID store.EntryID, limit int) ([]store.Entry, bool, error) {
	var out []store.Entry
	scan := store.Scan{Descending: true}
	for {
		scan.Limit = limit - len(out)
		page, err := h.store.Entries(ctx, store.EntryQuery{ConversationID: store.ConversationID(id), Max: maxID}, scan)
		if err != nil {
			return nil, false, err
		}
		out = append(out, page.Items...)
		if page.Next == "" || len(out) >= limit {
			for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
				out[i], out[j] = out[j], out[i]
			}
			return out, page.Next != "", nil
		}
		scan.Cursor = page.Next
	}
}

// ReadChat returns the chat page before the cursor "entry:index", or the newest
// window when before is empty, as readConversationPage.
func (h *Harness) ReadChat(ctx context.Context, id int64, before string) (ChatView, error) {
	view, err := h.conversationDocs(ctx, id)
	if err != nil {
		return ChatView{}, err
	}
	if before == "" {
		entries, more, err := h.newest(ctx, id, 0, windowSize+1)
		if err != nil {
			return ChatView{}, err
		}
		if more || len(entries) > windowSize {
			view.before, entries = entries[0].ID, entries[1:]
		}
		view.entries = entries
		return projectChat(view), nil
	}
	entry, index := splitCursor(before)
	if entry == 0 {
		return projectChat(view), nil
	}
	entries, more, err := h.newest(ctx, id, store.EntryID(entry), pageEntries)
	if err != nil {
		return ChatView{}, err
	}
	view.entries = entries
	chat := projectChat(view)
	var kept []any
	for _, message := range chat.Messages {
		cursor := messageCursor(message)
		if cursor == "" {
			continue
		}
		e, i := splitCursor(cursor)
		if e < entry || (e == entry && i < index) {
			kept = append(kept, message)
		}
	}
	all := len(kept)
	if all > pageMessages {
		kept = kept[all-pageMessages:]
	}
	chat.Messages = append([]any{}, kept...)
	if more || all > pageMessages {
		cursor := "0:0"
		if len(chat.Messages) > 0 {
			cursor = messageCursor(chat.Messages[0])
		} else if len(entries) > 0 {
			cursor = fmt.Sprintf("%d:0", entries[0].ID)
		}
		chat.Before = &cursor
	}
	return chat, nil
}

// ReadEnginePage returns up to limit messages before the cursor
// "entry:index", read from the newest limit entries, as view in
// runtime/index.ts reads a page for a native runtime.
func (h *Harness) ReadEnginePage(ctx context.Context, id int64, before string, limit int) (ChatView, error) {
	view, err := h.conversationDocs(ctx, id)
	if err != nil {
		return ChatView{}, err
	}
	entry, index := splitCursor(before)
	entries, more, err := h.newest(ctx, id, store.EntryID(entry), limit)
	if err != nil {
		return ChatView{}, err
	}
	view.entries = entries
	chat := projectChat(view)
	var kept []any
	for _, message := range chat.Messages {
		cursor := messageCursor(message)
		if entry != 0 && cursor != "" {
			e, i := splitCursor(cursor)
			if e > entry || (e == entry && i >= index) {
				continue
			}
		}
		kept = append(kept, message)
	}
	all := len(kept)
	if all > limit {
		kept = kept[all-limit:]
	}
	chat.Messages = append([]any{}, kept...)
	if more || all > limit {
		cursor := ""
		if len(chat.Messages) > 0 {
			cursor = messageCursor(chat.Messages[0])
		}
		if cursor == "" && len(entries) > 0 {
			cursor = fmt.Sprintf("%d:0", entries[0].ID)
		}
		chat.Before = &cursor
	}
	return chat, nil
}

// ReadHistory projects the whole chat, as chatView(readConversationHistory()).
func (h *Harness) ReadHistory(ctx context.Context, id int64) (ChatView, error) {
	view, err := h.conversationDocs(ctx, id)
	if err != nil {
		return ChatView{}, err
	}
	scan := store.Scan{}
	for {
		page, err := h.store.Entries(ctx, store.EntryQuery{ConversationID: store.ConversationID(id)}, scan)
		if err != nil {
			return ChatView{}, err
		}
		view.entries = append(view.entries, page.Items...)
		if page.Next == "" {
			return projectChat(view), nil
		}
		scan.Cursor = page.Next
	}
}

func splitCursor(cursor string) (int64, int64) {
	entry, index, _ := strings.Cut(cursor, ":")
	e, _ := strconv.ParseInt(entry, 10, 64)
	i, _ := strconv.ParseInt(index, 10, 64)
	return e, i
}

func messageCursor(message any) string {
	switch m := message.(type) {
	case ChatMessage:
		return m.Cursor
	case storedMessage:
		return m.cursor
	}
	return ""
}

// messageAttachments lists the file IDs a projected message shows.
func messageAttachments(message any) []string {
	var raw json.RawMessage
	switch m := message.(type) {
	case ChatMessage:
		raw = m.Attachments
	case storedMessage:
		raw = m.fields["attachments"]
	}
	var files []struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &files)
	ids := make([]string, len(files))
	for i, file := range files {
		ids[i] = file.ID
	}
	return ids
}

type inputData struct {
	Text        *string         `json:"text"`
	Attachments json.RawMessage `json:"attachments"`
	Materials   json.RawMessage `json:"materials"`
	RequestID   string          `json:"requestId"`
}

type toolDetails struct {
	Attachment json.RawMessage `json:"attachment"`
	Delivery   json.RawMessage `json:"delivery"`
	Notice     json.RawMessage `json:"notice"`
}

func present(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }

func projectChat(view conversationView) ChatView {
	lastAttempts := map[store.TaskID]store.EntryID{}
	for _, entry := range view.entries {
		if entry.ByTaskID == nil {
			continue
		}
		for _, m := range decodeMessages(entry.Model) {
			if m.Role == "assistant" {
				lastAttempts[*entry.ByTaskID] = entry.ID
				break
			}
		}
	}
	var turnID *string
	failed := false
	project := func(m modelMessage, id string, attachments json.RawMessage, text *string, stopped bool, diagnostics json.RawMessage) *ChatMessage {
		if m.Role == "system" {
			return nil
		}
		if m.Role == "user" {
			turn := id
			turnID = &turn
		}
		out := &ChatMessage{ID: id, TurnID: turnID, Role: m.Role, Timestamp: m.Timestamp, Attachments: attachments, Content: []map[string]any{}}
		if m.Role == "assistant" {
			out.StopReason = m.StopReason
		}
		failure := m.Role == "assistant" && m.StopReason == "error"
		if failure {
			out.Error = modelFailure(m, id)
		} else {
			parts := []contentPart{}
			if text != nil {
				parts = append(parts, contentPart{Type: "text", Text: *text})
			} else {
				for _, part := range m.parts() {
					if part.Type != "thinking" || !part.Redacted {
						parts = append(parts, part)
					}
				}
			}
			for i, part := range parts {
				item := map[string]any{"type": part.Type, "key": fmt.Sprintf("%s:%d", id, i)}
				switch part.Type {
				case "text":
					item["text"] = part.Text
				case "thinking":
					item["thinking"] = part.Thinking
				case "image":
					item["data"], item["mimeType"] = part.Data, part.MimeType
				default:
					item["id"], item["name"] = part.ID, part.Name
					if present(part.Arguments) {
						item["arguments"] = part.Arguments
					}
				}
				out.Content = append(out.Content, item)
			}
		}
		out.Stopped = m.Role == "assistant" && m.StopReason == "aborted"
		if m.Role == "toolResult" {
			callID, name, isError := m.ToolCallID, m.ToolName, m.IsError
			out.ToolCallID, out.ToolName, out.IsError, out.Diagnostics = &callID, &name, &isError, diagnostics
			switch {
			case stopped:
				out.ToolStatus = "stopped"
			case isError:
				out.ToolStatus = "failed"
			default:
				out.ToolStatus = "done"
			}
		}
		return out
	}

	chat := ChatView{ID: view.id, Model: view.agent.Model(), Instructions: view.agent.Instructions(),
		Messages: []any{}, Runs: []json.RawMessage{}, Approvals: []json.RawMessage{}, Tools: []LiveTool{}}
	if view.before != 0 {
		before := fmt.Sprintf("%d:%d", view.before, int64(maxSafeInteger))
		chat.Before = &before
	}
	running, runTask := view.live.running(), view.live.runTask()
	for _, entry := range view.entries {
		if hiddenEntry(entry.Kind) {
			continue
		}
		if entry.Kind == EngineRun {
			chat.Runs = append(chat.Runs, entry.Data)
			continue
		}
		if entry.Kind == EngineMessage {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(entry.Data, &fields)
			var head struct {
				Role   string  `json:"role"`
				TurnID *string `json:"turnId"`
			}
			_ = json.Unmarshal(entry.Data, &head)
			if head.Role == "user" {
				turnID = head.TurnID
			}
			chat.Messages = append(chat.Messages, storedMessage{fields: fields, cursor: fmt.Sprintf("%d:0", entry.ID)})
			continue
		}
		var input *inputData
		if entry.Kind == FileInput || entry.Kind == RoutineInput {
			input = &inputData{}
			_ = json.Unmarshal(entry.Data, input)
		}
		var diagnostics json.RawMessage
		if entry.Kind == "pi.tool-result" {
			var data struct {
				Diagnostics json.RawMessage `json:"diagnostics"`
			}
			_ = json.Unmarshal(entry.Data, &data)
			if present(data.Diagnostics) {
				diagnostics = data.Diagnostics
			}
		}
		stopped := diagnostics != nil && aborted(entry)
		for index, m := range decodeMessages(entry.Model) {
			if m.Role == "assistant" && m.StopReason == "error" && entry.ByTaskID != nil &&
				(lastAttempts[*entry.ByTaskID] != entry.ID || (running && int64(*entry.ByTaskID) == runTask)) {
				continue
			}
			var details toolDetails
			if m.Role == "toolResult" {
				_ = json.Unmarshal(m.Details, &details)
			}
			failed = m.Role == "assistant" && m.StopReason == "error"
			var attachments json.RawMessage
			var text *string
			if input != nil {
				text = input.Text
				if present(input.Attachments) {
					attachments = input.Attachments
				}
			}
			if attachments == nil && present(details.Attachment) {
				attachments = json.RawMessage("[" + string(details.Attachment) + "]")
			}
			id := fmt.Sprintf("%d:%d", entry.ID, index)
			projected := project(m, id, attachments, text, stopped, diagnostics)
			if projected == nil {
				continue
			}
			projected.Cursor = id
			if projected.Role == "user" && input != nil {
				projected.RequestID = input.RequestID
				if present(input.Materials) {
					projected.Materials = input.Materials
				}
			}
			if m.Role == "toolResult" && !m.IsError && !stopped {
				if present(details.Delivery) {
					projected.Delivery = details.Delivery
				}
				if present(details.Notice) {
					projected.Notice = details.Notice
				}
			}
			chat.Messages = append(chat.Messages, *projected)
		}
	}
	if generation := view.live.Generation; generation != nil && present(generation.Message) {
		var m modelMessage
		if json.Unmarshal(generation.Message, &m) == nil {
			chat.Draft = project(m, "draft", nil, nil, false, nil)
		}
	}
	if running {
		chat.ActiveTurnID = turnID
	}
	chat.Busy = running
	pendingTool := false
	for _, tool := range view.live.Tools {
		pendingTool = pendingTool || tool.Status != "done"
		chat.Tools = append(chat.Tools, LiveTool(tool))
	}
	chat.Processing = running && (view.live.Generation != nil || pendingTool)
	chat.Failed = failed && !running
	return chat
}

// Attachments lists the file IDs the chat shows.
func (c ChatView) Attachments() []string {
	var ids []string
	for _, message := range c.Messages {
		ids = append(ids, messageAttachments(message)...)
	}
	return ids
}
