package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Personed/Persona/internal/store"
)

// fakeStore serves Entries and Entry from memory; other methods are unused.
type fakeStore struct {
	store.Store
	entries []store.Entry
}

func (f *fakeStore) Entries(_ context.Context, q store.EntryQuery, scan store.Scan) (store.Page[store.Entry], error) {
	var items []store.Entry
	for _, e := range f.entries {
		if e.ConversationID == q.ConversationID && (q.Min == 0 || e.ID >= q.Min) && (q.Max == 0 || e.ID <= q.Max) {
			items = append(items, e)
		}
	}
	sort.Slice(items, func(i, j int) bool { return (items[i].ID < items[j].ID) != scan.Descending })
	if scan.Limit > 0 && len(items) > scan.Limit {
		items = items[:scan.Limit]
	}
	return store.Page[store.Entry]{Items: items}, nil
}

func (f *fakeStore) Entry(_ context.Context, id store.EntryID) (store.Entry, store.Seq, error) {
	for _, e := range f.entries {
		if e.ID == id {
			return e, 1, nil
		}
	}
	return store.Entry{}, 0, store.ErrNotFound
}

func (f *fakeStore) chat(id, role, text string, extra ...string) {
	content, _ := json.Marshal(text)
	data := fmt.Sprintf(`{"id":%q,"role":%q,"content":%s%s}`, id, role, content, strings.Join(extra, ""))
	f.entries = append(f.entries, store.Entry{
		ID: store.EntryID(len(f.entries) + 1), ConversationID: 1, Kind: EngineMessage, Data: json.RawMessage(data),
	})
}

func (f *fakeStore) model(messages string) {
	f.entries = append(f.entries, store.Entry{
		ID: store.EntryID(len(f.entries) + 1), ConversationID: 1, Kind: "persona.turn", Model: json.RawMessage(messages),
	})
}

type packet struct {
	After    *int64           `json:"after"`
	From     int64            `json:"from"`
	Through  int64            `json:"through"`
	Excerpt  bool             `json:"excerpt"`
	Messages []historyMessage `json:"messages"`
}

func history(t *testing.T, st store.Store, o HistoryOptions) (string, packet) {
	t.Helper()
	text, err := History(context.Background(), st, 1, o)
	if err != nil {
		t.Fatal(err)
	}
	var p packet
	if text != "" {
		if err := json.Unmarshal([]byte(text), &p); err != nil {
			t.Fatal(err)
		}
	}
	return text, p
}

func TestHistoryBeforeTurnAfterCursor(t *testing.T) {
	st := &fakeStore{}
	st.chat("u1", "user", "first")
	st.chat("a1", "assistant", "reply", `,"stopped":true`)
	st.model(`[{"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"toolCall","id":"c","name":"bash","arguments":{"cmd":"<ls>"}}]},
		{"role":"toolResult","toolName":"bash","toolCallId":"c","isError":true,"content":[{"type":"text","text":"denied"}]}]`)
	st.chat("u2", "user", "now")

	_, p := history(t, st, HistoryOptions{TurnID: "u2"})
	if p.After != nil || p.From != 1 || p.Through != 3 || p.Excerpt || len(p.Messages) != 4 {
		t.Fatalf("packet %+v", p)
	}
	if m := p.Messages[1]; m.Status != "stopped" || !strings.Contains(m.Text, "Historical response was stopped") {
		t.Fatalf("stopped message %+v", m)
	}
	if m := p.Messages[2]; !strings.Contains(m.Text, `Historical tool calls: [{"type":"toolCall","id":"c","name":"bash","arguments":{"cmd":"<ls>"}}]`) {
		t.Fatalf("tool calls %q", m.Text)
	}
	if m := p.Messages[3]; m.Role != "toolResult" || m.Status != "failed" || m.Tool != "bash" || m.ToolCallID != "c" {
		t.Fatalf("tool result %+v", m)
	}

	_, p = history(t, st, HistoryOptions{TurnID: "u2", Through: 2})
	if p.After == nil || *p.After != 2 || p.From != 3 || len(p.Messages) != 2 {
		t.Fatalf("after cursor %+v", p)
	}
	if text, _ := history(t, st, HistoryOptions{TurnID: "u2", Through: 3}); text != "" {
		t.Fatalf("nothing new should be empty, got %s", text)
	}

	_, p = history(t, st, HistoryOptions{TurnID: "u2", Tools: ToolsSummary})
	if m := p.Messages[3]; m.Text != "bash: failed" || strings.Contains(p.Messages[2].Text, "Historical tool calls") {
		t.Fatalf("summary %+v", p.Messages)
	}
	_, p = history(t, st, HistoryOptions{TurnID: "u2", Tools: ToolsOmit})
	if len(p.Messages) != 3 {
		t.Fatalf("omit %+v", p.Messages)
	}

	if _, err := History(context.Background(), st, 1, HistoryOptions{TurnID: "missing"}); err == nil {
		t.Fatal("unknown turn accepted")
	}
}

func TestHistoryBudgets(t *testing.T) {
	st := &fakeStore{}
	for i := range 100 {
		st.chat(fmt.Sprint("m", i), "user", strings.Repeat("界", 2000)) // 6000 bytes each
	}
	text, p := history(t, st, HistoryOptions{})
	if len(text) > HistoryBytes || !p.Excerpt || len(p.Messages) == 0 {
		t.Fatalf("packet %d bytes, %d messages, excerpt %v", len(text), len(p.Messages), p.Excerpt)
	}
	if last := p.Messages[len(p.Messages)-1]; last.EntryID != 100 {
		t.Fatalf("newest message first kept, got %d", last.EntryID)
	}
	for _, m := range p.Messages {
		if len(m.Text) > ExcerptBytes || !utf8.ValidString(m.Text) || !m.Excerpt {
			t.Fatalf("excerpt %d bytes, valid %v", len(m.Text), utf8.ValidString(m.Text))
		}
	}

	small := &fakeStore{}
	for i := range 80 {
		small.chat(fmt.Sprint("m", i), "user", "x")
	}
	_, p = history(t, small, HistoryOptions{})
	if len(p.Messages) != HistoryEntries || p.From != 17 || !p.Excerpt {
		t.Fatalf("entry cap: %d messages from %d", len(p.Messages), p.From)
	}
	_, p = history(t, small, HistoryOptions{Before: 17})
	if len(p.Messages) != 16 || p.Through != 16 {
		t.Fatalf("older page: %d messages through %d", len(p.Messages), p.Through)
	}

	if _, err := History(context.Background(), st, 1, HistoryOptions{Budget: 40}); !errors.Is(err, ErrHistoryBudget) {
		t.Fatalf("tiny budget: %v", err)
	}
}

func TestHistoryEntryPages(t *testing.T) {
	st := &fakeStore{}
	st.chat("u", "user", strings.Repeat("é", 3000))
	var got strings.Builder
	offset := 0
	for {
		page, err := HistoryEntry(context.Background(), st, 1, 1, offset)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			NextOffset *int   `json:"nextOffset"`
			TotalBytes int    `json:"totalBytes"`
			Text       string `json:"text"`
		}
		if err := json.Unmarshal([]byte(page), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Text) > ExcerptBytes || !utf8.ValidString(p.Text) {
			t.Fatalf("page of %d bytes", len(p.Text))
		}
		got.WriteString(p.Text)
		if p.NextOffset == nil {
			if got.Len() != p.TotalBytes {
				t.Fatalf("read %d of %d bytes", got.Len(), p.TotalBytes)
			}
			break
		}
		offset = *p.NextOffset
	}
	if _, err := HistoryEntry(context.Background(), st, 1, 1, strings.Index(got.String(), "é")+1); err == nil {
		t.Fatal("offset inside a rune accepted")
	}
	if _, err := HistoryEntry(context.Background(), st, 2, 1, 0); err == nil {
		t.Fatal("entry of another conversation returned")
	}
}
