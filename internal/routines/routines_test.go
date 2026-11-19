package routines_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/routines"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

// start is Monday 2026-01-05 00:00 UTC.
var start = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

func newService(t *testing.T) (*routines.Service, *store.SQLite) {
	t.Helper()
	records, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "persona.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := records.Close(); err != nil {
			t.Error(err)
		}
	})
	return routines.NewService(records, func() time.Time { return start }), records
}

func code(err error) routines.ErrorCode {
	var routineError *routines.Error
	if errors.As(err, &routineError) {
		return routineError.Code
	}
	return ""
}

func pointer[T any](value T) *T { return &value }

var weekdays = routines.Schedule{Type: routines.Cron, Expression: "0 9 * * 1-5", TimeZone: "Asia/Shanghai"}

func create(t *testing.T, service *routines.Service, botID int64, requestID string) routines.Routine {
	t.Helper()
	result, err := service.Manage(t.Context(), botID, routines.Change{
		Action:       "create",
		Title:        pointer(" Morning brief "),
		Instructions: pointer("Summarize unread mail."),
		Schedule:     &weekdays,
	}, requestID, "user")
	if err != nil {
		t.Fatal(err)
	}
	return result.(routines.Routine)
}

func writeDocument(t *testing.T, records store.Store, kind, value string) {
	t.Helper()
	ctx := t.Context()
	write := store.Write{Type: store.WriteDocumentChange, Value: json.RawMessage(value)}
	meta, err := records.FindDocument(ctx, kind, store.Scope{Kind: "session"}, "", store.Current)
	if errors.Is(err, store.ErrNotFound) {
		id, mintErr := records.MintID(ctx)
		if mintErr != nil {
			t.Fatal(mintErr)
		}
		meta = store.Document{ID: store.DocumentID(id), Kind: kind, Scope: store.Scope{Kind: "session"}}
		write.Type = store.WriteDocumentCreate
	} else if err != nil {
		t.Fatal(err)
	}
	write.Document = &meta
	if _, _, err := records.Commit(ctx, []store.Batch{{write}}); err != nil {
		t.Fatal(err)
	}
}

func TestManageLifecycle(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	created := create(t, service, 1, "r1")
	next := time.Date(2026, 1, 5, 1, 0, 0, 0, time.UTC).UnixMilli()
	if created.ID == "" || created.Revision != 1 || created.Title != "Morning brief" || !created.Enabled ||
		created.NextAt == nil || *created.NextAt != next || created.TaskID != nil ||
		created.Delivery != routines.Always || created.ExecutionMs != routines.DefaultExecutionMs || created.Source != "user" {
		t.Fatalf("created = %+v", created)
	}
	if again := create(t, service, 1, "r1"); again.ID != created.ID {
		t.Fatalf("replayed create = %+v", again)
	}
	change := routines.Change{Action: "update", ID: created.ID, Revision: pointer(1), Delivery: routines.Conditional, Condition: pointer("A new invoice arrived.")}
	if _, err := service.Manage(ctx, 2, change, "r2", "user"); code(err) != routines.NotFound {
		t.Fatalf("other Bot update = %v", err)
	}
	result, err := service.Manage(ctx, 1, change, "r2", "user")
	if err != nil {
		t.Fatal(err)
	}
	updated := result.(routines.Routine)
	if updated.Revision != 2 || !updated.Enabled || updated.Title != created.Title || updated.Condition != "A new invoice arrived." {
		t.Fatalf("updated = %+v", updated)
	}
	if _, err := service.Manage(ctx, 1, routines.Change{Action: "pause", ID: created.ID, Revision: pointer(1)}, "r3", "user"); code(err) != routines.Conflict {
		t.Fatalf("stale pause = %v", err)
	}
	result, err = service.Manage(ctx, 1, routines.Change{Action: "pause", ID: created.ID, Revision: pointer(2)}, "r3", "user")
	if paused := result.(routines.Routine); err != nil || paused.Enabled || paused.NextAt != nil || paused.Revision != 3 {
		t.Fatalf("paused = %+v, %v", paused, err)
	}
	result, err = service.Manage(ctx, 1, routines.Change{Action: "update", ID: created.ID, Revision: pointer(3), Delivery: routines.Always}, "r4", "user")
	if always := result.(routines.Routine); err != nil || always.Enabled || always.Condition != "" {
		t.Fatalf("paused update = %+v, %v", always, err)
	}
	result, err = service.Manage(ctx, 1, routines.Change{Action: "resume", ID: created.ID, Revision: pointer(4)}, "r5", "user")
	if resumed := result.(routines.Routine); err != nil || !resumed.Enabled || resumed.NextAt == nil || resumed.Revision != 5 {
		t.Fatalf("resumed = %+v, %v", resumed, err)
	}

	runNow := routines.Change{Action: "run_now", ID: created.ID, Revision: pointer(5)}
	if _, err := service.Manage(ctx, 1, runNow, "r6", "user"); code(err) != routines.BadRequest {
		t.Fatalf("run_now without a runner = %v", err)
	}
	service.RunNow = func(_ context.Context, routine routines.Routine, requestID string) (store.TaskID, error) {
		if routine.ID != created.ID || requestID != "r6" {
			t.Errorf("RunNow(%+v, %q)", routine, requestID)
		}
		return 42, nil
	}
	if result, err := service.Manage(ctx, 1, runNow, "r6", "user"); err != nil || result.(map[string]store.TaskID)["taskId"] != 42 {
		t.Fatalf("run_now = %+v, %v", result, err)
	}

	if _, err := service.Manage(ctx, 1, routines.Change{Action: "delete", ID: created.ID, Revision: pointer(5)}, "r7", "user"); err != nil {
		t.Fatal(err)
	}
	if listing, err := service.List(ctx, 1); err != nil || len(listing.Items) != 0 {
		t.Fatalf("List after delete = %+v, %v", listing, err)
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	service, records := newService(t)
	valid := func() routines.Change {
		return routines.Change{Action: "create", Title: pointer("Check"), Instructions: pointer("Look."), Schedule: &weekdays}
	}
	cases := map[string]func(*routines.Change){
		"missing title":    func(c *routines.Change) { c.Title = pointer("  ") },
		"long title":       func(c *routines.Change) { c.Title = pointer(strings.Repeat("字", routines.TitleLength+1)) },
		"missing schedule": func(c *routines.Change) { c.Schedule = nil },
		"zone name": func(c *routines.Change) {
			c.Schedule = &routines.Schedule{Type: routines.Cron, Expression: "0 9 * * *", TimeZone: "+08:00"}
		},
		"unknown zone": func(c *routines.Change) {
			c.Schedule = &routines.Schedule{Type: routines.Cron, Expression: "0 9 * * *", TimeZone: "Mars/Olympus"}
		},
		"six cron fields": func(c *routines.Change) {
			c.Schedule = &routines.Schedule{Type: routines.Cron, Expression: "0 0 9 * * *", TimeZone: "UTC"}
		},
		"invalid cron": func(c *routines.Change) {
			c.Schedule = &routines.Schedule{Type: routines.Cron, Expression: "61 9 * * *", TimeZone: "UTC"}
		},
		"once without zone": func(c *routines.Change) {
			c.Schedule = &routines.Schedule{Type: routines.Once, At: "2026-02-01T09:00:00", TimeZone: "UTC"}
		},
		"once in the past": func(c *routines.Change) {
			c.Schedule = &routines.Schedule{Type: routines.Once, At: "2026-01-04T09:00:00Z", TimeZone: "UTC"}
		},
		"execution time": func(c *routines.Change) { c.ExecutionMs = pointer(int64(999)) },
		"delivery":       func(c *routines.Change) { c.Delivery = "sometimes" },
		"no condition":   func(c *routines.Change) { c.Delivery = routines.Conditional },
	}
	for name, mutate := range cases {
		change := valid()
		mutate(&change)
		if _, err := service.Manage(ctx, 1, change, name, "user"); code(err) != routines.BadRequest {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	bound := valid()
	bound.SkillIDs = []string{"skill-1", "skill-1"}
	if _, err := service.Manage(ctx, 1, bound, "skill", "user"); code(err) != routines.NotFound {
		t.Fatalf("unbound skill = %v", err)
	}
	writeDocument(t, records, "persona.skills", `{"items":[{"id":"skill-1","enabledBots":[1]}]}`)
	result, err := service.Manage(ctx, 1, bound, "skill", "user")
	if err != nil || len(result.(routines.Routine).SkillIDs) != 1 {
		t.Fatalf("bound skill = %+v, %v", result, err)
	}
	if listing, _ := service.List(ctx, 1); len(listing.Items) != 1 {
		t.Fatalf("rejected changes were stored: %+v", listing.Items)
	}
}

func TestOnceSchedule(t *testing.T) {
	schedule := routines.Schedule{Type: routines.Once, At: "2026-01-05T09:30+08:00", TimeZone: "Asia/Shanghai"}
	next, err := schedule.Next(start)
	if err != nil || next == nil || !next.Equal(time.Date(2026, 1, 5, 1, 30, 0, 0, time.UTC)) {
		t.Fatalf("Next = %v, %v", next, err)
	}
	if next, err := schedule.Next(*next); err != nil || next != nil {
		t.Fatalf("Next after the run = %v, %v", next, err)
	}
}

func TestTools(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	registry := tools.NewRegistry()
	if err := routines.Register(registry, service); err != nil {
		t.Fatal(err)
	}
	approved := 0
	approve := func(context.Context, tools.Call, tools.Definition) (bool, error) {
		approved++
		return true, nil
	}
	call := func(name, callID, input string, approver tools.Approver) (tools.Result, error) {
		return registry.Call(ctx, tools.Call{BotID: 1, TaskID: 7, CallID: callID, Name: name, Input: json.RawMessage(input)}, approver)
	}
	input := `{"title":"Morning brief","instructions":"Summarize unread mail.","schedule":{"type":"cron","expression":"0 9 * * 1-5","timeZone":"Asia/Shanghai"}}`
	if _, err := call("routine_create", "c1", input, nil); !errors.Is(err, tools.ErrDenied) {
		t.Fatalf("unapproved create = %v", err)
	}
	result, err := call("routine_create", "c1", input, approve)
	if err != nil || result.IsError {
		t.Fatalf("create = %+v, %v", result, err)
	}
	var created routines.Routine
	if err := json.Unmarshal([]byte(result.Content[0].Text), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Enabled || created.Revision != 1 || created.Source != "bot:1/tool:c1" {
		t.Fatalf("created = %+v", created)
	}

	result, err = call("routine_list", "c2", `{}`, nil)
	if err != nil || result.IsError {
		t.Fatalf("list = %+v, %v", result, err)
	}
	var listing routines.Listing
	if err := json.Unmarshal([]byte(result.Content[0].Text), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Now != "2026-01-05T00:00:00.000Z" || listing.TimeZone == "" || len(listing.Items) != 1 || listing.Runs == nil {
		t.Fatalf("listing = %+v", listing)
	}

	if result, err := call("routine_manage", "c3", `{"action":"pause","id":"`+created.ID+`"}`, approve); err != nil || !result.IsError {
		t.Fatalf("manage without revision = %+v, %v", result, err)
	}
	result, err = call("routine_manage", "c4", `{"action":"pause","id":"`+created.ID+`","revision":1}`, approve)
	if err != nil || result.IsError || !strings.Contains(result.Content[0].Text, `"enabled":false`) {
		t.Fatalf("pause = %+v, %v", result, err)
	}
	if approved != 3 {
		t.Fatalf("approvals = %d", approved)
	}
}

func TestReport(t *testing.T) {
	ctx := context.Background()
	service, records := newService(t)
	registry := tools.NewRegistry()
	if err := routines.Register(registry, service); err != nil {
		t.Fatal(err)
	}
	report := func(taskID int64, input string) string {
		t.Helper()
		result, err := registry.Call(ctx, tools.Call{BotID: 1, TaskID: taskID, CallID: "c", Name: "routine_report", Input: json.RawMessage(input)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			return "error: " + result.Content[0].Text
		}
		return result.Content[0].Text
	}
	task := func(id store.TaskID, status store.TaskStatus, input string) store.Write {
		return store.Write{Type: store.WriteTask, Task: &store.Task{ID: id, ConversationID: 1, Input: json.RawMessage(input), State: store.TaskState{Status: status}}}
	}
	if _, _, err := records.Commit(ctx, []store.Batch{{
		{Type: store.WriteConversation, Conversation: &store.Conversation{ID: 1}},
		task(7, store.TaskRunning, `{"routineRunId":"run-1"}`),
		task(8, store.TaskRunning, `{"routineRunId":"run-2"}`),
		task(9, store.TaskRunning, `{}`),
		task(10, store.TaskTerminal, `{"routineRunId":"run-1"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	writeDocument(t, records, routines.DocKind, `{"items":[],"runs":[
		{"id":"run-1","botId":1,"status":"running","delivery":"conditional","files":[{"id":"file-1"}]},
		{"id":"run-2","botId":1,"status":"running","delivery":"always"}]}`)

	found := `{"notify":true,"text":"Invoice 42 arrived.","summary":"Invoices through 42.","key":"invoice-42"}`
	for name, attempt := range map[string]struct {
		taskID int64
		input  string
	}{
		"unknown task":     {99, found},
		"chat task":        {9, found},
		"finished task":    {10, found},
		"always delivery":  {8, found},
		"notify needs key": {7, `{"notify":true,"text":"Invoice 42 arrived.","summary":"Invoices through 42."}`},
		"missing summary":  {7, `{"notify":false,"text":""}`},
	} {
		if text := report(attempt.taskID, attempt.input); !strings.HasPrefix(text, "error: ") {
			t.Errorf("%s: %s", name, text)
		}
	}
	for range 2 {
		if text := report(7, found); text != `{"accepted":true}` {
			t.Fatalf("report = %s", text)
		}
	}
	if text := report(7, `{"notify":false,"text":"","summary":"Nothing new."}`); !strings.HasPrefix(text, "error: ") {
		t.Fatalf("second, different report = %s", text)
	}
	listing, err := service.List(ctx, 1)
	if err != nil || len(listing.Runs) != 2 {
		t.Fatalf("List = %+v, %v", listing, err)
	}
	var stored struct {
		Report routines.Report  `json:"report"`
		Files  []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(listing.Runs[0], &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Report.Key != "invoice-42" || !stored.Report.Notify || len(stored.Files) != 1 {
		t.Fatalf("stored run = %s", listing.Runs[0])
	}
}
