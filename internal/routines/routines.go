// Package routines stores Bot routines and serves the routine_* product tools.
//
// Routines and their run receipts live in the session-scoped "persona.routines"
// document, matching the TypeScript backend (packages/agent/src/work/routine-state.ts),
// so imported databases keep them. This package owns the document; the routine runner
// starts tasks for enabled routines and leaves TaskID unset until it does.
package routines

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Personed/Persona/internal/approval"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

// Limits shared with the TypeScript backend (packages/agent/src/work/limits.ts).
const (
	// TitleLength bounds a routine title, in UTF-16 code units.
	TitleLength = 80
	// TextLength bounds instructions, conditions and report text, in UTF-16 code units.
	TextLength = 16000
	// KeyLength bounds a report's source key.
	KeyLength = 1024
	// MinExecutionMs and MaxExecutionMs bound a run's active execution time.
	MinExecutionMs = 1000
	MaxExecutionMs = 86_400_000
	// DefaultExecutionMs applies when a routine sets no execution time.
	DefaultExecutionMs = 900_000
)

// Document kinds, matching pi-durable defineDoc kinds.
const (
	DocKind       = "persona.routines"
	skillsDocKind = "persona.skills"
)

// Delivery modes.
const (
	Always      = "always"
	Conditional = "conditional"
)

// Routine is one scheduled piece of Bot work. NextAt is Unix milliseconds.
type Routine struct {
	ID           string   `json:"id"`
	BotID        int64    `json:"botId"`
	Title        string   `json:"title"`
	Instructions string   `json:"instructions"`
	Schedule     Schedule `json:"schedule"`
	SkillIDs     []string `json:"skillIds"`
	Enabled      bool     `json:"enabled"`
	NextAt       *int64   `json:"nextAt"`
	TaskID       *int64   `json:"taskId"`
	Revision     int      `json:"revision"`
	RequestID    string   `json:"requestId"`
	Source       string   `json:"source"`
	ExecutionMs  int64    `json:"executionMs,omitempty"`
	Delivery     string   `json:"delivery,omitempty"`
	Condition    string   `json:"condition,omitempty"`
	Baseline     string   `json:"baseline,omitempty"`
	NotifiedKey  string   `json:"notifiedKey,omitempty"`
}

// Change is one requested routine change. Nil fields keep the routine's current value.
type Change struct {
	// Action is create, update, pause, resume, run_now or delete.
	Action       string    `json:"action"`
	ID           string    `json:"id"`
	Revision     *int      `json:"revision"`
	Title        *string   `json:"title"`
	Instructions *string   `json:"instructions"`
	Schedule     *Schedule `json:"schedule"`
	SkillIDs     []string  `json:"skillIds"`
	Delivery     string    `json:"delivery"`
	Condition    *string   `json:"condition"`
	ExecutionMs  *int64    `json:"executionMs"`
}

// Report is a conditional run's result, submitted through routine_report.
type Report struct {
	Notify  bool   `json:"notify"`
	Text    string `json:"text"`
	Summary string `json:"summary"`
	Key     string `json:"key,omitempty"`
}

// Listing is what routine_list returns. Runs are stored run receipts, unchanged.
type Listing struct {
	Now      string            `json:"now"`
	TimeZone string            `json:"timeZone"`
	Items    []Routine         `json:"items"`
	Runs     []json.RawMessage `json:"runs"`
}

// ErrorCode classifies a routine Error like the TypeScript WorkError.
type ErrorCode string

// Error codes.
const (
	NotFound   ErrorCode = "NOT_FOUND"
	Conflict   ErrorCode = "CONFLICT"
	BadRequest ErrorCode = "BAD_REQUEST"
)

// Error is a validation or concurrency failure the caller can correct.
type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string { return e.Message }

// Runs stay raw so fields this package does not own survive a rewrite.
type document struct {
	Items []Routine         `json:"items"`
	Runs  []json.RawMessage `json:"runs"`
}

// skill is the part of a "persona.skills" record a routine binding checks.
type skill struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Revision    int     `json:"revision"`
	Content     string  `json:"content"`
	EnabledBots []int64 `json:"enabledBots"`
}

type run struct {
	ID       string  `json:"id"`
	BotID    int64   `json:"botId"`
	Status   string  `json:"status"`
	Delivery string  `json:"delivery"`
	Report   *Report `json:"report"`
}

// Service reads and writes routines through a store.Store.
type Service struct {
	// RunNow starts a test run and returns its task. The routine runner sets it
	// before the service is used; nil refuses run_now. A redelivered call repeats
	// its requestID, so RunNow must record the task it starts durably and return
	// that task, without starting another, when a Bot repeats a requestID.
	RunNow func(ctx context.Context, routine Routine, requestID string) (store.TaskID, error)
	// OnChange fires once after each routine write; the routine runner uses it
	// to recompute its next scheduled fire. nil ignores the notification.
	OnChange func()
	// Confirm asks the user to accept a routine change and reports the answer,
	// matching the TypeScript manage confirmation card. nil refuses the change.
	Confirm func(ctx context.Context, botID int64, request approval.Request) (bool, error)
	// Deliver posts a tool result's chat notice, like the TypeScript
	// details.notice path. nil skips delivery.
	Deliver func(ctx context.Context, call tools.Call, text string) error

	store store.Store
	now   func() time.Time
	// mu serializes read-modify-write cycles; store.Store has no conditional writes.
	mu sync.Mutex
}

// notify reports a changed routine document.
func (s *Service) notify() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

// NewService returns a Service. now defaults to time.Now.
func NewService(records store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: records, now: now}
}

var sessionScope = store.Scope{Kind: "session"}

// read decodes the session document of a kind. A missing document leaves target unchanged.
func (s *Service) read(ctx context.Context, kind string, target any) (store.Document, error) {
	meta, err := s.store.FindDocument(ctx, kind, sessionScope, "", store.Current)
	if errors.Is(err, store.ErrNotFound) {
		return store.Document{}, nil
	}
	if err != nil {
		return store.Document{}, err
	}
	value, err := s.store.Document(ctx, meta.ID, store.Current)
	if err != nil {
		return store.Document{}, err
	}
	if err := json.Unmarshal(value.Value, target); err != nil {
		return store.Document{}, fmt.Errorf("routines: decode %s: %w", kind, err)
	}
	return meta, nil
}

func (s *Service) save(ctx context.Context, meta store.Document, doc document) error {
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
		id, err := s.store.MintID(ctx)
		if err != nil {
			return err
		}
		meta = store.Document{ID: store.DocumentID(id), Kind: DocKind, Scope: sessionScope}
		write = store.Write{Type: store.WriteDocumentCreate, Document: &meta, Value: value}
	}
	_, _, err = s.store.Commit(ctx, []store.Batch{{write}})
	return err
}

// List returns the host time and zone with a Bot's routines and run receipts.
func (s *Service) List(ctx context.Context, botID int64) (Listing, error) {
	var doc document
	if _, err := s.read(ctx, DocKind, &doc); err != nil {
		return Listing{}, err
	}
	now := s.now()
	listing := Listing{
		Now:      now.UTC().Format("2006-01-02T15:04:05.000Z"),
		TimeZone: hostTimeZone(),
		Items:    []Routine{},
		Runs:     []json.RawMessage{},
	}
	for _, item := range doc.Items {
		if item.BotID == botID {
			listing.Items = append(listing.Items, item)
		}
	}
	for _, raw := range doc.Runs {
		var view run
		if err := json.Unmarshal(raw, &view); err != nil {
			return Listing{}, fmt.Errorf("routines: decode run: %w", err)
		}
		if view.BotID == botID {
			listing.Runs = append(listing.Runs, raw)
		}
	}
	return listing, nil
}

// Manage applies one change to a Bot's routine and returns the routine, or a status
// for delete and the started task for run_now. Source is "user" or
// "bot:<id>/tool:<call>". A repeated requestID returns the stored routine unchanged.
// Created and resumed routines become enabled; an update keeps the current state.
func (s *Service) Manage(ctx context.Context, botID int64, change Change, requestID, source string) (any, error) {
	result, test, err := s.apply(ctx, botID, change, requestID, source, true)
	if err != nil || test == nil {
		return result, err
	}
	if s.RunNow == nil {
		return nil, &Error{BadRequest, "定时执行服务不可用。"}
	}
	taskID, err := s.RunNow(ctx, *test, requestID)
	if err != nil {
		return nil, err
	}
	return map[string]store.TaskID{"taskId": taskID}, nil
}

// apply merges the change into the document. When confirm is set (routine_*
// tool calls), it asks the shared approver between preparing the merge and
// committing it, without holding the service lock: a pending card must not
// block the runner or other routines. The commit re-reads the document and
// re-checks dedupe and revision, so a change landed during the wait is a
// conflict rather than a silent overwrite. For run_now it returns the routine
// to run and commits nothing.
func (s *Service) apply(ctx context.Context, botID int64, change Change, requestID, source string, confirm bool) (any, *Routine, error) {
	s.mu.Lock()
	var doc document
	if _, err := s.read(ctx, DocKind, &doc); err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	item := Routine{BotID: botID}
	index := -1
	if change.Action == "create" {
		if repeated := slices.IndexFunc(doc.Items, func(item Routine) bool {
			return item.BotID == botID && item.RequestID == requestID
		}); repeated >= 0 {
			stored := doc.Items[repeated]
			s.mu.Unlock()
			return stored, nil, nil
		}
		id, err := s.store.MintID(ctx)
		if err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
		item.ID = fmt.Sprintf("routine-%d", id)
	} else {
		index = slices.IndexFunc(doc.Items, func(item Routine) bool {
			return item.BotID == botID && item.ID == change.ID
		})
		if index < 0 {
			s.mu.Unlock()
			return nil, nil, &Error{NotFound, "定时任务不存在。"}
		}
		item = doc.Items[index]
		if item.RequestID == requestID {
			s.mu.Unlock()
			return item, nil, nil
		}
		if change.Revision == nil || *change.Revision != item.Revision {
			s.mu.Unlock()
			return nil, nil, &Error{Conflict, "内容已更新，请刷新后重试。"}
		}
	}
	var previous *Routine
	if index >= 0 {
		kept := doc.Items[index]
		previous = &kept
	}
	switch change.Action {
	case "create", "update":
		item.Enabled = change.Action == "create" || item.Enabled
		item.Source = source
		if err := s.edit(ctx, &item, change, s.now()); err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
	case "pause", "resume", "run_now", "delete":
	default:
		s.mu.Unlock()
		return nil, nil, &Error{BadRequest, "参数无效：action 不受支持。"}
	}
	s.mu.Unlock()
	if confirm {
		confirmed, err := s.confirmChange(ctx, botID, change.Action, &item, previous, requestID)
		if err != nil {
			return nil, nil, err
		}
		if !confirmed {
			return map[string]string{"status": "declined"}, nil, nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc = document{}
	meta, err := s.read(ctx, DocKind, &doc)
	if err != nil {
		return nil, nil, err
	}
	index = -1
	if change.Action == "create" {
		if repeated := slices.IndexFunc(doc.Items, func(item Routine) bool {
			return item.BotID == botID && item.RequestID == requestID
		}); repeated >= 0 {
			return doc.Items[repeated], nil, nil
		}
	} else {
		index = slices.IndexFunc(doc.Items, func(item Routine) bool {
			return item.BotID == botID && item.ID == change.ID
		})
		if index < 0 {
			return nil, nil, &Error{NotFound, "定时任务不存在。"}
		}
		stored := doc.Items[index]
		if stored.RequestID == requestID {
			return stored, nil, nil
		}
		// item carries the revision read before the wait; a newer one means
		// another change landed meanwhile and this confirmation is stale.
		if stored.Revision != item.Revision {
			return nil, nil, &Error{Conflict, "内容已更新，请刷新后重试。"}
		}
		// Runner.finish commits a newer baseline without bumping the revision;
		// keep it unless this change itself reset the monitoring state.
		if previous != nil && item.Baseline == previous.Baseline && item.NotifiedKey == previous.NotifiedKey {
			item.Baseline, item.NotifiedKey = stored.Baseline, stored.NotifiedKey
		}
	}
	switch change.Action {
	case "run_now":
		return nil, &item, nil
	case "delete":
		doc.Items = slices.Delete(doc.Items, index, index+1)
		return map[string]string{"status": "deleted"}, nil, s.save(ctx, meta, doc)
	case "pause", "resume":
		item.Enabled = change.Action == "resume"
	}
	// The runner starts a task for the new revision; a task for an older one ends itself.
	item.NextAt, item.TaskID = nil, nil
	if item.Enabled {
		// now is read after any approval wait so an occurrence that passed
		// while pending does not fire immediately.
		now := s.now()
		if err := item.Schedule.Validate(now); err != nil {
			return nil, nil, err
		}
		next, err := item.Schedule.Next(now)
		if err != nil {
			return nil, nil, err
		}
		at := next.UnixMilli()
		item.NextAt = &at
	}
	item.Revision++
	item.RequestID = requestID
	if index < 0 {
		doc.Items = append(doc.Items, item)
	} else {
		doc.Items[index] = item
	}
	if err := s.save(ctx, meta, doc); err != nil {
		return item, nil, err
	}
	s.notify()
	return item, nil, nil
}

// edit merges a create or update into item and validates the result.
func (s *Service) edit(ctx context.Context, item *Routine, change Change, now time.Time) error {
	previous := *item
	if change.Title != nil {
		item.Title = *change.Title
	}
	if change.Instructions != nil {
		item.Instructions = *change.Instructions
	}
	if change.Schedule != nil {
		item.Schedule = *change.Schedule
	}
	if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.Instructions) == "" || item.Schedule.Type == "" {
		return &Error{BadRequest, "缺少名称、要求或执行时间。"}
	}
	if err := item.Schedule.Validate(now); err != nil {
		return err
	}
	if change.SkillIDs != nil {
		item.SkillIDs = change.SkillIDs
	}
	item.SkillIDs = unique(item.SkillIDs)
	if len(item.SkillIDs) > 0 {
		var skills struct {
			Items []skill `json:"items"`
		}
		if _, err := s.read(ctx, skillsDocKind, &skills); err != nil {
			return err
		}
		for _, id := range item.SkillIDs {
			if !slices.ContainsFunc(skills.Items, func(skill skill) bool {
				return skill.ID == id && slices.Contains(skill.EnabledBots, item.BotID)
			}) {
				return &Error{NotFound, "技能已删除或未为此Bot启用。"}
			}
		}
	}
	if change.ExecutionMs != nil {
		if *change.ExecutionMs < MinExecutionMs || *change.ExecutionMs > MaxExecutionMs {
			return &Error{BadRequest, "执行期限必须在 1 秒至 24 小时之间。"}
		}
		item.ExecutionMs = *change.ExecutionMs
	}
	item.ExecutionMs = cmp.Or(item.ExecutionMs, DefaultExecutionMs)
	item.Delivery = cmp.Or(change.Delivery, item.Delivery, Always)
	if item.Delivery != Always && item.Delivery != Conditional {
		return &Error{BadRequest, "参数无效：delivery 须为 always 或 conditional。"}
	}
	if change.Condition != nil {
		item.Condition = *change.Condition
	}
	var err error
	if item.Delivery == Always {
		item.Condition = ""
	} else if item.Condition, err = requiredText(item.Condition, TextLength); err != nil {
		return err
	}
	if item.Delivery != previous.Delivery || item.Condition != previous.Condition {
		item.Baseline, item.NotifiedKey = "", ""
	}
	if item.Title, err = requiredText(item.Title, TitleLength); err != nil {
		return err
	}
	item.Instructions, err = requiredText(item.Instructions, TextLength)
	return err
}

// Report records a conditional run's result for the Bot task that is running it.
// The runner commits the baseline and delivery after that task succeeds.
func (s *Service) Report(ctx context.Context, botID int64, taskID store.TaskID, report Report) error {
	none := &Error{BadRequest, "当前没有可提交结论的监控运行。"}
	task, err := s.store.Task(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return none
	}
	if err != nil {
		return err
	}
	if int64(task.ConversationID) != botID || task.State.Status == store.TaskTerminal {
		return &Error{Conflict, "本轮执行已结束。"}
	}
	var input struct {
		RoutineRunID string `json:"routineRunId"`
	}
	if err := json.Unmarshal(task.Input, &input); err != nil || input.RoutineRunID == "" {
		return none
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var doc document
	meta, err := s.read(ctx, DocKind, &doc)
	if err != nil {
		return err
	}
	for index, raw := range doc.Runs {
		var view run
		if err := json.Unmarshal(raw, &view); err != nil {
			return fmt.Errorf("routines: decode run: %w", err)
		}
		if view.ID != input.RoutineRunID || view.BotID != botID {
			continue
		}
		// Only the live turn calls routine_report, so any status before the
		// terminal ones accepts it: the runner's poll may not have marked the
		// receipt running yet, and an approval wait does not end the run.
		switch view.Status {
		case "done", "failed", "stopped":
			return none
		}
		if view.Delivery != Conditional {
			return none
		}
		if report.Notify && (strings.TrimSpace(report.Text) == "" || strings.TrimSpace(report.Key) == "") {
			return &Error{BadRequest, "通知需要正文与稳定事项标识。"}
		}
		if view.Report != nil {
			if *view.Report != report {
				return &Error{Conflict, "本轮已经提交检查结论。"}
			}
			return nil
		}
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Errorf("routines: decode run: %w", err)
		}
		if fields["report"], err = json.Marshal(report); err != nil {
			return err
		}
		if doc.Runs[index], err = json.Marshal(fields); err != nil {
			return err
		}
		return s.save(ctx, meta, doc)
	}
	return none
}

// actionLabels names each action the way the confirmation card does.
var actionLabels = map[string]string{
	"create": "创建", "update": "修改", "pause": "暂停",
	"resume": "恢复", "run_now": "立即执行", "delete": "删除",
}

// confirmChange asks the user to accept the merged change through the same
// request card the TypeScript routines manage composes.
func (s *Service) confirmChange(ctx context.Context, botID int64, action string, item, previous *Routine, requestID string) (bool, error) {
	if s.Confirm == nil {
		return false, &Error{BadRequest, "确认服务不可用。"}
	}
	label := actionLabels[action]
	withSkills := len(item.SkillIDs) > 0 || (previous != nil && len(previous.SkillIDs) > 0)
	fields := s.requestFields(ctx, item, withSkills)
	detail := item.Instructions
	if action == "update" && previous != nil {
		previousFields := s.requestFields(ctx, previous, withSkills)
		if item.Title != previous.Title {
			fields = append([]approval.Field{{Label: "任务名称", Value: item.Title}}, fields...)
			previousFields = append([]approval.Field{{Label: "任务名称", Value: previous.Title}}, previousFields...)
		}
		fields = diffFields(fields, previousFields)
		if item.Instructions == previous.Instructions {
			detail = ""
		}
	}
	return s.Confirm(ctx, botID, approval.Request{
		ID:           "routine:" + requestID,
		Title:        label + "定时：" + item.Title,
		Detail:       detail,
		Kind:         approval.Permission,
		Fields:       fields,
		ConfirmLabel: label + "任务",
	})
}

// requestFields renders a routine the way the confirmation card does.
func (s *Service) requestFields(ctx context.Context, item *Routine, withSkills bool) []approval.Field {
	when := item.Schedule.At
	if item.Schedule.Type == Cron {
		when = TimeLabel(item.Schedule.Expression)
	} else if at, err := time.Parse(time.RFC3339Nano, item.Schedule.At); err == nil {
		if loc, err := time.LoadLocation(item.Schedule.TimeZone); err == nil {
			when = at.In(loc).Format("2006/1/2 15:04:05")
		}
	}
	delivery := "每次交付"
	if item.Delivery == Conditional {
		delivery = "满足条件才通知：" + item.Condition
	}
	fields := []approval.Field{
		{Label: "执行时间", Value: when},
		{Label: "时区", Value: item.Schedule.TimeZone},
		{Label: "执行期限", Value: fmt.Sprintf("%d 分钟（不含等待）", cmp.Or(item.ExecutionMs, DefaultExecutionMs)/60_000)},
		{Label: "交付方式", Value: delivery},
	}
	if withSkills {
		fields = append(fields, approval.Field{Label: "使用技能", Value: s.skillNames(ctx, item.SkillIDs)})
	}
	return fields
}

// skillNames renders bound skills by name, falling back to their IDs.
func (s *Service) skillNames(ctx context.Context, ids []string) string {
	var skills struct {
		Items []skill `json:"items"`
	}
	// A missing or unreadable skills document leaves the IDs as their names.
	_, _ = s.read(ctx, skillsDocKind, &skills)
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		name := id
		for _, item := range skills.Items {
			if item.ID == id {
				name = item.Name
				break
			}
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "未绑定"
	}
	return strings.Join(names, "、")
}

// diffFields keeps only fields that changed, rendered "previous → next".
func diffFields(fields, previous []approval.Field) []approval.Field {
	result := make([]approval.Field, 0, len(fields))
	for _, field := range fields {
		index := slices.IndexFunc(previous, func(item approval.Field) bool {
			return item.Label == field.Label
		})
		switch {
		case index < 0:
			result = append(result, field)
		case previous[index].Value != field.Value:
			result = append(result, approval.Field{
				Label: field.Label,
				Value: previous[index].Value + " → " + field.Value,
			})
		}
	}
	return result
}

func unique(values []string) []string {
	result := []string{}
	for _, value := range values {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func textLength(value string) int { return len(utf16.Encode([]rune(value))) }

func requiredText(value string, maximum int) (string, error) {
	text := strings.TrimSpace(value)
	if length := textLength(text); length == 0 || length > maximum {
		return "", &Error{BadRequest, fmt.Sprintf("内容长度须为1至%d个字符。", maximum)}
	}
	return text, nil
}
