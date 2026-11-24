package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Personed/Persona/internal/store"
)

// EngineView is the chat view of a Bot with an engine runtime.
type EngineView struct {
	ChatView
	Engine  EngineConfig   `json:"engine"`
	Runtime map[string]any `json:"runtime"`
	Error   *string        `json:"error"`
}

// View returns the Bot's chat page before the cursor, with the engine
// runtime state when the runtime is native, view in runtime/index.ts.
func (r *Runtime) View(ctx context.Context, bot int64, before string) (any, error) {
	if !r.native {
		return r.h.ReadChat(ctx, bot, before)
	}
	chat, err := r.h.ReadEnginePage(ctx, bot, before, enginePage)
	if err != nil {
		return nil, err
	}
	doc, err := r.engineDoc(ctx, bot)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, message := range chat.Messages {
		known[messageID(message)] = true
	}
	for _, draft := range doc.Drafts {
		if !known[draft.ID] {
			chat.Messages = append(chat.Messages, *draft)
		}
	}
	// Routine delivery, applyRoutineDelivery in routine-delivery.ts: a
	// routine run's input stays stored but hidden, and its answer carries the
	// routine source card; conditional runs hide everything else they wrote.
	runs, err := r.routineRuns(ctx, bot)
	if err != nil {
		return nil, err
	}
	applyRoutineDelivery(&chat, runs)
	processing, _ := doc.Runtime["processing"].(bool)
	chat.Busy = chat.Busy || len(doc.Tasks) > 0
	chat.Processing = len(chat.Approvals) == 0 && (chat.Processing || (doc.ActiveID != nil && processing))
	if doc.ActiveTurn != nil {
		chat.ActiveTurnID = doc.ActiveTurn
	}
	if doc.Config.Model != nil {
		if provider, model, ok := strings.Cut(*doc.Config.Model, "/"); ok {
			chat.Model = &ModelRef{Provider: provider, ModelID: model}
		}
	}
	if doc.Run != nil {
		runs := []json.RawMessage{}
		for _, run := range chat.Runs {
			var item struct {
				TurnID string `json:"turnId"`
			}
			if json.Unmarshal(run, &item) == nil && item.TurnID == doc.Run.TurnID {
				continue
			}
			runs = append(runs, run)
		}
		current, err := json.Marshal(doc.Run)
		if err != nil {
			return nil, err
		}
		chat.Runs = append(runs, current)
	}
	chat.Failed = chat.Failed || doc.Error != nil
	if doc.Config.Options == nil {
		doc.Config.Options = map[string]any{}
	}
	runtime := doc.Runtime
	if runtime == nil {
		runtime = map[string]any{}
	}
	return EngineView{ChatView: chat, Engine: doc.Config, Runtime: runtime, Error: doc.Error}, nil
}

// engineDoc reads the engine document, reconciling tasks that ended
// without finishing first.
func (r *Runtime) engineDoc(ctx context.Context, bot int64) (EngineState, error) {
	doc, _, err := Snapshot(ctx, r.h, EngineDoc, bot)
	if err != nil {
		return doc, err
	}
	for _, id := range doc.Tasks {
		task, err := r.h.store.Task(ctx, store.TaskID(id))
		if err != nil || task.State.Status != store.TaskTerminal {
			continue
		}
		err = r.h.Commit(ctx, func(tx *Tx) error {
			current, err := Edit(tx, EngineDoc, bot)
			if err != nil {
				return err
			}
			return r.reconcile(tx, store.ConversationID(bot), current)
		})
		if err != nil {
			return doc, err
		}
		doc, _, err = Snapshot(ctx, r.h, EngineDoc, bot)
		return doc, err
	}
	return doc, nil
}

func messageID(message any) string {
	switch m := message.(type) {
	case ChatMessage:
		return m.ID
	case Draft:
		return m.ID
	case storedMessage:
		var id string
		_ = json.Unmarshal(m.fields["id"], &id)
		return id
	}
	return ""
}

// Slim drops what a chat page leaves to message reads: tool result content
// and native details, image data and approval previews, slimChatView in
// runtime/index.ts.
func Slim(view any) (map[string]any, error) {
	data, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	if approvals, ok := out["approvals"].([]any); ok {
		for _, approval := range approvals {
			if request, ok := approval.(map[string]any); ok {
				delete(request, "preview")
			}
		}
	}
	if messages, ok := out["messages"].([]any); ok {
		for _, message := range messages {
			slimMessage(message)
		}
	}
	slimMessage(out["draft"])
	return out, nil
}

func slimMessage(value any) {
	message, ok := value.(map[string]any)
	if !ok {
		return
	}
	if message["role"] == "toolResult" {
		if native, ok := message["native"].(map[string]any); ok {
			slim := map[string]any{}
			for _, key := range []string{"type", "status", "exitCode", "durationMs"} {
				if v, ok := native[key]; ok {
					slim[key] = v
				}
			}
			message["native"] = slim
		}
		message["content"], message["detail"] = []any{}, true
		return
	}
	content, _ := message["content"].([]any)
	for _, part := range content {
		if p, ok := part.(map[string]any); ok && p["type"] == "image" {
			p["data"], message["detail"] = "", true
		}
	}
}

// Message reads one message in full: a stored message by its cursor, or an
// in-flight engine draft by ID.
func (r *Runtime) Message(ctx context.Context, bot int64, id, cursor string) (any, error) {
	if cursor == "" {
		if !r.native {
			return nil, nil
		}
		// A pending approval card is stored in persona.computer-requests, as
		// runtime/index.ts message() reads it before engine drafts.
		if view, err := r.h.conversationDocs(ctx, bot); err == nil {
			for _, raw := range view.approvals {
				if message := pendingApproval(raw, nil); message != nil && message["id"] == id {
					return message, nil
				}
			}
		}
		doc, _, err := Snapshot(ctx, r.h, EngineDoc, bot)
		if err != nil {
			return nil, err
		}
		for _, draft := range doc.Drafts {
			if draft.ID == id {
				return *draft, nil
			}
		}
		return nil, nil
	}
	head, _, _ := strings.Cut(cursor, ":")
	entryID, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return nil, nil
	}
	page, err := r.h.store.Entries(ctx, store.EntryQuery{ConversationID: store.ConversationID(bot), Min: store.EntryID(entryID), Max: store.EntryID(entryID)}, store.Scan{Limit: 1})
	if err != nil {
		return nil, err
	}
	view, err := r.h.conversationDocs(ctx, bot)
	if err != nil {
		return nil, err
	}
	view.entries = page.Items
	for _, message := range projectChat(view).Messages {
		if messageID(message) == id {
			return message, nil
		}
	}
	return nil, nil
}

// Updates streams the Bot's slim chat view: a snapshot, then for each change
// the changed state, changed messages and removed drafts, updates in
// runtime/index.ts.
func (r *Runtime) Updates(ctx context.Context, bot int64, send func(any) error) error {
	commits, err := r.h.Watch(ctx)
	if err != nil {
		return err
	}
	read := func() (map[string]any, error) {
		view, err := r.View(ctx, bot, "")
		if err != nil {
			return nil, err
		}
		return Slim(view)
	}
	previous, err := read()
	if err != nil {
		return err
	}
	if err := send(map[string]any{"type": "snapshot", "view": previous}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-commits:
			if !ok {
				return nil
			}
		}
		for drained := false; !drained; {
			select {
			case _, ok := <-commits:
				if !ok {
					return nil
				}
			default:
				drained = true
			}
		}
		view, err := read()
		if err != nil {
			return err
		}
		if update := diffViews(previous, view); update != nil {
			if err := send(update); err != nil {
				return err
			}
		}
		previous = view
	}
}

func diffViews(previous, view map[string]any) map[string]any {
	encode := func(v any) string {
		data, _ := json.Marshal(v)
		return string(data)
	}
	idOf := func(m any) (string, bool) {
		message, _ := m.(map[string]any)
		id, _ := message["id"].(string)
		_, cursor := message["cursor"]
		return id, cursor
	}
	oldMessages, _ := previous["messages"].([]any)
	newMessages, _ := view["messages"].([]any)
	known := map[string]string{}
	for _, message := range oldMessages {
		id, _ := idOf(message)
		known[id] = encode(message)
	}
	messages := []any{}
	retained := map[string]bool{}
	for _, message := range newMessages {
		id, _ := idOf(message)
		retained[id] = true
		if old, ok := known[id]; !ok || old != encode(message) {
			messages = append(messages, message)
		}
	}
	removed := []string{}
	for _, message := range oldMessages {
		if id, cursor := idOf(message); !cursor && !retained[id] {
			removed = append(removed, id)
		}
	}
	state := map[string]any{}
	for key, value := range view {
		if key == "messages" || key == "before" {
			continue
		}
		if encode(previous[key]) != encode(value) {
			state[key] = value
		}
	}
	if len(state) == 0 && len(messages) == 0 && len(removed) == 0 {
		return nil
	}
	return map[string]any{"type": "update", "state": state, "messages": messages, "removed": removed}
}

// routineRuns returns the Bot's routine run receipts.
func (r *Runtime) routineRuns(ctx context.Context, bot int64) ([]RoutineRun, error) {
	doc, ok, err := Snapshot(ctx, r.h, RoutinesDoc, 0)
	if err != nil || !ok {
		return nil, err
	}
	runs := []RoutineRun{}
	for _, run := range doc.Runs {
		if run.BotID == bot {
			runs = append(runs, run)
		}
	}
	return runs, nil
}

func rawOf(fields map[string]json.RawMessage, key string) (any, bool) {
	var value any
	raw, ok := fields[key]
	if !ok || json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	return value, true
}

func fieldIs(fields map[string]json.RawMessage, key, value string) bool {
	v, ok := rawOf(fields, key)
	s, _ := v.(string)
	return ok && s == value
}

func routineCard(fields map[string]json.RawMessage, key string, run RoutineRun) {
	source, _ := json.Marshal(map[string]any{
		"runId": run.ID, "title": run.Title, "scheduledAt": run.ScheduledAt})
	fields[key] = source
}

// applyRoutineDelivery mirrors routine-delivery.ts over the projected chat.
func applyRoutineDelivery(chat *ChatView, runs []RoutineRun) {
	for _, run := range runs {
		if run.RequestID == "" {
			continue
		}
		input := -1
		turnID := run.TurnID
		for i := range chat.Messages {
			m, ok := chat.Messages[i].(storedMessage)
			if !ok || !fieldIs(m.fields, "role", "user") {
				continue
			}
			matched := fieldIs(m.fields, "requestId", run.RequestID)
			if !matched {
				if v, ok := rawOf(m.fields, "routine"); ok {
					if card, ok := v.(map[string]any); ok && card["runId"] == run.ID {
						matched = true
					}
				}
			}
			if !matched {
				continue
			}
			input = i
			if turnID == "" {
				if v, ok := rawOf(m.fields, "turnId"); ok {
					turnID, _ = v.(string)
				}
			}
		}
		if input < 0 {
			continue
		}
		in := chat.Messages[input].(storedMessage)
		routineCard(in.fields, "routine", run)
		in.fields["hidden"], _ = json.Marshal(true)
		if run.Delivery == "conditional" {
			for i := range chat.Messages {
				m, ok := chat.Messages[i].(storedMessage)
				if !ok || !fieldIs(m.fields, "turnId", turnID) {
					continue
				}
				id, _ := rawOf(m.fields, "id")
				str, _ := id.(string)
				if str == "routine-result:"+run.ID || str == "routine-status:"+run.ID {
					continue
				}
				if _, has := m.fields["approval"]; has {
					continue
				}
				if _, has := m.fields["takeover"]; has {
					continue
				}
				m.fields["hidden"], _ = json.Marshal(true)
			}
			if chat.ActiveTurnID != nil && *chat.ActiveTurnID == turnID {
				chat.Processing = false
			}
			continue
		}
		for i := range chat.Messages {
			m, ok := chat.Messages[i].(storedMessage)
			if !ok || !fieldIs(m.fields, "role", "assistant") || !fieldIs(m.fields, "turnId", turnID) {
				continue
			}
			if _, has := m.fields["approval"]; has {
				continue
			}
			if _, has := m.fields["takeover"]; has {
				continue
			}
			content, _ := rawOf(m.fields, "content")
			parts, _ := content.([]any)
			text := false
			for _, part := range parts {
				p, _ := part.(map[string]any)
				if p["type"] == "text" && p["text"] != "" {
					text = true
				}
			}
			if !text {
				continue
			}
			routineCard(m.fields, "routine", run)
			break
		}
	}
}
