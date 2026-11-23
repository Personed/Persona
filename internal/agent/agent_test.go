package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Personed/Persona/internal/store"
)

func open(t *testing.T) *Agent {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	records, err := store.Open(ctx, filepath.Join(dir, "persona-go.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = records.Close() })
	a, err := Open(ctx, records, Options{FilesRoot: filepath.Join(dir, "files"),
		Agent: Change{Model: &ModelRef{Provider: "faux", ModelID: "faux-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

const requestID = "00000000-0000-4000-8000-000000000000"

// TestChatViewFixture projects the turn that bots-conversation.json records
// after send and compares it with the recorded get answer.
func TestChatViewFixture(t *testing.T) {
	ctx := context.Background()
	a := open(t)
	task := store.TaskID(12)
	entries := []store.Entry{
		{ID: 10, ConversationID: a.Root, Kind: FileInput,
			Data:  json.RawMessage(`{"text":"hello","attachments":[],"requestId":"` + requestID + `"}`),
			Model: json.RawMessage(`[{"role":"user","content":"hello","timestamp":1800000000000}]`)},
		{ID: 14, ConversationID: a.Root, Kind: "pi.assistant", ByTaskID: &task,
			Model: json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"Hello from Persona."}],"stopReason":"stop","timestamp":1800000000001}]`)},
	}
	for _, entry := range entries {
		if _, _, err := a.Harness.Store().Commit(ctx, []store.Batch{{{Type: store.WriteEntry, Entry: &entry}}}); err != nil {
			t.Fatal(err)
		}
	}
	chat, err := a.Harness.ReadChat(ctx, int64(a.Root), "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(map[string]any{"json": chat})
	got = bytes.NewBufferString(strings.NewReplacer("1800000000000", `"<time>"`, "1800000000001", `"<time>"`, requestID, "<uuid>").Replace(string(got))).Bytes()
	var fixture []struct {
		Response struct {
			Body json.RawMessage `json:"body"`
		} `json:"response"`
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "api", "fixtures", "bots-conversation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var want, have any
	_ = json.Unmarshal(fixture[3].Response.Body, &want)
	_ = json.Unmarshal(got, &have)
	if !reflect.DeepEqual(want, have) {
		t.Errorf("get = %s\nwant %s", got, fixture[3].Response.Body)
	}

	bots, err := a.Bots.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bots) != 1 || bots[0].Preview != "Hello from Persona." || bots[0].LastMessageAt == nil || *bots[0].LastMessageAt != 1800000000001 {
		t.Errorf("list = %+v", bots)
	}
}

func TestReadChatPages(t *testing.T) {
	ctx := context.Background()
	a := open(t)
	for id := store.EntryID(10); id < 70; id++ {
		entry := store.Entry{ID: id, ConversationID: a.Root, Kind: "pi.user", Model: json.RawMessage(`[{"role":"user","content":"x"}]`)}
		if _, _, err := a.Harness.Store().Commit(ctx, []store.Batch{{{Type: store.WriteEntry, Entry: &entry}}}); err != nil {
			t.Fatal(err)
		}
	}
	chat, err := a.Harness.ReadChat(ctx, int64(a.Root), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != windowSize || chat.Before == nil || *chat.Before != "19:9007199254740991" {
		t.Fatalf("window: %d messages, before %v", len(chat.Messages), chat.Before)
	}
	page, err := a.Harness.ReadChat(ctx, int64(a.Root), *chat.Before)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 10 || page.Before != nil || messageCursor(page.Messages[9]) != "19:0" {
		t.Errorf("page: %d messages, before %v", len(page.Messages), page.Before)
	}
}

func TestBots(t *testing.T) {
	ctx := context.Background()
	a := open(t)
	bot, err := a.Bots.Create(ctx, CreateBot{Name: "Contract", Instructions: "Answer briefly.", RequestID: requestID})
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.Bots.Create(ctx, CreateBot{Name: "Other", RequestID: requestID})
	if err != nil || again.ID != bot.ID || again.Name != "Contract" {
		t.Errorf("repeated create = %+v, %v", again, err)
	}
	profile, err := a.Bots.Update(ctx, bot.ID, UpdateBot{Name: "  Renamed ", Instructions: "New.", Avatar: bot.Avatar, RequestID: "a"})
	if err != nil || profile.Name != "Renamed" || profile.Revision != 1 || profile.Instructions != "New." {
		t.Errorf("update = %+v, %v", profile, err)
	}
	var work *WorkError
	if _, err := a.Bots.Update(ctx, bot.ID, UpdateBot{Name: "x", RequestID: "b"}); !errors.As(err, &work) || work.Code != "CONFLICT" {
		t.Errorf("stale update: %v", err)
	}
	if err := a.Bots.CompleteWelcome(ctx, 999); !errors.Is(err, ErrNoBot) {
		t.Errorf("complete unknown: %v", err)
	}
	list, err := a.Bots.List(ctx)
	if err != nil || len(list) != 2 || list[1].Preview != "New." {
		t.Errorf("list = %+v, %v", list, err)
	}
	hidden, err := a.Bots.SetModelVisibility(ctx, Visibility{Harness: "pi", Model: "m", Visible: false})
	if err != nil || len(hidden) != 1 {
		t.Errorf("hide = %v, %v", hidden, err)
	}
	if hidden, err = a.Bots.SetModelVisibility(ctx, Visibility{Harness: "pi", Model: "m", Visible: true}); err != nil || len(hidden) != 0 {
		t.Errorf("show = %v, %v", hidden, err)
	}
}

func TestFiles(t *testing.T) {
	f := NewFiles(t.TempDir())
	png := []byte("\x89PNG\r\n\x1a\nrest")
	file, err := f.Upload(1, `C:\photos\shot.png`, png, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if file.Name != "shot.png" || file.MimeType != "image/png" || file.UploadID != requestID || !strings.HasPrefix(file.Path, "uploads/"+requestID+"/") {
		t.Errorf("upload = %+v", file)
	}
	workspace, _ := f.Workspace(1)
	if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(file.Path))); err != nil {
		t.Error(err)
	}
	if _, data, err := f.Download(1, file.ID); err != nil || !bytes.Equal(data, png) {
		t.Errorf("download: %v", err)
	}
	var work *WorkError
	if _, err := f.Upload(1, "bad:name", png, requestID); !errors.As(err, &work) || work.Message != "bad:name：文件名无效。" {
		t.Errorf("invalid name: %v", err)
	}
	if _, err := f.Upload(1, "a.bmp", []byte("BM0123456789abcdef"), requestID); !errors.As(err, &work) || !strings.Contains(work.Message, "image/bmp") {
		t.Errorf("unsupported image: %v", err)
	}
	if err := f.Discard(1, file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Metadata(1, file.ID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("discarded file remains: %v", err)
	}
}

func TestPreviewText(t *testing.T) {
	for in, want := range map[string]string{
		"# Title\n\n**bold** and `code` [link](http://x)": "Title bold and code link",
		"- one\n- two":     "one two",
		"snake_case stays": "snake_case stays",
	} {
		if got := previewText(in); got != want {
			t.Errorf("previewText(%q) = %q, want %q", in, got, want)
		}
	}
}
