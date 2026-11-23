package agent

import (
	"context"
	"encoding/json"

	"github.com/Personed/Persona/internal/engine"
	"github.com/Personed/Persona/internal/engine/pi"
	"github.com/Personed/Persona/internal/store"
)

// runChat runs a turn of a Bot without an engine runtime view: the model
// messages are stored as entries of the task, and pi.live shows the run and
// the streamed answer while it is in flight.
func (r *Runtime) runChat(ctx context.Context, bot int64, w *worker, taskID store.TaskID, in taskInput) {
	id := int64(taskID)
	run, _ := json.Marshal(map[string]any{"taskId": id, "turnId": in.TurnID})
	uncertain := false
	err := r.h.Commit(ctx, func(tx *Tx) error {
		live, err := Create(tx, LiveDoc, bot)
		if err != nil {
			return err
		}
		task, err := tx.Task(taskID)
		if err != nil || task == nil {
			return err
		}
		if task.State.Status == store.TaskRunning {
			// Started by a previous process: never prompt it again.
			live.Run, live.Generation, live.Tools = nil, nil, nil
			r.fault(tx, task)
			uncertain = true
			return tx.SettleSubmission(in.SubmissionID, store.SubmissionUnanswered, nil, uncertainTurn)
		}
		live.Run, live.Generation = run, nil
		task.State.Status = store.TaskRunning
		started := r.ms()
		task.StartedAt = &started
		tx.PutTask(task)
		return nil
	})
	if err != nil || uncertain {
		return
	}
	answer, failure := r.executeChat(ctx, bot, w, taskID, in)
	w.mu.Lock()
	stopped := w.stopped
	w.mu.Unlock()
	_ = r.h.Commit(context.Background(), func(tx *Tx) error {
		live, err := Edit(tx, LiveDoc, bot)
		if err != nil {
			return err
		}
		live.Run, live.Generation, live.Tools = nil, nil, nil
		if failure != "" && answer == nil && !stopped {
			model, _ := json.Marshal([]map[string]any{{"role": "assistant", "content": []any{},
				"stopReason": "error", "errorMessage": failure, "timestamp": r.ms()}})
			if _, err := tx.AppendEntry(store.Entry{ConversationID: store.ConversationID(bot), Kind: modelEntry, Model: model, ByTaskID: &taskID}); err != nil {
				return err
			}
		}
		switch {
		case answer != nil && failure == "":
			err = tx.SettleSubmission(in.SubmissionID, store.SubmissionDone, answer, "")
		case stopped:
			err = tx.SettleSubmission(in.SubmissionID, store.SubmissionUnanswered, nil, "aborted")
		default:
			err = tx.SettleSubmission(in.SubmissionID, store.SubmissionUnanswered, nil, failure)
		}
		if err != nil {
			return err
		}
		task, err := tx.Task(taskID)
		if err != nil || task == nil {
			return err
		}
		r.terminal(tx, task)
		return nil
	})
}

func (r *Runtime) executeChat(ctx context.Context, bot int64, w *worker, taskID store.TaskID, in taskInput) (*store.EntryID, string) {
	if r.engine == nil {
		return nil, "引擎尚未配置。"
	}
	config, _, err := Snapshot(ctx, r.h, AgentDoc, bot)
	if err != nil {
		return nil, err.Error()
	}
	options := engine.SessionOptions{BotID: bot}
	if model := config.Model(); model != nil {
		options.Model = engine.Model{Provider: model.Provider, ModelID: model.ModelID}
	}
	session, err := r.engine.Start(ctx, options)
	if err != nil {
		return nil, err.Error()
	}
	defer func() { _ = session.Close() }()
	w.mu.Lock()
	w.session = session
	w.mu.Unlock()
	type result struct {
		answer  *store.EntryID
		failure string
	}
	done := make(chan result, 1)
	go func() {
		var projection pi.Projection
		var answer *store.EntryID
		for event := range session.Events() {
			if ctx.Err() != nil {
				return
			}
			projection.Project(event.Data, r.now())
			var frame struct {
				Type    string          `json:"type"`
				Message json.RawMessage `json:"message"`
			}
			_ = json.Unmarshal(event.Data, &frame)
			var message struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
			}
			_ = json.Unmarshal(frame.Message, &message)
			if message.Role == "assistant" && (frame.Type == "message_update" || frame.Type == "message_end") {
				if entry := r.chatMessage(ctx, bot, taskID, frame.Type == "message_end", frame.Message); entry != nil &&
					message.StopReason != "error" && message.StopReason != "aborted" {
					answer = entry
				}
			}
			if projection.Settled() {
				done <- result{answer, projection.Failure()}
				return
			}
		}
		done <- result{answer, "引擎已退出。执行结果未确认。"}
	}()
	if err := session.Prompt(ctx, engine.Prompt{RequestID: in.TurnID, Text: in.Text}); err != nil {
		return nil, err.Error()
	}
	select {
	case res := <-done:
		return res.answer, res.failure
	case <-ctx.Done():
		return nil, "工作已停止。"
	}
}

// chatMessage shows a streamed assistant message in pi.live, or stores a
// finished one as an entry of the task and returns its ID.
func (r *Runtime) chatMessage(ctx context.Context, bot int64, taskID store.TaskID, end bool, message json.RawMessage) *store.EntryID {
	var id *store.EntryID
	_ = r.h.Commit(ctx, func(tx *Tx) error {
		live, err := Edit(tx, LiveDoc, bot)
		if err != nil {
			return err
		}
		if !live.running() || live.runTask() != int64(taskID) {
			return nil
		}
		if !end {
			live.Generation = &struct {
				Message json.RawMessage `json:"message,omitempty"`
			}{Message: message}
			return nil
		}
		live.Generation = nil
		model, err := json.Marshal([]json.RawMessage{message})
		if err != nil {
			return err
		}
		entry, err := tx.AppendEntry(store.Entry{ConversationID: store.ConversationID(bot), Kind: modelEntry, Model: model, ByTaskID: &taskID})
		id = &entry
		return err
	})
	return id
}
