package pi

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Personed/Persona/internal/engine"
)

// Projection turns Pi frames into Persona engine events: the event shapes of
// EngineEvent in packages/agent/src/harness/types.ts, as engine.Event with the
// Persona type in Type and the payload, receivedAt included, in Data. One
// Projection follows one session's frames in order.
type Projection struct {
	messageID string
	sequence  int
	failure   string
	settled   bool
}

// Failure is the reason the last assistant message ended without stopping
// normally, or "" once a message stopped normally.
func (p *Projection) Failure() string { return p.failure }

// Settled reports whether the last projected frame was agent_settled, the end
// of a prompt's work.
func (p *Projection) Settled() bool { return p.settled }

type piContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type piUsage struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cacheRead"`
	CacheWrite *float64 `json:"cacheWrite"`
	Cost       *struct {
		Total *float64 `json:"total"`
	} `json:"cost"`
}

type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Timestamp    json.RawMessage `json:"timestamp"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Usage        *piUsage        `json:"usage"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	IsError      bool            `json:"isError"`
	Details      json.RawMessage `json:"details"`
}

type piFrame struct {
	Type                  string     `json:"type"`
	Message               *piMessage `json:"message"`
	AssistantMessageEvent *struct {
		Type         string `json:"type"`
		ContentIndex int    `json:"contentIndex"`
		Delta        string `json:"delta"`
	} `json:"assistantMessageEvent"`
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	Args          json.RawMessage `json:"args"`
	PartialResult json.RawMessage `json:"partialResult"`
	Result        json.RawMessage `json:"result"`
	IsError       bool            `json:"isError"`
	Reason        string          `json:"reason"`
	Aborted       bool            `json:"aborted"`
	ErrorMessage  string          `json:"errorMessage"`
}

// Project maps one Pi frame. Frames Persona does not show yield no events.
func (p *Projection) Project(frame json.RawMessage, receivedAt time.Time) []engine.Event {
	out := &events{base: map[string]any{"receivedAt": receivedAt.UnixMilli()}}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame, &head) != nil {
		return nil
	}
	p.settled = head.Type == "agent_settled"
	// Extension UI frames reuse field names such as message with other types.
	if head.Type == "extension_ui_request" {
		if kind, payload, ok := extensionUI(frame); ok {
			out.add(kind, payload)
		}
		return out.list
	}
	var f piFrame
	if json.Unmarshal(frame, &f) != nil {
		return nil
	}
	switch f.Type {
	case "message_start":
		if f.Message != nil && f.Message.Role == "assistant" {
			p.sequence++
			p.messageID = "pi:" + timestampOr(f.Message.Timestamp, strconv.Itoa(p.sequence))
			out.add("runtime", map[string]any{"state": map[string]any{"processing": true}})
		}
	case "message_update":
		if part := f.AssistantMessageEvent; part != nil {
			switch part.Type {
			case "text_delta":
				out.add("text", map[string]any{"id": p.messageID, "text": part.Delta})
			case "thinking_delta":
				out.add("thinking", map[string]any{"id": p.messageID + ":" + strconv.Itoa(part.ContentIndex), "text": part.Delta})
			}
		}
	case "message_end":
		m := f.Message
		if m == nil || m.Role != "assistant" {
			break
		}
		if m.Usage != nil {
			out.add("usage", map[string]any{"usage": usage(m)})
		}
		switch m.StopReason {
		case "stop":
			p.failure = ""
		case "error":
			p.failure = m.ErrorMessage
			if p.failure == "" {
				p.failure = "回复失败，原因待确认"
			}
		case "aborted":
			p.failure = "工作已停止。"
		}
		if m.StopReason != "error" && m.StopReason != "aborted" {
			recordMessage(out, m, p.messageID)
		}
	case "tool_execution_start":
		out.add("tool", map[string]any{"id": f.ToolCallID, "name": f.ToolName, "input": objectOrEmpty(f.Args)})
	case "tool_execution_update", "tool_execution_end":
		partial := f.Type == "tool_execution_update"
		result := f.Result
		if partial {
			result = f.PartialResult
		}
		var r struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(result, &r)
		out.add("tool_result", map[string]any{
			"id":      f.ToolCallID,
			"name":    f.ToolName,
			"text":    textContent(r.Content),
			"failed":  !partial && f.IsError,
			"partial": partial,
			"native":  rawOrNull(result),
		})
	case "compaction_start":
		trigger := "auto"
		if f.Reason == "manual" {
			trigger = "manual"
		}
		out.add("compaction", map[string]any{"status": "running", "trigger": trigger})
	case "compaction_end":
		payload := map[string]any{"status": "completed"}
		if f.Aborted || f.ErrorMessage != "" {
			payload["status"] = "failed"
		}
		if f.ErrorMessage != "" {
			payload["error"] = f.ErrorMessage
		}
		out.add("compaction", payload)
	}
	return out.list
}

// Recovered projects the message entries of a get_entries page as recovered
// records, in order. Recovery only fills records; it never replays work.
func Recovered(entries []json.RawMessage, receivedAt time.Time) []engine.Event {
	var list []engine.Event
	for _, raw := range entries {
		var entry struct {
			Type    string     `json:"type"`
			ID      string     `json:"id"`
			Message *piMessage `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Type != "message" || entry.Message == nil {
			continue
		}
		m := entry.Message
		if m.Role == "assistant" && (m.StopReason == "error" || m.StopReason == "aborted") {
			continue
		}
		out := &events{base: map[string]any{"receivedAt": receivedAt.UnixMilli(), "recovered": true}}
		if timestampOr(m.Timestamp, "") != "" {
			out.base["timestamp"] = m.Timestamp
		}
		recordMessage(out, m, "pi:"+timestampOr(m.Timestamp, entry.ID))
		list = append(list, out.list...)
	}
	return list
}

// Context is the context reading from a get_session_stats result. window is
// used when Pi reports no context window; model is "provider/id" or "".
func Context(stats json.RawMessage, window *int, model string) engine.Event {
	var s struct {
		ContextUsage *struct {
			Tokens        *int `json:"tokens"`
			ContextWindow *int `json:"contextWindow"`
		} `json:"contextUsage"`
	}
	_ = json.Unmarshal(stats, &s)
	payload := map[string]any{
		"tokens": nil, "window": window, "source": "pi.get_session_stats",
		"basis": "window", "estimated": true, "model": nil,
	}
	if s.ContextUsage != nil {
		payload["tokens"] = s.ContextUsage.Tokens
		if s.ContextUsage.ContextWindow != nil {
			payload["window"] = s.ContextUsage.ContextWindow
		}
	}
	if model != "" {
		payload["model"] = model
	}
	return event("context", payload)
}

func recordMessage(out *events, m *piMessage, id string) {
	switch m.Role {
	case "assistant":
		var parts []piContent
		_ = json.Unmarshal(m.Content, &parts)
		for i, part := range parts {
			if part.Type == "thinking" {
				out.add("thinking", map[string]any{"id": id + ":" + strconv.Itoa(i), "text": part.Thinking, "replace": true})
			}
		}
		if text := textContent(m.Content); len(bytes.TrimSpace([]byte(text))) > 0 {
			out.add("message", map[string]any{"id": id, "text": text})
		}
		for _, part := range parts {
			if part.Type == "toolCall" {
				out.add("tool", map[string]any{"id": part.ID, "name": part.Name, "input": objectOrEmpty(part.Arguments)})
			}
		}
	case "toolResult":
		out.add("tool_result", map[string]any{
			"id":     m.ToolCallID,
			"name":   m.ToolName,
			"text":   textContent(m.Content),
			"failed": m.IsError,
			"native": map[string]any{"content": rawOrNull(m.Content), "details": rawOrNull(m.Details)},
		})
	}
}

func usage(m *piMessage) map[string]any {
	var cost *float64
	if m.Usage.Cost != nil {
		cost = m.Usage.Cost.Total
	}
	return map[string]any{
		"id":         "pi:" + timestampOr(m.Timestamp, ""),
		"source":     "pi.message_end",
		"provider":   nullIfEmpty(m.Provider),
		"model":      nullIfEmpty(m.Model),
		"input":      m.Usage.Input,
		"output":     m.Usage.Output,
		"cacheRead":  m.Usage.CacheRead,
		"cacheWrite": m.Usage.CacheWrite,
		"costUsd":    cost,
	}
}

// textContent joins the text parts of Pi content, which is a string or a list of parts.
func textContent(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []piContent
	_ = json.Unmarshal(content, &parts)
	// Blocks join with a line break, as textContent in pi.ts does.
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func timestampOr(raw json.RawMessage, fallback string) string {
	var number json.Number
	if json.Unmarshal(raw, &number) == nil && number != "" {
		return number.String()
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return text
	}
	return fallback
}

func objectOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage("{}")
	}
	return raw
}

func rawOrNull(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type events struct {
	base map[string]any
	list []engine.Event
}

func (e *events) add(kind string, payload map[string]any) {
	for k, v := range e.base {
		payload[k] = v
	}
	e.list = append(e.list, event(kind, payload))
}

func event(kind string, payload map[string]any) engine.Event {
	payload["type"] = kind
	data, _ := marshal(payload)
	return engine.Event{Type: kind, Data: data}
}

// marshal encodes like JSON.stringify for these payloads: no HTML escaping,
// so byte budgets match the TypeScript side.
func marshal(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
