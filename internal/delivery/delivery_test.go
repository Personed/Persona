package delivery_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/computer"
	"github.com/Personed/Persona/internal/delivery"
	"github.com/Personed/Persona/internal/mcpserver"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/tools"
)

const (
	botID  = 1
	taskID = 9
)

// target is a work computer holding files by absolute path. A nil value is a
// directory. stats overrides what Stat reports for a path.
type target struct {
	computer.Target
	info  computer.Info
	files map[string][]byte
	stats map[string]computer.FileInfo
}

func (t *target) Info(context.Context) (computer.Info, error) { return t.info, nil }

func (t *target) Stat(_ context.Context, path string) (computer.FileInfo, error) {
	content, ok := t.files[path]
	if !ok {
		return computer.FileInfo{}, fs.ErrNotExist
	}
	if reported, ok := t.stats[path]; ok {
		return reported, nil
	}
	return computer.FileInfo{Path: path, Size: int64(len(content)), Dir: content == nil, Links: 1}, nil
}

func (t *target) ReadFile(_ context.Context, path string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(t.files[path])), nil
}

type fixture struct {
	service  *delivery.Service
	registry tools.Registry
	records  *store.SQLite
	target   *target
	root     string
}

func commit(t *testing.T, records store.Store, writes ...store.Write) {
	t.Helper()
	if _, _, err := records.Commit(t.Context(), []store.Batch{writes}); err != nil {
		t.Fatal(err)
	}
}

func document(id store.DocumentID, kind string, scope store.Scope, value string) store.Write {
	return store.Write{
		Type:     store.WriteDocumentCreate,
		Document: &store.Document{ID: id, Kind: kind, Scope: scope},
		Value:    json.RawMessage(value),
	}
}

func newFixture(t *testing.T, taskInput string) *fixture {
	t.Helper()
	dir := t.TempDir()
	records, err := store.Open(t.Context(), filepath.Join(dir, "persona-go.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := records.Close(); err != nil {
			t.Error(err)
		}
	})
	commit(t, records,
		store.Write{Type: store.WriteConversation, Conversation: &store.Conversation{ID: botID}},
		store.Write{Type: store.WriteTask, Task: &store.Task{
			ID: taskID, ConversationID: botID, Kind: "bot", Input: json.RawMessage(taskInput),
			State: store.TaskState{Status: "running"},
		}},
		document(20, "persona.engine", store.Scope{Kind: "conversation", ConversationID: botID}, `{"activeTurn":"turn-1"}`),
	)
	f := &fixture{
		registry: tools.NewRegistry(),
		records:  records,
		root:     filepath.Join(dir, "persona-go.sqlite.files"),
		target: &target{
			info: computer.Info{ID: "mac-1", OS: "linux"},
			files: map[string][]byte{
				"/work/bot":                nil,
				"/work/bot/out":            nil,
				"/work/bot/out/report.pdf": []byte("%PDF-1.7 report"),
				"/work/bot/notes.txt":      []byte("plain notes"),
				"/work/bot/big.bin":        bytes.Repeat([]byte{1}, delivery.MaxFileBytes+1),
				"/work/secret.txt":         []byte("other bot"),
			},
			stats: map[string]computer.FileInfo{},
		},
	}
	f.service = delivery.NewService(delivery.Options{
		Store: records,
		Root:  f.root,
		Workspace: func(context.Context, int64) (delivery.Workspace, error) {
			return delivery.Workspace{Target: f.target, Dir: "/work/bot"}, nil
		},
		Now: func() time.Time { return time.UnixMilli(1_700_000_000_000) },
	})
	if err := delivery.Register(f.registry, f.service); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) call(t *testing.T, callID, input string) tools.Result {
	t.Helper()
	result, err := f.registry.Call(t.Context(), tools.Call{
		BotID: botID, TaskID: taskID, CallID: callID, Name: "attach_file", Input: json.RawMessage(input),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type chatMessage struct {
	ID          string                `json:"id"`
	TurnID      *string               `json:"turnId"`
	Hidden      bool                  `json:"hidden"`
	Role        string                `json:"role"`
	Timestamp   int64                 `json:"timestamp"`
	ToolCallID  string                `json:"toolCallId"`
	ToolName    string                `json:"toolName"`
	ToolStatus  string                `json:"toolStatus"`
	Notice      *delivery.Notice      `json:"notice"`
	Attachments []delivery.Attachment `json:"attachments"`
}

func (f *fixture) messages(t *testing.T) []chatMessage {
	t.Helper()
	page, err := f.records.Entries(t.Context(), store.EntryQuery{ConversationID: botID}, store.Scan{})
	if err != nil {
		t.Fatal(err)
	}
	var messages []chatMessage
	for _, entry := range page.Items {
		if entry.Kind != delivery.MessageKind || entry.ByTaskID != nil {
			t.Fatalf("entry = %+v", entry)
		}
		var message chatMessage
		if err := json.Unmarshal(entry.Data, &message); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	return messages
}

func TestAttachWorkspaceFileOncePerCall(t *testing.T) {
	f := newFixture(t, `{}`)
	content := f.target.files["/work/bot/out/report.pdf"]
	digest := sha256.Sum256(append(append([]byte("out/report.pdf\x00"), content...), "mac-1"...))
	want := delivery.Attachment{
		ID: hex.EncodeToString(digest[:]), Name: "report.pdf", Size: int64(len(content)),
		Path: "out/report.pdf", MimeType: "application/pdf", ComputerID: "mac-1",
	}

	for range 2 {
		result := f.call(t, "session-1/1", `{"path":"out/report.pdf"}`)
		if result.IsError || result.Content[0].Text != "Attached report.pdf (15 bytes)" {
			t.Fatalf("result = %+v", result)
		}
	}
	messages := f.messages(t)
	if len(messages) != 1 {
		t.Fatalf("a redelivered call posted %d messages", len(messages))
	}
	message := messages[0]
	if message.TurnID == nil || *message.TurnID != "turn-1" || message.Hidden || message.Role != "toolResult" ||
		message.ToolCallID != "session-1/1" || message.ToolName != "attach_file" || message.ToolStatus != "done" ||
		message.Timestamp != 1_700_000_000_000 || !strings.HasPrefix(message.ID, "product:") ||
		len(message.Attachments) != 1 || message.Attachments[0].ID != want.ID {
		t.Fatalf("message = %+v", message)
	}

	// The snapshot uses the TypeScript layout and outlives the workspace file.
	directory := filepath.Join(f.root, "1", "artifacts", want.ID)
	saved, err := os.ReadFile(filepath.Join(directory, "content"))
	if err != nil || !bytes.Equal(saved, content) {
		t.Fatalf("content = %q, %v", saved, err)
	}
	metadata, err := os.ReadFile(filepath.Join(directory, "metadata.json"))
	wantMetadata, _ := json.Marshal(want)
	if err != nil || !bytes.Equal(metadata, wantMetadata) {
		t.Fatalf("metadata = %s, %v", metadata, err)
	}
	delete(f.target.files, "/work/bot/out/report.pdf")

	// A new call delivers again, by absolute path or by saved id.
	f.target.files["/work/bot/out/report.pdf"] = content
	f.call(t, "session-1/2", `{"path":"/work/bot/out/report.pdf"}`)
	delete(f.target.files, "/work/bot/out/report.pdf")
	if result := f.call(t, "session-1/3", `{"id":"`+want.ID+`"}`); result.IsError {
		t.Fatalf("result = %+v", result)
	}
	messages = f.messages(t)
	if len(messages) != 3 || messages[1].Attachments[0].ID != want.ID || messages[2].Attachments[0].ID != want.ID {
		t.Fatalf("messages = %+v", messages)
	}

	// Sniffed text is stored as opaque bytes.
	f.call(t, "session-1/4", `{"path":"notes.txt"}`)
	if got := f.messages(t)[3].Attachments[0]; got.MimeType != "application/octet-stream" || got.Name != "notes.txt" {
		t.Fatalf("attachment = %+v", got)
	}
}

func TestAttachRejectsWithoutDelivering(t *testing.T) {
	f := newFixture(t, `{}`)
	f.target.files["/work/bot/grown.bin"] = bytes.Repeat([]byte{1}, delivery.MaxFileBytes+1)
	f.target.stats["/work/bot/grown.bin"] = computer.FileInfo{Size: 4, Links: 1}
	for input, want := range map[string]string{
		``:                                         "Provide either",
		`{}`:                                       "Provide either",
		`{"path":"notes.txt","id":"abc"}`:          "Provide either",
		`{"path":7}`:                               "Invalid arguments",
		`{"path":"../secret.txt"}`:                 "outside this Bot's workspace",
		`{"path":"/work/secret.txt"}`:              "outside this Bot's workspace",
		`{"path":"/work/bot"}`:                     "outside this Bot's workspace",
		`{"path":"out/../../secret.txt"}`:          "outside this Bot's workspace",
		`{"path":"notes.txt:stream"}`:              "outside this Bot's workspace",
		`{"path":"notes.txt."}`:                    "outside this Bot's workspace",
		`{"path":"out"}`:                           "Only files can be attached",
		`{"path":"gone/notes.txt"}`:                "File not found: gone/notes.txt",
		`{"path":"."}`:                             "outside this Bot's workspace",
		`{"path":"missing.txt"}`:                   "File not found: missing.txt",
		`{"path":"big.bin"}`:                       "File exceeds 10 MiB",
		`{"path":"grown.bin"}`:                     "File exceeds 10 MiB",
		`{"id":"../../1/artifacts"}`:               "Invalid file ID",
		`{"id":"` + strings.Repeat("a", 64) + `"}`: "No saved file has this id.",
	} {
		result := f.call(t, "call "+input, input)
		if !result.IsError || !strings.Contains(result.Content[0].Text, want) {
			t.Errorf("%s: result = %+v, want %q", input, result, want)
		}
	}
	if messages := f.messages(t); len(messages) != 0 {
		t.Fatalf("rejected calls posted %+v", messages)
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Fatalf("rejected calls saved files: %v", err)
	}
}

// A link inside the workspace can name a file outside it: the work computer reads
// the other Bot's file through each of these paths.
func TestAttachRejectsLinkedPaths(t *testing.T) {
	const linked = "Linked paths are outside the workspace capability"
	f := newFixture(t, `{}`)
	secret := f.target.files["/work/secret.txt"]
	for path, info := range map[string]computer.FileInfo{
		"/work/bot/symlink.txt":     {Size: int64(len(secret)), Symlink: true, Links: 1},
		"/work/bot/hardlink.txt":    {Size: int64(len(secret)), Links: 2},
		"/work/bot/peer":            {Dir: true, Symlink: true, Links: 1},
		"/work/bot/peer/secret.txt": {Size: int64(len(secret)), Links: 1},
		"/work/bot/unknown.txt":     {Size: int64(len(secret))},
		"/work/bot/old":             {Dir: true},
		"/work/bot/old/secret.txt":  {Size: int64(len(secret)), Links: 1},
	} {
		if !info.Dir {
			f.target.files[path] = secret
		} else {
			f.target.files[path] = nil
		}
		f.target.stats[path] = info
	}
	for input, want := range map[string]string{
		`{"path":"symlink.txt"}`:               linked,
		`{"path":"hardlink.txt"}`:              linked,
		`{"path":"peer/secret.txt"}`:           linked,
		`{"path":"/work/bot/peer/secret.txt"}`: linked,
		`{"path":"unknown.txt"}`:               "did not report link information",
		`{"path":"old/secret.txt"}`:            "did not report link information",
	} {
		result := f.call(t, "call "+input, input)
		if !result.IsError || !strings.Contains(result.Content[0].Text, want) {
			t.Errorf("%s: result = %+v, want %q", input, result, want)
		}
	}

	// A workspace that is itself a link is refused for every file.
	f.target.stats["/work/bot"] = computer.FileInfo{Dir: true, Symlink: true, Links: 1}
	if result := f.call(t, "workspace", `{"path":"notes.txt"}`); !result.IsError || result.Content[0].Text != linked {
		t.Errorf("linked workspace: result = %+v", result)
	}
	if messages := f.messages(t); len(messages) != 0 {
		t.Fatalf("rejected calls posted %+v", messages)
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Fatalf("rejected calls saved files: %v", err)
	}
}

func TestAttachSavedFileOnlyOnItsComputer(t *testing.T) {
	f := newFixture(t, `{}`)
	f.call(t, "publish", `{"path":"notes.txt"}`)
	id := f.messages(t)[0].Attachments[0].ID

	f.target.info.ID = "mac-2"
	if result := f.call(t, "moved", `{"id":"`+id+`"}`); !result.IsError || !strings.Contains(result.Content[0].Text, "另一台电脑") {
		t.Fatalf("result = %+v", result)
	}

	// A file saved before computers were recorded belongs to the legacy computer.
	legacy := strings.Repeat("b", 64)
	directory := filepath.Join(f.root, "1", "artifacts", legacy)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"content":       "\x89PNG\r\n\x1a\n",
		"metadata.json": `{"id":"` + legacy + `","name":"shot.png","size":8,"path":"evidence/shot.png","source":{"tool":"screenshot"}}`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if result := f.call(t, "legacy-1", `{"id":"`+legacy+`"}`); !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	commit(t, f.records, document(30, "persona.computers", store.Scope{Kind: "session"}, `{"attachmentComputerId":"mac-2"}`))
	if result := f.call(t, "legacy-2", `{"id":"`+legacy+`"}`); result.IsError || result.Content[0].Text != "Attached shot.png (8 bytes)" {
		t.Fatalf("result = %+v", result)
	}
	messages := f.messages(t)
	if len(messages) != 2 {
		t.Fatalf("messages = %+v", messages)
	}
	got := messages[1].Attachments[0]
	if got.MimeType != "image/png" || got.ComputerID != "" || string(got.Source) != `{"tool":"screenshot"}` {
		t.Fatalf("attachment = %+v", got)
	}
}

func TestDeliverNoticeAndConditionalRoutine(t *testing.T) {
	f := newFixture(t, `{"routineRunId":"run-2"}`)
	commit(t, f.records, document(30, "persona.routines", store.Scope{Kind: "session"},
		`{"items":[],"runs":[{"id":"run-1","delivery":"always"},{"id":"run-2","delivery":"conditional"}]}`))
	call := tools.Call{BotID: botID, TaskID: taskID, CallID: "session-1/7", Name: "routine_create"}
	for range 2 {
		if err := f.service.Deliver(t.Context(), call, delivery.Message{Notice: &delivery.Notice{Text: "已创建定时"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.Deliver(t.Context(), call, delivery.Message{}); err != nil {
		t.Fatal(err)
	}
	messages := f.messages(t)
	if len(messages) != 1 || !messages[0].Hidden || messages[0].Notice == nil || messages[0].Notice.Text != "已创建定时" ||
		messages[0].ToolName != "routine_create" || messages[0].Attachments != nil {
		t.Fatalf("messages = %+v", messages)
	}
}

func TestGatewayServesAttachFile(t *testing.T) {
	f := newFixture(t, `{}`)
	ctx := t.Context()
	gateway, err := mcpserver.Open(ctx, mcpserver.Options{Registry: f.registry, BotID: botID, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gateway.Close(); err != nil {
			t.Error(err)
		}
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "pi-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gateway.URL, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "attach_file") {
		t.Fatalf("tools = %v", names)
	}
	// Delivery needs no approval: the gateway has no approver here.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "attach_file", Arguments: map[string]any{"path": "out/report.pdf"}})
	if err != nil || result.IsError {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if content, ok := result.Content[0].(*mcp.TextContent); !ok || content.Text != "Attached report.pdf (15 bytes)" {
		t.Fatalf("content = %#v", result.Content[0])
	}
	if messages := f.messages(t); len(messages) != 1 || messages[0].Attachments[0].Name != "report.pdf" {
		t.Fatalf("messages = %+v", messages)
	}
}
