package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/Personed/Persona/internal/store"
)

// flushDelay batches streamed text before it is written, engineLimits.flushMs.
const flushDelay = 50 * time.Millisecond

// projection writes one engine task's native events into the engine
// document, EngineProjection in projection.ts. Events of a turn that is no
// longer active are dropped.
type projection struct {
	r     *Runtime
	bot   int64
	task  int64
	turn  string
	entry store.EntryID

	mu         sync.Mutex
	write      sync.Mutex
	messages   []*Draft
	changed    []*Draft
	events     []map[string]any
	processing bool
	timer      *time.Timer
}

var bufferedEvents = []string{"usage", "runtime", "context", "compaction", "session", "accepted"}
var contentEvents = []string{"tool", "tool_result", "text", "thinking", "message"}

func (p *projection) emit(ctx context.Context, event map[string]any) {
	kind, _ := event["type"].(string)
	now := p.r.ms()
	if at, ok := number(event["receivedAt"]); ok {
		now = at
	}
	p.mu.Lock()
	if kind == "runtime" {
		if state, _ := event["state"].(map[string]any); state != nil {
			if processing, ok := state["processing"].(bool); ok {
				p.processing = processing
			}
		}
	}
	if slices.Contains(bufferedEvents, kind) {
		p.events = append(p.events, event)
		p.mu.Unlock()
		p.flush(ctx)
		return
	}
	id, hasID := event["id"].(string)
	content := hasID && slices.Contains(contentEvents, kind)
	var key string
	switch {
	case content && kind == "thinking":
		key = id + ":thinking"
	case content && kind == "tool_result":
		key = id + ":result"
	case content:
		key = id + ":message"
	case hasID:
		key = fmt.Sprintf("activity:%s:%s", kind, id)
	default:
		key = fmt.Sprintf("activity:%d", len(p.messages))
	}
	messageID := p.turn + ":" + key
	var message *Draft
	for _, item := range p.messages {
		if item.ID == messageID {
			message = item
		}
	}
	if message == nil {
		role := "assistant"
		if kind == "tool_result" {
			role = "toolResult"
		}
		received := now
		message = &Draft{ID: messageID, TurnID: p.turn, Role: role, Content: []map[string]any{}, Timestamp: now, ReceivedAt: &received}
		p.messages = append(p.messages, message)
	}
	if native, ok := event["native"]; ok && native != nil {
		message.Native, _ = json.Marshal(native)
	}
	if content {
		for _, previous := range p.messages {
			if previous != message && previous.Pending != nil && *previous.Pending && !hasToolCall(previous) {
				done := false
				previous.Pending = &done
				if previous.EndedAt == nil {
					ended := now
					previous.EndedAt = &ended
				}
				p.mark(previous)
			}
		}
	}
	text, _ := event["text"].(string)
	switch kind {
	case "tool":
		message.Content = []map[string]any{{"type": "toolCall", "id": id, "name": event["name"], "arguments": event["input"], "key": messageID}}
	case "tool_result":
		partial, _ := event["partial"].(bool)
		failed, _ := event["failed"].(bool)
		name, _ := event["name"].(string)
		updated := now
		message.ToolCallID, message.ToolName, message.IsError, message.UpdatedAt = id, name, &failed, &updated
		switch {
		case partial:
			message.ToolStatus = "running"
		case failed:
			message.ToolStatus = "failed"
		default:
			message.ToolStatus = "done"
			ended := now
			message.EndedAt = &ended
		}
		message.Content = []map[string]any{{"type": "text", "text": text, "key": messageID}}
	case "message":
		if message.Pending == nil || *message.Pending {
			message.Timestamp = now
		}
		done, ended := false, now
		message.Content = []map[string]any{{"type": "text", "text": text, "key": messageID}}
		message.Pending, message.EndedAt = &done, &ended
	case "text", "thinking":
		replace, _ := event["replace"].(bool)
		if message.Pending != nil && !*message.Pending && !replace {
			p.mu.Unlock()
			return
		}
		previous := ""
		if len(message.Content) > 0 && !replace {
			if kind == "text" {
				previous, _ = message.Content[0]["text"].(string)
			} else {
				previous, _ = message.Content[0]["thinking"].(string)
			}
		}
		if kind == "text" {
			message.Content = []map[string]any{{"type": "text", "text": previous + text, "key": messageID}}
		} else {
			message.Content = []map[string]any{{"type": "thinking", "thinking": previous + text, "key": messageID}}
		}
		pending := true
		message.Pending = &pending
	default:
		message.Activity, _ = json.Marshal(event)
	}
	p.mark(message)
	streamed := kind == "text" || kind == "thinking"
	if streamed && p.timer == nil {
		p.timer = time.AfterFunc(flushDelay, func() { p.flush(context.Background()) })
	}
	p.mu.Unlock()
	if !streamed {
		p.flush(ctx)
	}
}

func (p *projection) mark(message *Draft) {
	if !slices.Contains(p.changed, message) {
		p.changed = append(p.changed, message)
	}
}

func hasToolCall(m *Draft) bool {
	for _, part := range m.Content {
		if part["type"] == "toolCall" {
			return true
		}
	}
	return false
}

func number(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// flush writes the changed messages and buffered events.
func (p *projection) flush(ctx context.Context) {
	p.write.Lock()
	defer p.write.Unlock()
	for {
		p.mu.Lock()
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		if len(p.changed) == 0 && len(p.events) == 0 {
			p.mu.Unlock()
			return
		}
		drafts := make([]*Draft, len(p.changed))
		for i, message := range p.changed {
			copied := *message
			drafts[i] = &copied
		}
		events, processing := p.events, p.processing
		p.changed, p.events = nil, nil
		p.mu.Unlock()
		_ = p.r.h.Commit(ctx, func(tx *Tx) error {
			doc, err := Edit(tx, EngineDoc, p.bot)
			if err != nil {
				return err
			}
			if doc.ActiveID == nil || *doc.ActiveID != p.task {
				return nil
			}
			if doc.Runtime == nil {
				doc.Runtime = map[string]any{}
			}
			for _, event := range events {
				if err := p.apply(tx, doc, event); err != nil {
					return err
				}
			}
			for _, draft := range drafts {
				index := slices.IndexFunc(doc.Drafts, func(item *Draft) bool { return item.ID == draft.ID })
				if index < 0 {
					doc.Drafts = append(doc.Drafts, draft)
				} else {
					doc.Drafts[index] = draft
				}
			}
			doc.Runtime["processing"] = processing
			return nil
		})
	}
}

func (p *projection) apply(tx *Tx, doc *EngineState, event map[string]any) error {
	switch event["type"] {
	case "usage":
		usage, _ := event["usage"].(map[string]any)
		if model, _ := usage["model"].(string); doc.Run != nil && model != "" {
			doc.Run.Model = &model
		}
		items, err := Create(tx, UsageTaskDoc, p.task)
		if err != nil {
			return err
		}
		id, _ := usage["id"].(string)
		items.Items[id], _ = json.Marshal(usage)
	case "context", "compaction":
		projectContext(doc, event, p.r.ms())
	case "runtime":
		state, _ := event["state"].(map[string]any)
		for key, value := range state {
			if key == "notices" {
				notices, _ := doc.Runtime["notices"].(map[string]any)
				if notices == nil {
					notices = map[string]any{}
				}
				for k, v := range value.(map[string]any) {
					notices[k] = v
				}
				value = notices
			}
			doc.Runtime[key] = value
		}
	case "session":
		id, _ := event["id"].(string)
		if doc.SessionID == nil || *doc.SessionID != id {
			runtime := map[string]any{}
			if controls, ok := doc.Runtime["controls"]; ok {
				runtime["controls"] = controls
			}
			doc.Runtime = runtime
		}
		doc.SessionID = &id
	case "accepted":
		doc.Dispatch = "accepted"
		id, _ := event["id"].(string)
		doc.TurnID = nil
		if id != "" {
			doc.TurnID = &id
			if doc.Run != nil {
				doc.Run.NativeID = id
			}
		}
	}
	return nil
}

// projectContext records a context reading or compaction state, context.ts.
func projectContext(doc *EngineState, event map[string]any, now int64) {
	state := doc.Runtime
	if event["type"] == "context" {
		tokens, window := event["tokens"], event["window"]
		if t, ok := number(tokens); ok && t < 0 {
			return
		}
		if w, ok := number(window); ok && w <= 0 {
			return
		}
		model := event["model"]
		if model == nil && doc.Config.Model != nil {
			model = *doc.Config.Model
		}
		state["contextTokens"], state["contextWindow"] = tokens, window
		state["context"] = map[string]any{
			"source": event["source"], "basis": event["basis"], "estimated": event["estimated"],
			"observedAt": now, "harness": doc.Config.Provider, "sessionId": doc.SessionID,
			"model": model, "configuredModel": doc.Config.Model,
		}
		return
	}
	state["compaction"] = event["status"]
}
