package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/memory"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/store/storetest"
	"github.com/Personed/Persona/internal/tools"
)

func newService(t *testing.T) (*memory.Service, *storetest.Store) {
	t.Helper()
	records := storetest.New()
	clock := time.UnixMilli(1_700_000_000_000)
	return memory.NewService(records, func() time.Time {
		clock = clock.Add(time.Millisecond)
		return clock
	}), records
}

func code(err error) memory.ErrorCode {
	var memoryError *memory.Error
	if errors.As(err, &memoryError) {
		return memoryError.Code
	}
	return ""
}

func TestSaveListSearchDelete(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	saved, err := service.Save(ctx, 1, memory.Input{Text: "  Prefers Chinese replies ", Category: memory.Profile, RequestID: "r1"}, memory.UserSource)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" || saved.Revision != 1 || saved.Text != "Prefers Chinese replies" || !saved.UserAuthored || saved.BotID == nil || *saved.BotID != 1 {
		t.Fatalf("saved = %+v", saved)
	}
	again, err := service.Save(ctx, 1, memory.Input{Text: "other", RequestID: "r1"}, memory.UserSource)
	if err != nil || again.ID != saved.ID || again.Text != saved.Text {
		t.Fatalf("replayed save = %+v, %v", again, err)
	}
	if _, err := service.Save(ctx, 1, memory.Input{Text: "Deploys with make ship", RequestID: "r2"}, "bot:1/tool:c"); err != nil {
		t.Fatal(err)
	}
	items, err := service.List(ctx, 1)
	if err != nil || len(items) != 2 || items[1].Category != memory.Work {
		t.Fatalf("List = %+v, %v", items, err)
	}
	if other, _ := service.List(ctx, 2); len(other) != 0 {
		t.Fatalf("other Bot sees %+v", other)
	}
	found, err := service.Search(ctx, 1, "CHINESE prefers")
	if err != nil || len(found) != 1 || found[0].ID != saved.ID {
		t.Fatalf("Search = %+v, %v", found, err)
	}
	if found, _ := service.Search(ctx, 1, "chinese ship"); len(found) != 0 {
		t.Fatalf("Search matched a partial query: %+v", found)
	}
	if err := service.Delete(ctx, 1, saved.ID, 2, memory.UserSource); code(err) != memory.Conflict {
		t.Fatalf("stale delete = %v", err)
	}
	if err := service.Delete(ctx, 2, saved.ID, 1, memory.UserSource); err != nil {
		t.Fatalf("other Bot delete = %v", err)
	}
	if err := service.Delete(ctx, 1, saved.ID, 1, memory.UserSource); err != nil {
		t.Fatal(err)
	}
	if items, _ := service.List(ctx, 1); len(items) != 1 {
		t.Fatalf("after delete = %+v", items)
	}
}

func TestSaveValidation(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	saved, err := service.Save(ctx, 1, memory.Input{Text: "note", RequestID: "r1"}, memory.UserSource)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		botID int64
		input memory.Input
		want  memory.ErrorCode
	}{
		{"stale revision", 1, memory.Input{ID: saved.ID, Text: "x", Revision: 0, RequestID: "a"}, memory.Conflict},
		{"other Bot", 2, memory.Input{ID: saved.ID, Text: "x", Revision: 1, RequestID: "b"}, memory.NotFound},
		{"blank text", 1, memory.Input{Text: "   ", RequestID: "c"}, memory.BadRequest},
		{"long text", 1, memory.Input{Text: strings.Repeat("字", memory.TextLength+1), RequestID: "d"}, memory.BadRequest},
	}
	for _, test := range cases {
		if _, err := service.Save(ctx, test.botID, test.input, memory.UserSource); code(err) != test.want {
			t.Errorf("%s: err = %v, want %s", test.name, err, test.want)
		}
	}
}

func TestProfileCapacity(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	text := strings.Repeat("a", 3000)
	for index := range 2 {
		if _, err := service.Save(ctx, 1, memory.Input{Text: text, Category: memory.Profile, RequestID: string(rune('a' + index))}, memory.UserSource); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.Save(ctx, 1, memory.Input{Text: text, Category: memory.Profile, RequestID: "c"}, memory.UserSource); code(err) != memory.BadRequest {
		t.Fatalf("over capacity = %v", err)
	}
	if _, err := service.Save(ctx, 1, memory.Input{Text: text, Category: memory.Work, RequestID: "d"}, memory.UserSource); err != nil {
		t.Fatalf("work note = %v", err)
	}
}

func TestSharedRecordsAndReviewGuard(t *testing.T) {
	ctx := context.Background()
	service, records := newService(t)
	shared := true
	entry, err := service.Save(ctx, 1, memory.Input{Text: "Lives in Recife", Shared: &shared, RequestID: "r1"}, memory.UserSource)
	if err != nil || entry.BotID != nil {
		t.Fatalf("shared save = %+v, %v", entry, err)
	}
	if items, _ := service.List(ctx, 2); len(items) != 1 {
		t.Fatalf("shared record hidden from Bot 2: %+v", items)
	}
	review := store.Document{ID: 999, Kind: memory.ReviewDocKind, Scope: store.Scope{Kind: "conversation", ConversationID: 2}}
	if _, _, err := records.Commit(ctx, []store.Batch{{{Type: store.WriteDocumentCreate, Document: &review, Value: json.RawMessage(`{"enabled":true,"turns":0,"through":null,"reviewing":true}`)}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(ctx, 2, memory.Input{ID: entry.ID, Text: "Moved", Revision: 1, RequestID: "r2"}, "bot:2/tool:x"); code(err) != memory.Conflict {
		t.Fatalf("review edit of shared record = %v", err)
	}
	if err := service.Delete(ctx, 2, entry.ID, 1, "bot:2/tool:y"); code(err) != memory.Conflict {
		t.Fatalf("review delete of shared record = %v", err)
	}
	updated, err := service.Save(ctx, 1, memory.Input{ID: entry.ID, Text: "Lives in Olinda", Revision: 1, RequestID: "r3"}, "bot:1/tool:z")
	if err != nil || updated.BotID != nil || !updated.UserAuthored {
		t.Fatalf("edit keeps sharing and authorship: %+v, %v", updated, err)
	}
}

func TestSnapshot(t *testing.T) {
	one, two := int64(1), int64(2)
	items := []memory.Entry{
		{ID: "old", BotID: &one, Text: "old fact", Category: memory.Profile, Revision: 1, UpdatedAt: 1},
		{ID: "new", BotID: &one, Text: "new <fact>", Category: memory.Profile, Revision: 1, UpdatedAt: 2},
		{ID: "work", BotID: &one, Text: "work note", Category: memory.Work, Revision: 1, UpdatedAt: 3},
		{ID: "other", BotID: &two, Text: "other Bot", Category: memory.Profile, Revision: 1, UpdatedAt: 4},
		{ID: "big", BotID: nil, Text: strings.Repeat("x", memory.ProfileBytes), Category: memory.Profile, Revision: 1, UpdatedAt: 5},
	}
	snapshot := memory.Snapshot(items, 1)
	lines := strings.Split(strings.TrimSuffix(snapshot, "\n"), "\n")
	want := []string{
		`{"id":"new","scope":"bot","revision":1,"text":"new <fact>"}`,
		`{"id":"old","scope":"bot","revision":1,"text":"old fact"}`,
	}
	if len(lines) != 3 || lines[1] != want[0] || lines[2] != want[1] {
		t.Fatalf("Snapshot = %q", snapshot)
	}
}

func approveAll(context.Context, tools.Call, tools.Definition) (bool, error) { return true, nil }

func TestToolsThroughRegistry(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)
	registry := tools.NewRegistry()
	if err := memory.Register(registry, service); err != nil {
		t.Fatal(err)
	}
	definitions, _ := registry.List(ctx, 1)
	var names []string
	for _, definition := range definitions {
		names = append(names, definition.Name)
		if !json.Valid(definition.InputSchema) {
			t.Errorf("%s schema is invalid JSON", definition.Name)
		}
	}
	if strings.Join(names, ",") != "memory_list,memory_save,memory_search,memory_delete" {
		t.Fatalf("tools = %v", names)
	}
	call := func(name, input string) tools.Result {
		t.Helper()
		result, err := registry.Call(ctx, tools.Call{BotID: 1, TaskID: 7, CallID: name + input, Name: name, Input: json.RawMessage(input)}, approveAll)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result
	}
	for _, name := range []string{"memory_save", "memory_delete"} {
		if _, err := registry.Call(ctx, tools.Call{BotID: 1, CallID: "x", Name: name, Input: json.RawMessage(`{}`)}, nil); !errors.Is(err, tools.ErrDenied) {
			t.Fatalf("%s without approval = %v", name, err)
		}
	}
	var saved memory.Entry
	if err := json.Unmarshal([]byte(call("memory_save", `{"revision":0,"text":"Uses pnpm","category":"work"}`).Content[0].Text), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Source != "bot:1/tool:memory_save"+`{"revision":0,"text":"Uses pnpm","category":"work"}` || saved.UserAuthored {
		t.Fatalf("saved = %+v", saved)
	}
	if result := call("memory_save", `{"text":"x"}`); !result.IsError {
		t.Fatalf("missing revision accepted: %+v", result)
	}
	if result := call("memory_search", `{"query":"pnpm"}`); !strings.Contains(result.Content[0].Text, saved.ID) {
		t.Fatalf("search = %+v", result)
	}
	if result := call("memory_delete", `{"id":"`+saved.ID+`","revision":1}`); result.IsError || result.Content[0].Text != `{"deleted":"`+saved.ID+`"}` {
		t.Fatalf("delete = %+v", result)
	}
	if result := call("memory_list", `{}`); result.Content[0].Text != "[]" {
		t.Fatalf("list = %+v", result)
	}
}
