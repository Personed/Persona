package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/computer"
)

// testComputerDriver stands in for cua-driver: one observation and two actions.
func testComputerDriver() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "driver", Version: "1"}, nil)
	object := map[string]any{"type": "object"}
	var clicks atomic.Int64
	server.AddTool(&mcp.Tool{Name: "get_desktop_state", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var data bytes.Buffer
		_ = png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 1)))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("clicks %d", clicks.Load())}, &mcp.ImageContent{MIMEType: "image/png", Data: data.Bytes()}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "click", InputSchema: object}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("click %d %s", clicks.Add(1), r.Params.Arguments)}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "hang", InputSchema: object}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		clicks.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	return server
}

func text(t *testing.T, observation computer.Observation) string {
	t.Helper()
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(observation.Raw, &result); err != nil || len(result.Content) == 0 {
		t.Fatalf("native result %s: %v", observation.Raw, err)
	}
	return result.Content[0].Text
}

func TestComputerJournal(t *testing.T) {
	targets := map[string]Transport{"stdio": Local(executable(t))}
	if _, err := exec.LookPath("ssh"); err == nil {
		targets["ssh"] = SSH(sshServer(t))
	}
	for name, transport := range targets {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dial := func() *Client {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				client, err := Dial(ctx, transport, Options{WorkDir: dir, ComputerDrivers: map[string]*MCPConfig{
					"desktop": {Transport: "stdio", Command: executable(t), Args: []string{"computer-driver"}},
				}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = client.Close() })
				return client
			}
			ctx := context.Background()
			client := dial()
			click := func(c *Client, callID string) (computer.Observation, error) {
				return c.Act(ctx, ComputerCall{Dir: dir, TaskID: 7, CallID: callID, Action: computer.Action{
					Service: "desktop", Name: "click", Arguments: json.RawMessage(`{"x":3,"y":4}`),
				}})
			}
			tools, err := client.ComputerCatalog(ctx, dir, "desktop")
			if err != nil || len(tools) != 3 {
				t.Fatalf("catalog = %d tools, %v", len(tools), err)
			}
			first, err := click(client, "a")
			if err != nil || text(t, first) != `click 1 {"x":3,"y":4}` {
				t.Fatalf("click = %s, %v", first.Raw, err)
			}
			if again, err := click(client, "a"); err != nil || !bytes.Equal(again.Raw, first.Raw) {
				t.Fatalf("repeated call = %s, %v", again.Raw, err)
			}
			if observed, err := client.Observe(ctx, "desktop"); err != nil || text(t, observed) != "clicks 1" ||
				observed.Screenshot == nil || observed.Screenshot.Width != 2 || observed.Screenshot.Height != 1 {
				t.Fatalf("observe = %+v, %v", observed, err)
			}

			hung, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			_, err = client.Act(hung, ComputerCall{Dir: dir, TaskID: 7, CallID: "b", Action: computer.Action{Service: "desktop", Name: "hang"}})
			cancel()
			if code(err) != "COMPUTER_UNCERTAIN" {
				t.Fatalf("cancelled action: %v", err)
			}
			if _, err := click(client, "c"); code(err) != "COMPUTER_UNCERTAIN" {
				t.Fatalf("action after uncertain outcome: %v", err)
			}

			// A new worker reads the journal: nothing uncertain is replayed.
			_ = client.Close()
			client = dial()
			if _, err := client.Act(ctx, ComputerCall{Dir: dir, TaskID: 7, CallID: "b", Action: computer.Action{Service: "desktop", Name: "hang"}}); code(err) != "COMPUTER_UNCERTAIN" {
				t.Fatalf("uncertain call after restart: %v", err)
			}
			if observed, err := client.Observe(ctx, "desktop"); err != nil || text(t, observed) != "clicks 0" {
				t.Fatalf("observe after restart = %v", err)
			}
			if err := client.VerifyComputer(ctx, dir, 7, "desktop"); err != nil {
				t.Fatal(err)
			}
			if next, err := click(client, "c"); err != nil || !strings.HasPrefix(text(t, next), "click 1 ") {
				t.Fatalf("action after verify: %v", err)
			}
			if again, err := click(client, "a"); err != nil || !bytes.Equal(again.Raw, first.Raw) {
				t.Fatalf("journaled result after restart = %s, %v", again.Raw, err)
			}
			if _, err := client.Act(ctx, ComputerCall{Dir: filepath.Base(dir), CallID: "d", Action: computer.Action{Service: "desktop", Name: "click"}}); code(err) != "EINVAL" {
				t.Fatalf("relative work directory: %v", err)
			}
		})
	}
}
