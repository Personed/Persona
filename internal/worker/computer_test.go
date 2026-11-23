package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
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
	server.AddTool(&mcp.Tool{Name: "crash", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		os.Exit(1)
		return nil, nil
	})
	return server
}

// testBrowserDriver stands in for agent-browser: a slow click, an action with
// an unknown outcome, and an observation that names the driver process.
func testBrowserDriver() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "browser", Version: "1"}, nil)
	object := map[string]any{"type": "object"}
	var clicks atomic.Int64
	server.AddTool(&mcp.Tool{Name: "agent_browser_click", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		time.Sleep(500 * time.Millisecond)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("clicked %d", clicks.Add(1))}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "agent_browser_wedge", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "unknown"}},
			StructuredContent: map[string]any{"response": map[string]any{"code": "outcome_unknown"}},
		}, nil
	})
	server.AddTool(&mcp.Tool{Name: "agent_browser_snapshot", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("pid %d", os.Getpid())}}}, nil
	})
	return server
}

// eachComputer runs test over stdio and SSH with the test drivers. dial
// starts a new worker; drivers replace the test drivers by service.
func eachComputer(t *testing.T, test func(t *testing.T, dir string, dial func(drivers map[string]*MCPConfig) *Client)) {
	targets := map[string]Transport{"stdio": Local(executable(t))}
	if _, err := exec.LookPath("ssh"); err == nil {
		targets["ssh"] = SSH(sshServer(t))
	}
	for name, transport := range targets {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			test(t, dir, func(drivers map[string]*MCPConfig) *Client {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				all := map[string]*MCPConfig{
					"desktop": {Transport: "stdio", Command: executable(t), Args: []string{"computer-driver"}},
					"browser": {Transport: "stdio", Command: executable(t), Args: []string{"browser-driver"},
						Env: map[string]string{"TEST_BROWSER_CLOSED": filepath.Join(dir, "closed")}},
				}
				for service, driver := range drivers {
					all[service] = driver
				}
				client, err := Dial(ctx, transport, Options{WorkDir: dir, ComputerDrivers: all})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = client.Close() })
				return client
			})
		})
	}
}

func act(c *Client, dir, callID, service, name string) (computer.Observation, error) {
	return c.Act(context.Background(), ComputerCall{Dir: dir, TaskID: 7, CallID: callID, Action: computer.Action{Service: service, Name: name}})
}

func TestComputerVerifyDuringAction(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		client := dial(nil)
		ctx, cancel := context.WithCancel(context.Background())
		hung := make(chan error, 1)
		go func() {
			_, err := client.Act(ctx, ComputerCall{Dir: dir, TaskID: 7, CallID: "a", Action: computer.Action{Service: "desktop", Name: "hang"}})
			hung <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := client.VerifyComputer(context.Background(), dir, 7, "desktop")
			if code(err) == "COMPUTER_BUSY" {
				break
			}
			if err != nil || time.Now().After(deadline) {
				t.Fatalf("verify during action: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err := <-hung; err == nil {
			t.Fatal("cancelled action succeeded")
		}
		// The verification above did not clear what the action left uncertain.
		if _, err := act(client, dir, "b", "desktop", "click"); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("action after uncertain outcome: %v", err)
		}
		if err := client.VerifyComputer(context.Background(), dir, 7, "desktop"); err != nil {
			t.Fatal(err)
		}
		if _, err := act(client, dir, "b", "desktop", "click"); err != nil {
			t.Fatalf("action after verify: %v", err)
		}
	})
}

func TestComputerBrowserCancelAfterDispatch(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		client := dial(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		_, _ = client.Act(ctx, ComputerCall{Dir: dir, TaskID: 7, CallID: "a", Action: computer.Action{Service: "browser", Name: "agent_browser_click"}})
		cancel()
		// The dispatched click settles and is journaled, not left uncertain.
		saved, err := act(client, dir, "a", "browser", "agent_browser_click")
		if err != nil || text(t, saved) != "clicked 1" {
			t.Fatalf("settled click = %v", err)
		}
		if next, err := act(client, dir, "b", "browser", "agent_browser_click"); err != nil || text(t, next) != "clicked 2" {
			t.Fatalf("next click = %v", err)
		}
	})
}

func TestComputerBrowserRestart(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		client := dial(nil)
		ctx := context.Background()
		before, err := client.Observe(ctx, "browser")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := act(client, dir, "a", "browser", "agent_browser_wedge"); err != nil {
			t.Fatal(err)
		}
		if err := client.VerifyComputer(ctx, dir, 7, "browser"); code(err) != "COMPUTER_RESTART_REQUIRED" {
			t.Fatalf("browser verify: %v", err)
		}
		if _, err := act(client, dir, "b", "browser", "agent_browser_click"); code(err) != "COMPUTER_UNCERTAIN" ||
			!strings.Contains(err.Error(), "agent_browser_restart") {
			t.Fatalf("action after unknown outcome: %v", err)
		}
		if err := client.RestartBrowser(ctx, dir, 7); err != nil {
			t.Fatal(err)
		}
		if closed, err := os.ReadFile(filepath.Join(dir, "closed")); err != nil || len(closed) == 0 {
			t.Fatalf("browser daemon not closed for the session: %q, %v", closed, err)
		}
		after, err := client.Observe(ctx, "browser")
		if err != nil || text(t, after) == text(t, before) {
			t.Fatalf("driver not restarted: %v", err)
		}
		if _, err := act(client, dir, "b", "browser", "agent_browser_click"); err != nil {
			t.Fatalf("action after restart: %v", err)
		}
	})
}

func TestComputerDriverExit(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		client := dial(nil)
		if _, err := act(client, dir, "a", "desktop", "crash"); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("action whose driver exited: %v", err)
		}
		if observed, err := client.Observe(context.Background(), "desktop"); err != nil || text(t, observed) != "clicks 0" {
			t.Fatalf("observe with a new driver: %v", err)
		}
		if _, err := act(client, dir, "b", "desktop", "click"); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("action after the driver exited: %v", err)
		}
	})
}

func TestComputerReplayWithoutDriver(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		client := dial(nil)
		first, err := act(client, dir, "a", "desktop", "click")
		if err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
		client = dial(map[string]*MCPConfig{"desktop": {Transport: "stdio", Command: filepath.Join(dir, "missing-driver")}})
		if again, err := act(client, dir, "a", "desktop", "click"); err != nil || !bytes.Equal(again.Raw, first.Raw) {
			t.Fatalf("saved result without a driver = %v", err)
		}
		if _, err := act(client, dir, "b", "desktop", "click"); code(err) != "ENOENT" {
			t.Fatalf("new action without a driver: %v", err)
		}
	})
}

func TestComputerLargeResult(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		client := dial(nil)
		// A journaled result larger than a worker frame.
		big, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", MaxFrame+1)}}})
		journal := filepath.Join(dir, ".persona", "computer")
		if err := os.MkdirAll(journal, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := appendJournal(journal, 7, journalEntry{State: "result", CallID: "a", Service: "desktop", Result: big}); err != nil {
			t.Fatal(err)
		}
		if next, err := act(client, dir, "b", "desktop", "click"); err != nil || !strings.HasPrefix(text(t, next), "click 1 ") {
			t.Fatalf("action after a large result: %v", err)
		}
		if _, err := act(client, dir, "a", "desktop", "click"); code(err) != "E2BIG" {
			t.Fatalf("saved large result: %v", err)
		}
	})
}

func TestComputerProtocolErrorReplay(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		client := dial(nil)
		_, first := act(client, dir, "a", "desktop", "missing")
		var want *Error
		if !errors.As(first, &want) || want.Code != "MCP_PROTOCOL" || want.MCPCode == 0 {
			t.Fatalf("rejected action: %v", first)
		}
		_ = client.Close()
		client = dial(nil)
		_, again := act(client, dir, "a", "desktop", "missing")
		var got *Error
		if !errors.As(again, &got) || *got != *want {
			t.Fatalf("replayed rejection = %v, want %v", again, first)
		}
		if _, err := act(client, dir, "b", "desktop", "click"); err != nil {
			t.Fatalf("action after a rejection: %v", err)
		}
	})
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
			if err != nil || len(tools) != 4 {
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
