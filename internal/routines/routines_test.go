package routines_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/approval"
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
	service := routines.NewService(records, func() time.Time { return start })
	service.Confirm = func(context.Context, int64, approval.Request) (bool, error) {
		return true, nil
	}
	return service, records
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

// The details-panel switch is the user's own click: it commits directly
// instead of opening a confirmation card.
func TestEnableSkipsConfirmation(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	created := create(t, service, 1, "r1")
	service.Confirm = func(context.Context, int64, approval.Request) (bool, error) {
		return false, errors.New("confirmation must not run")
	}
	if err := service.Enable(ctx, 1, created.ID, false, created.Revision, "r2"); err != nil {
		t.Fatal(err)
	}
	item, err := service.List(ctx, 1)
	if err != nil || len(item.Items) != 1 || item.Items[0].Enabled {
		t.Fatalf("after disable = %+v, %v", item.Items, err)
	}
	if err := service.Enable(ctx, 1, created.ID, true, item.Items[0].Revision, "r3"); err != nil {
		t.Fatal(err)
	}
	item, err = service.List(ctx, 1)
	if err != nil || !item.Items[0].Enabled || item.Items[0].NextAt == nil {
		t.Fatalf("after enable = %+v, %v", item.Items, err)
	}
}

// A conditional run that finishes while the user reviews the card commits its
// baseline without bumping the revision; the approved change must keep it.
func TestApplyKeepsBaselineCommittedDuringConfirm(t *testing.T) {
	ctx := context.Background()
	service, records := newService(t)
	created := create(t, service, 1, "r1")
	service.Confirm = func(context.Context, int64, approval.Request) (bool, error) {
		listing, err := service.List(ctx, 1)
		if err != nil {
			return false, err
		}
		listing.Items[0].Baseline = "new baseline"
		listing.Items[0].NotifiedKey = "new key"
		payload, err := json.Marshal(map[string]any{"items": listing.Items, "runs": listing.Runs})
		if err != nil {
			return false, err
		}
		writeDocument(t, records, routines.DocKind, string(payload))
		return true, nil
	}
	change := routines.Change{Action: "update", ID: created.ID, Revision: pointer(created.Revision), Title: pointer("Renamed")}
	if _, err := service.Manage(ctx, 1, change, "r2", "user"); err != nil {
		t.Fatal(err)
	}
	listing, err := service.List(ctx, 1)
	if err != nil || len(listing.Items) != 1 {
		t.Fatalf("List = %+v, %v", listing, err)
	}
	got := listing.Items[0]
	if got.Baseline != "new baseline" || got.NotifiedKey != "new key" || got.Title != "Renamed" || got.Revision != 2 {
		t.Fatalf("committed routine = %+v", got)
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

// The expected runs are what croner 10 computes in "5-part" mode, as the
// TypeScript service did.
func TestCronSchedule(t *testing.T) {
	for _, c := range []struct {
		expression, zone, after, want string
	}{
		{"0 9 L * *", "UTC", "2026-02-15T00:00:00Z", "2026-02-28T09:00:00Z"},
		{"0 9 L * *", "UTC", "2028-02-15T00:00:00Z", "2028-02-29T09:00:00Z"},
		{"0 9 LW * *", "UTC", "2026-05-15T00:00:00Z", "2026-05-29T09:00:00Z"},  // May 31 is a Sunday.
		{"0 9 15W * *", "UTC", "2026-08-01T00:00:00Z", "2026-08-14T09:00:00Z"}, // August 15 is a Saturday.
		{"0 9 1W * *", "UTC", "2026-07-15T00:00:00Z", "2026-08-03T09:00:00Z"},  // W stays in the month.
		{"0 9 31W * *", "UTC", "2026-05-01T00:00:00Z", "2026-05-29T09:00:00Z"},
		{"0 9 * * FRI#2", "UTC", "2026-01-01T00:00:00Z", "2026-01-09T09:00:00Z"},
		{"0 9 * * 5#L", "UTC", "2026-01-01T00:00:00Z", "2026-01-30T09:00:00Z"},
		{"0 9 * * 5L", "UTC", "2026-01-01T00:00:00Z", "2026-01-30T09:00:00Z"},
		{"0 9 * * sun", "UTC", "2026-01-05T00:00:00Z", "2026-01-11T09:00:00Z"},
		{"0 9 * * 7", "UTC", "2026-01-05T00:00:00Z", "2026-01-11T09:00:00Z"},
		{"0 9 * * 0", "UTC", "2026-01-05T00:00:00Z", "2026-01-11T09:00:00Z"},
		{"0 9 15 ? *", "UTC", "2026-01-05T00:00:00Z", "2026-01-15T09:00:00Z"},
		{"0 9 13 * FRI", "UTC", "2026-01-05T00:00:00Z", "2026-01-09T09:00:00Z"},  // Day of month OR weekday.
		{"0 9 13 * +FRI", "UTC", "2026-01-05T00:00:00Z", "2026-02-13T09:00:00Z"}, // + requires both.
		{"0 9 L * MON#1", "UTC", "2026-01-05T00:00:00Z", "2026-01-05T09:00:00Z"},
		{"30 8 * JAN-MAR MON-FRI", "UTC", "2026-03-31T12:00:00Z", "2027-01-01T08:30:00Z"},
		{"*/20 9-10 1,15 * *", "Asia/Shanghai", "2026-01-15T02:50:00Z", "2026-02-01T01:00:00Z"},
		// croner rolls February 30 over to March 2 and resumes there, skipping March 1.
		{"0 0 1,30 * *", "UTC", "2026-02-15T00:00:00Z", "2026-03-30T00:00:00Z"},
		{"30 2 * * *", "America/New_York", "2026-03-07T12:00:00Z", "2026-03-08T07:30:00Z"}, // Skipped hour.
		{"30 1 * * *", "America/New_York", "2026-10-31T12:00:00Z", "2026-11-01T05:30:00Z"}, // Repeated hour.
		{"30 1 * * *", "America/New_York", "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"},
	} {
		after, _ := time.Parse(time.RFC3339, c.after)
		want, _ := time.Parse(time.RFC3339, c.want)
		schedule := routines.Schedule{Type: routines.Cron, Expression: c.expression, TimeZone: c.zone}
		if next, err := schedule.Next(after); err != nil || next == nil || !next.Equal(want) {
			t.Errorf("%q after %s = %v, %v; want %s", c.expression, c.after, next, err, c.want)
		}
	}
	for _, expression := range []string{"0 9 * * 8", "0 9 15W-20 * *", "0 9 * * MON#6", "0 9 L/2 * *", "0 9 * * *  *"} {
		schedule := routines.Schedule{Type: routines.Cron, Expression: expression, TimeZone: "UTC"}
		if next, err := schedule.Next(start); err == nil {
			t.Errorf("%q = %v; want an error", expression, next)
		}
	}
}

func TestTools(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	registry := tools.NewRegistry()
	if err := routines.Register(registry, service); err != nil {
		t.Fatal(err)
	}
	call := func(name, callID, input string) (tools.Result, error) {
		return registry.Call(ctx, tools.Call{BotID: 1, TaskID: 7, CallID: callID, Name: name, Input: json.RawMessage(input)}, nil)
	}
	input := `{"title":"Morning brief","instructions":"Summarize unread mail.","schedule":{"type":"cron","expression":"0 9 * * 1-5","timeZone":"Asia/Shanghai"}}`
	service.Confirm = nil
	if result, err := call("routine_create", "c1", input); err != nil || !result.IsError {
		t.Fatalf("unconfirmed create = %+v, %v", result, err)
	}
	confirmed := 0
	service.Confirm = func(context.Context, int64, approval.Request) (bool, error) {
		confirmed++
		return true, nil
	}
	result, err := call("routine_create", "c1", input)
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

	result, err = call("routine_list", "c2", `{}`)
	if err != nil || result.IsError {
		t.Fatalf("list = %+v, %v", result, err)
	}
	var listing routines.Listing
	if err := json.Unmarshal([]byte(result.Content[0].Text), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Now != "2026-01-05T00:00:00.000Z" || len(listing.Items) != 1 || listing.Runs == nil {
		t.Fatalf("listing = %+v", listing)
	}
	listed := routines.Schedule{Type: routines.Cron, Expression: "0 9 * * *", TimeZone: listing.TimeZone}
	if err := listed.Validate(start); err != nil {
		t.Fatalf("listed time zone %q is not schedulable: %v", listing.TimeZone, err)
	}

	if result, err := call("routine_manage", "c3", `{"action":"pause","id":"`+created.ID+`"}`); err != nil || !result.IsError {
		t.Fatalf("manage without revision = %+v, %v", result, err)
	}
	result, err = call("routine_manage", "c4", `{"action":"pause","id":"`+created.ID+`","revision":1}`)
	if err != nil || result.IsError || !strings.Contains(result.Content[0].Text, `"enabled":false`) {
		t.Fatalf("pause = %+v, %v", result, err)
	}
	if confirmed != 2 {
		t.Fatalf("confirmations = %d", confirmed)
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
