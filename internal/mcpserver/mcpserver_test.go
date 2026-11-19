package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/mcpserver"
	"github.com/Personed/Persona/internal/memory"
	"github.com/Personed/Persona/internal/store/storetest"
	"github.com/Personed/Persona/internal/tools"
)

type mutatingTool struct{}

func (mutatingTool) Definition() tools.Definition {
	return tools.Definition{Name: "write_file", InputSchema: json.RawMessage(`{"type":"object"}`), Mutates: true}
}

func (mutatingTool) Run(context.Context, tools.Call) (tools.Result, error) {
	return tools.Result{Content: []tools.Content{{Type: "image", MimeType: "image/png", Data: "iVBORw=="}}, Handle: "result-1"}, nil
}

type countingTool struct{ runs atomic.Int64 }

func (*countingTool) Definition() tools.Definition {
	return tools.Definition{Name: "count", InputSchema: json.RawMessage(`{"type":"object"}`), Mutates: true}
}

func (c *countingTool) Run(context.Context, tools.Call) (tools.Result, error) {
	return tools.Result{Content: []tools.Content{{Type: "text", Text: fmt.Sprint("run ", c.runs.Add(1))}}}, nil
}

func open(t *testing.T, approve tools.Approver, extra ...tools.Tool) (*mcpserver.Gateway, *mcp.ClientSession) {
	t.Helper()
	ctx := context.Background()
	registry := tools.NewRegistry()
	if err := memory.Register(registry, memory.NewService(storetest.New(), nil)); err != nil {
		t.Fatal(err)
	}
	for _, tool := range append([]tools.Tool{mutatingTool{}}, extra...) {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	gateway, err := mcpserver.Open(ctx, mcpserver.Options{Registry: registry, BotID: 1, TaskID: 9, Approve: approve})
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
	return gateway, session
}

func text(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %#v", result.Content[0])
	}
	return content.Text
}

func TestGatewayServesMemoryTools(t *testing.T) {
	ctx := context.Background()
	var approved []string
	gateway, session := open(t, func(_ context.Context, call tools.Call, _ tools.Definition) (bool, error) {
		approved = append(approved, call.Name)
		return call.Name != "write_file", nil
	})

	waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := gateway.Ready(waiting); err == nil {
		t.Fatal("Ready before tools/list succeeded")
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "memory_delete,memory_list,memory_save,memory_search,write_file" {
		t.Fatalf("tools = %v", names)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "memory_list" && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint) {
			t.Fatalf("memory_list annotations = %+v", tool.Annotations)
		}
	}
	if err := gateway.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	saved, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "memory_save", Arguments: map[string]any{"revision": 0, "text": "Prefers Chinese replies", "category": "profile"}})
	if err != nil || saved.IsError {
		t.Fatalf("memory_save = %+v, %v", saved, err)
	}
	var entry memory.Entry
	if err := json.Unmarshal([]byte(text(t, saved)), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Revision != 1 || !strings.HasPrefix(entry.Source, "bot:1/tool:") {
		t.Fatalf("entry = %+v", entry)
	}
	found, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "memory_search", Arguments: map[string]any{"query": "chinese"}})
	if err != nil || !strings.Contains(text(t, found), entry.ID) {
		t.Fatalf("memory_search = %+v, %v", found, err)
	}
	stale, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "memory_delete", Arguments: map[string]any{"id": entry.ID, "revision": 5}})
	if err != nil || !stale.IsError {
		t.Fatalf("stale memory_delete = %+v, %v", stale, err)
	}
	denied, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "write_file", Arguments: map[string]any{}})
	if err != nil || !denied.IsError || !strings.Contains(text(t, denied), "denied") {
		t.Fatalf("unapproved write_file = %+v, %v", denied, err)
	}
	if strings.Join(approved, ",") != "memory_save,memory_delete,write_file" {
		t.Fatalf("approved = %v", approved)
	}
}

func TestGatewayReplaysRedeliveredCall(t *testing.T) {
	counter := &countingTool{}
	var approvals atomic.Int64
	gateway, _ := open(t, func(context.Context, tools.Call, tools.Definition) (bool, error) {
		approvals.Add(1)
		return true, nil
	}, counter)
	session := ""
	post := func(body string) string {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, gateway.URL, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("X-Persona-Request-Id", "spoofed")
		if session != "" {
			request.Header.Set("Mcp-Session-Id", session)
			request.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if session == "" {
			session = response.Header.Get("Mcp-Session-Id")
		}
		data, _ := io.ReadAll(response.Body)
		return string(data)
	}
	post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"pi","version":"0"}}}`)
	post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	call := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"count","arguments":{"n":1}}}`
	first, again := post(call), post(call)
	if !strings.Contains(first, "run 1") || first != again {
		t.Fatalf("first = %s, redelivered = %s", first, again)
	}
	if reused := post(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"count","arguments":{"n":2}}}`); !strings.Contains(reused, "reused") {
		t.Fatalf("reused ID = %s", reused)
	}
	if next := post(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"count","arguments":{"n":1}}}`); !strings.Contains(next, "run 2") {
		t.Fatalf("new ID = %s", next)
	}
	if counter.runs.Load() != 2 || approvals.Load() != 2 {
		t.Fatalf("runs = %d, approvals = %d", counter.runs.Load(), approvals.Load())
	}
}

func TestGatewayApprovalAndContent(t *testing.T) {
	ctx := context.Background()
	var approved tools.Call
	_, session := open(t, func(_ context.Context, call tools.Call, _ tools.Definition) (bool, error) {
		approved = call
		return true, nil
	})
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "write_file", Arguments: map[string]any{"path": "a"}})
	if err != nil || result.IsError {
		t.Fatalf("write_file = %+v, %v", result, err)
	}
	if approved.BotID != 1 || approved.TaskID != 9 || approved.CallID == "" || string(approved.Input) != `{"path":"a"}` {
		t.Fatalf("approved call = %+v", approved)
	}
	image, ok := result.Content[0].(*mcp.ImageContent)
	if !ok || image.MIMEType != "image/png" || string(image.Data) != "\x89PNG" {
		t.Fatalf("content = %#v", result.Content[0])
	}
	if result.Meta[mcpserver.ResultHandleMeta] != "result-1" {
		t.Fatalf("meta = %+v", result.Meta)
	}
}

func TestGatewayRejectsOtherPaths(t *testing.T) {
	gateway, _ := open(t, nil)
	base := gateway.URL[:strings.LastIndex(gateway.URL, "/")]
	response, err := http.Post(base+"/guess", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", response.StatusCode)
	}
}
