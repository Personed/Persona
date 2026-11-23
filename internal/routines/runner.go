package routines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/Personed/Persona/internal/approval"
	"github.com/Personed/Persona/internal/store"
)

// TaskKind is the durable task kind the runner executes.
const TaskKind = "persona.routine"

// Run modes, matching routine-task.ts.
const (
	modeScheduled = "scheduled"
	modeTest      = "test"
)

// taskInput is the persona.routine task's input, createRoutineTask's input.
type taskInput struct {
	ID        string `json:"id"`
	BotID     int64  `json:"botId"`
	Revision  int    `json:"revision"`
	At        int64  `json:"at"`
	Mode      string `json:"mode"`
	RequestID string `json:"requestId"`
}

// RunSubmit is the chat input a run places, Engines.submit in the TypeScript
// runtime: displayText replaces the scripted content on the stored user
// message and routineRunId marks the turn for routine delivery.
type RunSubmit struct {
	BotID        int64
	Text         string
	DisplayText  string
	RequestID    string
	RoutineRunID string
}

// Runner is the persona.routine task executor, routine-task.ts: test runs
// (work.routines.test and routine_manage run_now) start one durable task per
// request ID, the scheduler fires enabled routines at nextAt, and every run
// submits the receipt's scripted content through Runtime.submit and records
// the outcome back on the same receipt. The service owns the document; the
// runner owns the durable tasks that run routines.
type Runner struct {
	s      *Service
	submit func(ctx context.Context, in RunSubmit) (store.SubmissionID, error)
	// stop revokes a Bot's running turn when a run's execution deadline
	// expires, the shared Stop path; nil lets an expired run keep waiting.
	stop func(ctx context.Context, botID int64) error
	// approvals tells a running turn waiting on a user answer or approval
	// apart from execution, pausing its deadline; nil never waits.
	approvals *approval.Service
	wake      chan struct{}
	// ctx is the runner's own lifetime, set by Start: RunNow's request context
	// ends when the call returns, so executes must outlive it.
	ctx  context.Context
	cxMu sync.Mutex
}

// NewRunner attaches to the service: it sets RunNow and returns the runner
// Start must be called to schedule and recover tasks.
func NewRunner(s *Service, submit func(ctx context.Context, in RunSubmit) (store.SubmissionID, error), stop func(ctx context.Context, botID int64) error, approvals *approval.Service) *Runner {
	r := &Runner{s: s, submit: submit, stop: stop, approvals: approvals, wake: make(chan struct{}, 1)}
	s.RunNow = r.RunNow
	return r
}

// RunNow is Service.RunNow: one test task per request ID.
func (r *Runner) RunNow(ctx context.Context, routine Routine, requestID string) (store.TaskID, error) {
	return r.start(ctx, taskInput{ID: routine.ID, BotID: routine.BotID, Revision: routine.Revision,
		At: r.s.now().UnixMilli(), Mode: modeTest, RequestID: requestID})
}

// Start recovers unfinished tasks and schedules enabled routines until ctx
// ends.
func (r *Runner) Start(ctx context.Context) {
	r.cxMu.Lock()
	r.ctx = ctx
	r.cxMu.Unlock()
	tasks, err := r.s.store.Tasks(ctx, store.TaskQuery{Kind: TaskKind}, store.Scan{Limit: 500})
	for err == nil {
		for _, task := range tasks.Items {
			if task.State.Status != store.TaskTerminal {
				go r.execute(r.lifetime(ctx), task.ID)
			}
		}
		if tasks.Next == "" {
			break
		}
		tasks, err = r.s.store.Tasks(ctx, store.TaskQuery{Kind: TaskKind}, store.Scan{Limit: 500, Cursor: tasks.Next})
	}
	if err != nil {
		log.Printf("persona routines: recover: %v", err)
	}
	for {
		wait := time.Hour
		if fire, err := r.due(ctx); err != nil {
			log.Printf("persona routines: schedule: %v", err)
		} else if fire != nil {
			wait = time.Until(*fire)
			if wait <= 0 {
				wait = time.Millisecond
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.wake:
			timer.Stop()
			continue
		case <-timer.C:
		}
		if err := r.fire(ctx); err != nil {
			log.Printf("persona routines: schedule: %v", err)
		}
	}
}

// Kick asks the scheduler to recompute the next fire now, after a routine
// changed.
func (r *Runner) Kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// due returns the nearest scheduled fire time.
func (r *Runner) due(ctx context.Context) (*time.Time, error) {
	var doc document
	if _, err := r.s.read(ctx, DocKind, &doc); err != nil {
		return nil, err
	}
	var nearest *time.Time
	for i := range doc.Items {
		item := &doc.Items[i]
		if !item.Enabled || item.NextAt == nil || item.TaskID != nil {
			continue
		}
		at := time.UnixMilli(*item.NextAt)
		if nearest == nil || at.Before(*nearest) {
			nearest = &at
		}
	}
	return nearest, nil
}

// fire starts the tasks of enabled routines whose time came. A missed repeat
// coalesces into one run at the scheduled time.
func (r *Runner) fire(ctx context.Context) error {
	var doc document
	if _, err := r.s.read(ctx, DocKind, &doc); err != nil {
		return err
	}
	now := r.s.now().UnixMilli()
	for i := range doc.Items {
		item := doc.Items[i]
		if !item.Enabled || item.NextAt == nil || *item.NextAt > now || item.TaskID != nil {
			continue
		}
		if _, err := r.start(ctx, taskInput{ID: item.ID, BotID: item.BotID, Revision: item.Revision,
			At: *item.NextAt, Mode: modeScheduled, RequestID: item.RequestID}); err != nil {
			log.Printf("persona routines: fire %s: %v", item.ID, err)
		}
	}
	return nil
}

// start creates the durable task and its run receipt in one commit, as the
// TypeScript task's wait phase does. A non-terminal task with the same input
// request ID answers retries without starting again.
func (r *Runner) start(ctx context.Context, input taskInput) (store.TaskID, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	existing, err := r.s.store.Tasks(ctx, store.TaskQuery{ConversationID: store.ConversationID(input.BotID), Kind: TaskKind}, store.Scan{Limit: 200})
	if err != nil {
		return 0, err
	}
	for _, task := range existing.Items {
		if task.State.Status == store.TaskTerminal {
			continue
		}
		var in taskInput
		if json.Unmarshal(task.Input, &in) == nil && in.RequestID == input.RequestID {
			return task.ID, nil
		}
	}
	var doc document
	meta, err := r.s.read(ctx, DocKind, &doc)
	if err != nil {
		return 0, err
	}
	routine := findRoutine(doc.Items, input.BotID, input.ID)
	if routine == nil {
		return 0, &Error{NotFound, "定时任务不存在。"}
	}
	if input.Mode == modeScheduled && (!routine.Enabled || routine.Revision != input.Revision) {
		return 0, nil
	}
	taskID, err := r.s.store.MintID(ctx)
	if err != nil {
		return 0, err
	}
	now := r.s.now().UnixMilli()
	receipt, err := r.receipt(ctx, routine, input, taskID, now)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return 0, err
	}
	doc.Runs = append(doc.Runs, raw)
	if input.Mode == modeScheduled {
		index := slices.IndexFunc(doc.Items, func(item Routine) bool {
			return item.ID == input.ID && item.BotID == input.BotID
		})
		doc.Items[index].TaskID = &taskID
		doc.Items[index].NextAt = nil
	}
	if doc.Items == nil {
		doc.Items = []Routine{}
	}
	if doc.Runs == nil {
		doc.Runs = []json.RawMessage{}
	}
	value, err := json.Marshal(doc)
	if err != nil {
		return 0, err
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	task := store.Task{ID: store.TaskID(taskID), ConversationID: store.ConversationID(input.BotID),
		Kind: TaskKind, Input: inputJSON, Background: true}
	task.State.Status = store.TaskPending
	var write store.Write
	if meta.ID == 0 {
		meta = store.Document{ID: store.DocumentID(taskID), Kind: DocKind, Scope: sessionScope}
		write = store.Write{Type: store.WriteDocumentCreate, Document: &meta, Value: value}
	} else {
		write = store.Write{Type: store.WriteDocumentChange, Document: &meta, Value: value}
	}
	if _, _, err = r.s.store.Commit(ctx, []store.Batch{{write, {Type: store.WriteTask, Task: &task}}}); err != nil {
		return 0, err
	}
	go r.execute(r.lifetime(ctx), task.ID)
	return task.ID, nil
}

// lifetime returns the runner's context once Start ran, the caller's
// otherwise.
func (r *Runner) lifetime(fallback context.Context) context.Context {
	r.cxMu.Lock()
	defer r.cxMu.Unlock()
	if r.ctx != nil {
		return r.ctx
	}
	return context.WithoutCancel(fallback)
}

// findRoutine matches a routine by id and bot.
func findRoutine(items []Routine, botID int64, id string) *Routine {
	for i := range items {
		if items[i].ID == id && items[i].BotID == botID {
			return &items[i]
		}
	}
	return nil
}

// receipt builds the queued run record the task commits, routine-task.ts's
// doc.runs.push: the scripted content the engine answers, the delivery
// settings and the dedup request ID.
func (r *Runner) receipt(ctx context.Context, routine *Routine, input taskInput, taskID int64, now int64) (map[string]any, error) {
	content := fmt.Sprintf("Scheduled task: %s\n%s", routine.Title, routine.Instructions)
	delivery := routine.Delivery
	if delivery == "" {
		delivery = Always
	}
	var buildErr error
	if delivery == Conditional {
		baseline := routine.Baseline
		if baseline == "" {
			baseline = "Unknown; establish the first baseline unless the explicit condition is already met."
		}
		content += fmt.Sprintf("\nMonitoring condition: %s\nPrevious successful baseline: %s\nUse routine_report once after checking evidence. Do not send ordinary chat updates. Its text is the single notification when notify is true. Reuse existing file references for attachments.",
			routine.Condition, baseline)
	}
	var skills struct {
		Items []skill `json:"items"`
	}
	if _, err := r.s.read(ctx, skillsDocKind, &skills); err != nil {
		return nil, err
	}
	for _, id := range routine.SkillIDs {
		index := slices.IndexFunc(skills.Items, func(skill skill) bool {
			return skill.ID == id && slices.Contains(skill.EnabledBots, routine.BotID)
		})
		if index < 0 {
			buildErr = &Error{BadRequest, "绑定技能已删除或未启用。请修改任务后重试。"}
			break
		}
		skill := skills.Items[index]
		content += fmt.Sprintf("\n\nUse enabled skill %s (revision %d). Read skill_read with id \"%s\" for supporting files:\n%s",
			skill.Name, skill.Revision, skill.ID, skill.Content)
	}
	next, _ := routine.Schedule.Next(time.UnixMilli(input.At))
	receipt := map[string]any{
		"id":           fmt.Sprintf("%d:%d", taskID, input.At),
		"routineId":    routine.ID,
		"botId":        routine.BotID,
		"title":        routine.Title,
		"mode":         input.Mode,
		"revision":     routine.Revision,
		"delivery":     delivery,
		"scheduledAt":  input.At,
		"firedAt":      now,
		"finishedAt":   nil,
		"coalesced":    routine.Schedule.Type == "cron" && next != nil && next.UnixMilli() <= now,
		"status":       "queued",
		"error":        nil,
		"content":      content,
		"requestId":    fmt.Sprintf("routine:%s:%d", input.RequestID, input.At),
		"submissionId": nil,
		"executionMs":  DefaultExecutionMs,
		"activeMs":     0,
		"waitingMs":    0,
		"changedAt":    now,
	}
	if routine.ExecutionMs > 0 {
		receipt["executionMs"] = routine.ExecutionMs
	}
	if routine.Condition != "" {
		receipt["condition"] = routine.Condition
	}
	if buildErr != nil {
		receipt["error"] = buildErr.Error()
	}
	return receipt, nil
}

// execute runs one persona.routine task to its terminal state: queued →
// running, the Runtime.submit round trip, then the receipt's final status and
// the routine's next fire.
func (r *Runner) execute(ctx context.Context, taskID store.TaskID) {
	task, err := r.s.store.Task(ctx, taskID)
	if err != nil || task.State.Status == store.TaskTerminal {
		return
	}
	var in taskInput
	if json.Unmarshal(task.Input, &in) != nil {
		return
	}
	runID := fmt.Sprintf("%d:%d", taskID, in.At)
	var pending []store.Write
	if in.Mode == modeScheduled {
		if skip, err := r.editRun(ctx, runID, func(_ map[string]any, doc *document) (bool, error) {
			routine := findRoutine(doc.Items, in.BotID, in.ID)
			return routine == nil || !routine.Enabled || routine.Revision != in.Revision, nil
		}); err != nil {
			log.Printf("persona routines: run %s: %v", runID, err)
			return
		} else if skip {
			r.terminal(ctx, taskID)
			return
		}
	}
	status, runErr := r.roundtrip(ctx, in, runID)
	if err := r.finish(ctx, in, runID, status, runErr, &pending); err != nil {
		log.Printf("persona routines: run %s: %v", runID, err)
	}
	r.terminal(ctx, taskID)
}

// roundtrip submits the run's scripted content and waits for the submission,
// submitRun in routine-task.ts. The execution deadline revokes the turn
// through the shared Stop path.
func (r *Runner) roundtrip(ctx context.Context, in taskInput, runID string) (string, string) {
	run, err := r.readRun(ctx, runID)
	if err != nil {
		return "failed", "任务提交失败。请检查 Bot 配置。"
	}
	if message, _ := run["error"].(string); message != "" {
		return "failed", message
	}
	title, _ := run["title"].(string)
	content, _ := run["content"].(string)
	requestID, _ := run["requestId"].(string)
	display := "定时任务"
	if in.Mode == modeTest {
		display = "试跑"
	}
	subID, err := r.submit(ctx, RunSubmit{BotID: in.BotID, Text: content,
		DisplayText: display + "：" + title, RequestID: requestID, RoutineRunID: runID})
	if err != nil {
		return "failed", err.Error()
	}
	if _, err := r.editRun(ctx, runID, func(run map[string]any, _ *document) (bool, error) {
		run["submissionId"] = int64(subID)
		run["turnId"] = fmt.Sprintf("engine:%d", int64(subID))
		return false, nil
	}); err != nil {
		log.Printf("persona routines: run %s: %v", runID, err)
	}
	executionMs, _ := run["executionMs"].(float64)
	if executionMs <= 0 {
		executionMs = DefaultExecutionMs
	}
	// The deadline charges only active execution, routine-task.ts's
	// activeMs/waitingMs: a run queued behind chat or waiting on a user answer
	// or approval does not burn execution time, so expiry can only revoke the
	// run's own turn, never an unrelated one.
	var charged int64
	var engineTask store.TaskID
	tracked := "queued"
	lastTick := r.s.now()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		sub, err := r.s.store.Submission(ctx, subID)
		if err == nil {
			switch sub.Status {
			case store.SubmissionDone:
				return "done", ""
			case store.SubmissionUnanswered:
				if sub.Reason == "aborted" {
					return "stopped", ""
				}
				if sub.Reason == "unanswered" {
					return "failed", "unanswered"
				}
				return "failed", "已停止。"
			}
		}
		now := r.s.now()
		state := "queued"
		if engineTask == 0 {
			engineTask = r.engineTask(ctx, in.BotID, subID)
		}
		if engineTask != 0 {
			if active, err := r.taskActive(ctx, in.BotID, engineTask); err == nil && active {
				state = "running"
			}
			if state == "running" && r.approvals != nil {
				if pending, err := r.approvals.Pending(ctx, in.BotID); err == nil && len(pending) > 0 {
					state = "waiting_approval"
				}
			}
		}
		if state == "running" {
			charged += now.Sub(lastTick).Milliseconds()
		}
		lastTick = now
		if state != tracked {
			next := state
			if _, err := r.editRun(ctx, runID, func(run map[string]any, _ *document) (bool, error) {
				transition(run, next, now.UnixMilli())
				return false, nil
			}); err == nil {
				tracked = state
			}
		}
		if charged >= int64(executionMs) {
			if r.stop != nil {
				_ = r.stop(ctx, in.BotID)
			}
			return "stopped", ""
		}
		select {
		case <-ctx.Done():
			return "stopped", ""
		case <-tick.C:
		}
	}
}

// engineTask resolves the run's engine turn task once its submission is
// admitted; its kind is agent.EngineTurn, which routines cannot import.
func (r *Runner) engineTask(ctx context.Context, botID int64, subID store.SubmissionID) store.TaskID {
	tasks, err := r.s.store.Tasks(ctx, store.TaskQuery{
		ConversationID: store.ConversationID(botID), Kind: "persona.engine-turn"}, store.Scan{Limit: 200})
	if err != nil {
		return 0
	}
	for _, task := range tasks.Items {
		if task.State.Status == store.TaskTerminal {
			continue
		}
		var input struct {
			SubmissionID store.SubmissionID `json:"submissionId"`
		}
		if json.Unmarshal(task.Input, &input) == nil && input.SubmissionID == subID {
			return task.ID
		}
	}
	return 0
}

// taskActive reports whether the run's engine turn is the Bot's active task,
// reading the conversation's persona.engine document.
func (r *Runner) taskActive(ctx context.Context, botID int64, taskID store.TaskID) (bool, error) {
	meta, err := r.s.store.FindDocument(ctx, "persona.engine",
		store.Scope{Kind: "conversation", ConversationID: store.ConversationID(botID)}, "", store.Current)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	value, err := r.s.store.Document(ctx, meta.ID, store.Current)
	if err != nil {
		return false, err
	}
	var doc struct {
		ActiveID *int64 `json:"activeId"`
	}
	if err := json.Unmarshal(value.Value, &doc); err != nil {
		return false, err
	}
	return doc.ActiveID != nil && *doc.ActiveID == int64(taskID), nil
}

// finish settles the run receipt and reschedules or disables the routine,
// routine-task.ts's final commit plus the conditional delivery rules.
func (r *Runner) finish(ctx context.Context, in taskInput, runID, status, runErr string, _ *[]store.Write) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var doc document
	meta, err := r.s.read(ctx, DocKind, &doc)
	if err != nil {
		return err
	}
	now := r.s.now().UnixMilli()
	var extra []store.Write
	for index, raw := range doc.Runs {
		var run map[string]any
		if err := json.Unmarshal(raw, &run); err != nil {
			return fmt.Errorf("routines: decode run: %w", err)
		}
		if run["id"] != runID {
			continue
		}
		if current, _ := run["status"].(string); current == "done" || current == "failed" || current == "stopped" {
			return nil
		}
		botID := in.BotID
		routine := findRoutine(doc.Items, botID, in.ID)
		if delivery, _ := run["delivery"].(string); delivery == Conditional && status == "done" {
			report, _ := run["report"].(map[string]any)
			if report == nil {
				status = "failed"
				runErr = "监控未提交有效检查结论。"
			} else {
				run["delivered"] = false
				if routine != nil && routine.Revision == in.Revision {
					summary, _ := report["summary"].(string)
					routine.Baseline = summary
					notify, _ := report["notify"].(bool)
					key, _ := report["key"].(string)
					if notify && key != routine.NotifiedKey {
						run["delivered"] = true
						routine.NotifiedKey = key
						entry, err := r.stored(ctx, run, fmt.Sprintf("routine-result:%s", runID), report["text"], now)
						if err != nil {
							return err
						}
						extra = append(extra, store.Write{Type: store.WriteEntry, Entry: entry})
					}
				}
			}
		}
		if delivery, _ := run["delivery"].(string); delivery == Conditional && (status == "failed" || status == "stopped") {
			entry, err := r.stored(ctx, run, fmt.Sprintf("routine-status:%s", runID), first(runErr, "监控已停止。"), now)
			if err != nil {
				return err
			}
			extra = append(extra, store.Write{Type: store.WriteEntry, Entry: entry})
		}
		transition(run, status, now)
		run["error"] = nil
		if runErr != "" {
			run["error"] = runErr
		}
		run["finishedAt"] = now
		if doc.Runs[index], err = json.Marshal(run); err != nil {
			return err
		}
		if routine != nil {
			routine.TaskID = nil
			if in.Mode == modeScheduled {
				if routine.Enabled && routine.Revision == in.Revision {
					if next, err := routine.Schedule.Next(time.UnixMilli(now)); err == nil && next != nil {
						at := next.UnixMilli()
						routine.NextAt = &at
					} else {
						routine.Enabled = false
					}
				}
			}
		}
		break
	}
	if doc.Items == nil {
		doc.Items = []Routine{}
	}
	if doc.Runs == nil {
		doc.Runs = []json.RawMessage{}
	}
	value, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	write := store.Write{Type: store.WriteDocumentChange, Document: &meta, Value: value}
	if meta.ID == 0 {
		meta = store.Document{ID: store.DocumentID(r.s.mustMint(ctx)), Kind: DocKind, Scope: sessionScope}
		write = store.Write{Type: store.WriteDocumentCreate, Document: &meta, Value: value}
	}
	batch := append(store.Batch{write}, extra...)
	_, _, err = r.s.store.Commit(ctx, []store.Batch{batch})
	if err == nil && in.Mode == modeScheduled {
		// The scheduler may be sleeping until the old nextAt; wake it for the
		// new one so a recurring routine does not skip its next fire.
		r.Kick()
	}
	return err
}

// stored builds the persona.engine-message entry of a conditional run's
// routine-result or routine-status card, stored in routine-task.ts.
func (r *Runner) stored(ctx context.Context, run map[string]any, id string, text any, now int64) (*store.Entry, error) {
	entryID, err := r.s.store.MintID(ctx)
	if err != nil {
		return nil, err
	}
	botID, _ := run["botId"].(float64)
	data, err := json.Marshal(map[string]any{
		"id":        id,
		"turnId":    run["turnId"],
		"role":      "assistant",
		"stopped":   false,
		"routine":   map[string]any{"runId": run["id"], "title": run["title"], "scheduledAt": run["scheduledAt"], "turnId": run["turnId"]},
		"timestamp": now,
		"content":   []map[string]any{{"type": "text", "text": text, "key": run["id"]}},
	})
	if err != nil {
		return nil, err
	}
	return &store.Entry{ID: store.EntryID(entryID), ConversationID: store.ConversationID(int64(botID)),
		Kind: "persona.engine-message", Data: data}, nil
}

// readRun decodes one run receipt.
func (r *Runner) readRun(ctx context.Context, runID string) (map[string]any, error) {
	var doc document
	if _, err := r.s.read(ctx, DocKind, &doc); err != nil {
		return nil, err
	}
	for _, raw := range doc.Runs {
		var run map[string]any
		if json.Unmarshal(raw, &run) == nil && run["id"] == runID {
			return run, nil
		}
	}
	return nil, errors.New("routines: run not found")
}

// editRun applies change to one run receipt. Skip answers true to abort the
// task without touching the receipt.
func (r *Runner) editRun(ctx context.Context, runID string, change func(run map[string]any, doc *document) (skip bool, err error)) (bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var doc document
	meta, err := r.s.read(ctx, DocKind, &doc)
	if err != nil {
		return false, err
	}
	for index, raw := range doc.Runs {
		var run map[string]any
		if err := json.Unmarshal(raw, &run); err != nil {
			return false, fmt.Errorf("routines: decode run: %w", err)
		}
		if run["id"] != runID {
			continue
		}
		skip, err := change(run, &doc)
		if err != nil || skip {
			return skip, err
		}
		if doc.Runs[index], err = json.Marshal(run); err != nil {
			return false, err
		}
		return false, r.s.save(ctx, meta, doc)
	}
	return false, errors.New("routines: run not found")
}

// terminal marks the task done, the executor's outcome write.
func (r *Runner) terminal(ctx context.Context, taskID store.TaskID) {
	task, err := r.s.store.Task(ctx, taskID)
	if err != nil || task.State.Status == store.TaskTerminal {
		return
	}
	task.State.Status = store.TaskTerminal
	task.State.Outcome = &store.TaskOutcome{Status: "completed"}
	ended := r.s.now().UnixMilli()
	task.EndedAt = &ended
	if _, _, err := r.s.store.Commit(ctx, []store.Batch{{{Type: store.WriteTask, Task: &task}}}); err != nil {
		log.Printf("persona routines: task %d: %v", taskID, err)
	}
}

// mustMint keeps receipt creation atomic; callers checked the error path.
func (s *Service) mustMint(ctx context.Context) int64 {
	id, err := s.store.MintID(ctx)
	if err != nil {
		return 0
	}
	return id
}

// transition updates a run receipt's status and duration counters,
// transitionRun in routine-task.ts.
func transition(run map[string]any, status string, now int64) {
	changed, _ := run["changedAt"].(float64)
	delta := float64(now) - changed
	if delta < 0 {
		delta = 0
	}
	switch run["status"] {
	case "running":
		active, _ := run["activeMs"].(float64)
		run["activeMs"] = active + delta
	case "waiting_answer", "waiting_approval":
		waiting, _ := run["waitingMs"].(float64)
		run["waitingMs"] = waiting + delta
	}
	run["status"] = status
	run["changedAt"] = now
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
