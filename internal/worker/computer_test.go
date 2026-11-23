package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
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
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("clicks %d", clicks.Load())}, &mcp.ImageContent{MIMEType: "image/png", Data: data.Bytes()}},
			StructuredContent: map[string]any{"screenshot_width": 2, "screenshot_height": 1}}, nil
	})
	// Viewer input lands in TEST_DESKTOP_LOG, one JSON call per line.
	for _, name := range []string{"type_text", "hotkey", "press_key", "drag", "move_cursor", "scroll"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: object}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			file, err := os.OpenFile(os.Getenv("TEST_DESKTOP_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				fmt.Fprintf(file, "%s %s\n", name, r.Params.Arguments)
				_ = file.Close()
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name}}}, nil
		})
	}
	server.AddTool(&mcp.Tool{Name: "click", InputSchema: object}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("click %d %s", clicks.Add(1), r.Params.Arguments)}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "hang", InputSchema: object}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		clicks.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	server.AddTool(&mcp.Tool{Name: "stubborn", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Like Cua's direct mode, this runs to the end despite cancellation.
		time.Sleep(time.Second)
		_ = os.WriteFile(os.Getenv("TEST_STUBBORN_DONE"), nil, 0o600)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
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
	server.AddTool(&mcp.Tool{Name: "agent_browser_get_text", InputSchema: object}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "text"}}}, nil
		}
	})
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
		// Cancel only once the driver has the action, so its outcome is unknown.
		for {
			observed, err := client.Observe(context.Background(), "desktop")
			if err != nil || time.Now().After(deadline) {
				t.Fatalf("observe during action: %v", err)
			}
			if text(t, observed) == "clicks 1" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := client.VerifyComputer(context.Background(), dir, 7, "desktop"); code(err) != "COMPUTER_BUSY" {
			t.Fatalf("verify during dispatched action: %v", err)
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

func TestComputerCancelledObservation(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		client := dial(nil)
		before, err := client.Observe(context.Background(), "browser")
		if err != nil {
			t.Fatal(err)
		}
		clicked := make(chan error, 1)
		go func() {
			observed, err := act(client, dir, "a", "browser", "agent_browser_click")
			if err == nil && text(t, observed) != "clicked 1" {
				err = fmt.Errorf("click = %s", observed.Raw)
			}
			clicked <- err
		}()
		time.Sleep(100 * time.Millisecond)
		// A cancelled observation leaves the shared driver session to the click.
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err = client.Act(ctx, ComputerCall{Dir: dir, CallID: "o", Action: computer.Action{Service: "browser", Name: "agent_browser_get_text"}})
		cancel()
		if err == nil {
			t.Fatal("expected the observation to be cancelled")
		}
		if err := <-clicked; err != nil {
			t.Fatalf("click during a cancelled observation: %v", err)
		}
		if after, err := client.Observe(context.Background(), "browser"); err != nil || text(t, after) != text(t, before) {
			t.Fatalf("driver session replaced after a cancelled observation: %v", err)
		}
		if _, err := act(client, dir, "b", "browser", "agent_browser_click"); err != nil {
			t.Fatalf("action after a cancelled observation: %v", err)
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

		closed, _ := os.ReadFile(filepath.Join(dir, "closed"))
		socket, session, _ := strings.Cut(string(closed), "\n")
		daemon := exec.Command(executable(t), "linger")
		isolate(daemon)
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { _ = daemon.Wait(); close(exited) }()
		t.Cleanup(func() { _ = daemon.Process.Kill() })
		if err := os.WriteFile(filepath.Join(socket, session+".pid"), []byte(strconv.Itoa(daemon.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := client.RestartBrowser(ctx, dir, 7); err != nil {
			t.Fatal(err)
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("restart reconnected while the old browser daemon ran")
		}

		// A crashed daemon's pid file whose pid now names another program.
		other := exec.Command("sleep", "60")
		if runtime.GOOS == "windows" {
			other = exec.Command("ping", "-n", "60", "127.0.0.1")
		}
		if err := other.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
		if err := os.WriteFile(filepath.Join(socket, session+".pid"), []byte(strconv.Itoa(other.Process.Pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := client.RestartBrowser(ctx, dir, 7); code(err) != "EIO" {
			t.Fatalf("restart over another program's pid: %v", err)
		}
		if !processAlive(other.Process.Pid) {
			t.Fatal("restart stopped a program that is not the browser daemon")
		}
		if _, err := act(client, dir, "c", "browser", "agent_browser_click"); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("action after an unconfirmed restart: %v", err)
		}
	})
}

func TestComputerDesktopCancel(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		done := filepath.Join(dir, "stubborn-done")
		client := dial(map[string]*MCPConfig{"desktop": {Transport: "stdio", Command: executable(t), Args: []string{"computer-driver"},
			Env: map[string]string{"TEST_STUBBORN_DONE": done}}})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := client.Act(ctx, ComputerCall{Dir: dir, TaskID: 7, CallID: "a", Action: computer.Action{Service: "desktop", Name: "stubborn"}})
		cancel()
		if code(err) != "COMPUTER_UNCERTAIN" && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancelled action: %v", err)
		}
		time.Sleep(1500 * time.Millisecond)
		if _, err := os.Stat(done); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("desktop action kept running after cancellation: %v", err)
		}
		if _, err := act(client, dir, "b", "desktop", "click"); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("action after cancellation: %v", err)
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
			if err != nil || len(tools) != 11 {
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

func TestBrowserSocketDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows temp directory is per user")
	}
	base := t.TempDir()
	// A temp directory that does not exist yet is created, as before.
	fresh := filepath.Join(base, "missing-tmp", "fresh")
	if err := privateSocketDir(fresh); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(fresh); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("fresh socket directory: %v, %v", info.Mode(), err)
	}
	if err := privateSocketDir(fresh); err != nil {
		t.Fatalf("own socket directory: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(fresh, link); err != nil {
		t.Fatal(err)
	}
	if err := privateSocketDir(link); code(err) != "EACCES" {
		t.Fatalf("symlinked socket directory: %v", err)
	}
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil || os.Chmod(open, 0o755) != nil {
		t.Fatal(err)
	}
	if err := privateSocketDir(open); code(err) != "EACCES" {
		t.Fatalf("socket directory open to others: %v", err)
	}
	// Another user pre-creates the directory with the right mode.
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		if err := os.Chown(other, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := privateSocketDir(other); code(err) != "EACCES" {
			t.Fatalf("socket directory of another user: %v", err)
		}
	} else if info, err := os.Lstat(other); err != nil || ownedPrivate(info, os.Getuid()+1) {
		t.Fatalf("socket directory accepted for another owner: %v", err)
	}
}

func TestComputerBrowserSocketRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows temp directory is per user")
	}
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		socket := browserSocketDir(dir)
		t.Cleanup(func() { _ = os.Remove(socket) })
		if err := os.Symlink(t.TempDir(), socket); err != nil {
			t.Fatal(err)
		}
		client := dial(nil)
		if _, err := client.Observe(context.Background(), "browser"); code(err) != "EACCES" {
			t.Fatalf("browser with a planted socket directory: %v", err)
		}
	})
}

func TestComputerStatus(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		log := filepath.Join(dir, "cli")
		browser := func(env map[string]string) *MCPConfig {
			env["TEST_BROWSER_CLOSED"] = filepath.Join(dir, "closed")
			env["TEST_BROWSER_CLI"] = log
			return &MCPConfig{Transport: "stdio", Command: executable(t), Args: []string{"browser-driver"}, Env: env}
		}
		client := dial(map[string]*MCPConfig{"browser": browser(map[string]string{})})
		ctx := context.Background()
		calls := func() []string {
			data, _ := os.ReadFile(log)
			return strings.Split(strings.TrimSpace(string(data)), "\n")
		}

		if status, err := client.PrepareComputer(ctx, dir, "desktop"); err != nil || !status.Available {
			t.Fatalf("desktop prepare: %+v, %v", status, err)
		}
		status, err := client.PrepareComputer(ctx, dir, "browser")
		if err != nil || !status.Available || status.Mode != "headless" || status.SharedDesktop {
			t.Fatalf("browser prepare: %+v, %v", status, err)
		}
		if got := calls(); len(got) != 1 || !strings.HasPrefix(got[0], "set viewport 1280 720 --json") {
			t.Fatalf("prepare commands: %q", got)
		}
		if status, err := client.InstallBrowser(ctx, dir); err != nil || !status.Available {
			t.Fatalf("install: %+v, %v", status, err)
		}
		if got := calls(); !strings.HasPrefix(got[len(got)-1], "install --use-system-ca") {
			t.Fatalf("install command: %q", got)
		}
		if _, err := client.SetBrowserMode(ctx, dir, "windowed"); code(err) != "EINVAL" {
			t.Fatalf("invalid mode: %v", err)
		}

		if _, err := act(client, dir, "a", "browser", "agent_browser_wedge"); err != nil {
			t.Fatal(err)
		}
		if err := client.SettleComputer(ctx, dir, "browser"); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("settle with an unknown outcome: %v", err)
		}
		headless := calls()[0]
		status, err = client.SetBrowserMode(ctx, dir, "headed")
		if err != nil || !status.Available || status.Mode != "headed" || !status.SharedDesktop {
			t.Fatalf("headed mode: %+v, %v", status, err)
		}
		if closed, err := os.ReadFile(filepath.Join(dir, "closed")); err != nil || len(closed) == 0 {
			t.Fatalf("mode change did not restart the browser: %v", err)
		}
		if got := calls(); got[len(got)-1] == headless {
			t.Fatalf("headed browser prepared like the headless one: %q", got)
		}
		if err := client.SettleComputer(ctx, dir, "browser"); err != nil {
			t.Fatalf("settle after restart: %v", err)
		}
		if _, err := act(client, dir, "b", "browser", "agent_browser_click"); err != nil {
			t.Fatalf("action after mode change: %v", err)
		}
		if err := client.SettleComputer(ctx, dir, "desktop"); err != nil {
			t.Fatalf("desktop settle: %v", err)
		}

		broken := dial(map[string]*MCPConfig{
			"browser": browser(map[string]string{"TEST_BROWSER_FAIL": "1"}),
			"desktop": {Transport: "stdio", Command: filepath.Join(dir, "missing-driver")},
		})
		if status, err := broken.PrepareComputer(ctx, dir, "browser"); err != nil || status.Available || !strings.Contains(status.Error, "browser unavailable") {
			t.Fatalf("failing browser prepare: %+v, %v", status, err)
		}
		if status, err := broken.PrepareComputer(ctx, dir, "desktop"); err != nil || status.Available || status.Error == "" {
			t.Fatalf("missing desktop driver prepare: %+v, %v", status, err)
		}
		if status, err := broken.SetBrowserMode(ctx, dir, "headless"); err != nil || status.Available {
			t.Fatalf("mode change whose browser does not prepare: %+v, %v", status, err)
		}
		if err := broken.SettleComputer(ctx, dir, "browser"); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("settle after an incomplete restart: %v", err)
		}
	})
}

func TestComputerDesktopView(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		log := filepath.Join(dir, "desktop-input")
		client := dial(map[string]*MCPConfig{"desktop": {Transport: "stdio", Command: executable(t), Args: []string{"computer-driver"},
			Env: map[string]string{"TEST_DESKTOP_LOG": log}}})
		ctx := context.Background()
		view, err := client.OpenView(ctx, dir, "desktop")
		if err != nil {
			t.Fatal(err)
		}
		frame, err := view.Next(ctx)
		if err != nil || frame.Seq != 1 || frame.MimeType != "image/png" || frame.Width != 2 || frame.Height != 1 || len(frame.Data) == 0 {
			t.Fatalf("first frame: %+v, %v", frame, err)
		}
		waiting, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		if _, err := view.Next(waiting); err == nil || waiting.Err() == nil {
			t.Fatalf("frame before acknowledgement: %v", err)
		}
		cancel()
		if err := view.Send(ctx, ViewMessage{Type: "ack", Seq: 2}); code(err) != "EINVAL" {
			t.Fatalf("stale acknowledgement: %v", err)
		}
		if err := view.Send(ctx, ViewMessage{Type: "ack", Seq: 1}); err != nil {
			t.Fatal(err)
		}
		if frame, err := view.Next(ctx); err != nil || frame.Seq != 2 {
			t.Fatalf("frame after acknowledgement: %+v, %v", frame, err)
		}
		for _, message := range []ViewMessage{
			{Type: "text", Text: "hi"},
			{Type: "key", Key: "c", Modifiers: 2 | 8},
			{Type: "key", Key: "Enter"},
			{Type: "pointer", Event: "down", X: 10, Y: 10, Button: "left"},
			{Type: "pointer", Event: "move", X: 30, Y: 30},
			{Type: "pointer", Event: "up", X: 50, Y: 50, Button: "left"},
			{Type: "pointer", Event: "move", X: 5, Y: 6},
			{Type: "pointer", Event: "scroll", X: 5, Y: 6, DeltaY: -120},
			{Type: "pointer", Event: "down", X: 7, Y: 7, Button: "left"},
			{Type: "pointer", Event: "up", X: 8, Y: 8, Button: "left"},
		} {
			if err := view.Send(ctx, message); err != nil {
				t.Fatalf("send %+v: %v", message, err)
			}
		}
		data, _ := os.ReadFile(log)
		got := string(data)
		for _, want := range []string{
			`type_text {"session":"` + view.id + `","target":{"display_id":"primary","kind":"desktop"},"text":"hi"}`,
			`hotkey {"keys":["ctrl","shift","c"]`, `press_key {"key":"Enter"`,
			`drag {"button":"left","from_x":10,"from_y":10,`, `"to_x":50,"to_y":50}`,
			`move_cursor {"session"`, `scroll {"amount":3,"direction":"up"`,
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("desktop input %q lacks %q", got, want)
			}
		}
		if strings.Count(got, "move_cursor") != 1 {
			t.Fatalf("pointer move while held moved the cursor: %q", got)
		}
		if observed, err := client.Observe(ctx, "desktop"); err != nil || text(t, observed) != "clicks 1" {
			t.Fatalf("short press did not click: %v", err)
		}
		if err := view.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := view.Next(ctx); code(err) != "ENOENT" {
			t.Fatalf("next after close: %v", err)
		}
	})
}

// testBrowserStream stands in for agent-browser's stream and command API.
type testBrowserStream struct {
	*httptest.Server
	sessionFile string
	mu          sync.Mutex
	commands    []string
	messages    []string
	wrong       atomic.Bool
}

func newTestBrowserStream(t *testing.T, dir string) *testBrowserStream {
	stream := &testBrowserStream{sessionFile: filepath.Join(dir, "browser-session")}
	stream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/command" {
			body, _ := io.ReadAll(r.Body)
			var command map[string]any
			_ = json.Unmarshal(body, &command)
			if command["action"] == "session_info" {
				session, _ := os.ReadFile(stream.sessionFile)
				if stream.wrong.Load() {
					session = []byte("other")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"session": string(session)}})
				return
			}
			stream.mu.Lock()
			stream.commands = append(stream.commands, string(body))
			stream.mu.Unlock()
			if command["key"] == "fail" {
				http.Error(w, "lost", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
			return
		}
		if r.URL.Query().Get("pacing") != "ack" {
			http.Error(w, "no pacing", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		frame := func(seq int) error {
			data, _ := json.Marshal(map[string]any{"type": "frame", "seq": seq, "data": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("jpeg %d", seq))),
				"metadata": map[string]any{"deviceWidth": 1280, "deviceHeight": 720, "timestamp": 1.5}})
			return conn.Write(ctx, websocket.MessageText, data)
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"status"}`))
		if frame(1) != nil {
			return
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			stream.mu.Lock()
			stream.messages = append(stream.messages, string(data))
			stream.mu.Unlock()
			if strings.Contains(string(data), `"ack"`) && frame(2) != nil {
				return
			}
		}
	}))
	t.Cleanup(stream.Close)
	return stream
}

func (s *testBrowserStream) port() string { return s.URL[strings.LastIndex(s.URL, ":")+1:] }

func (s *testBrowserStream) log() (commands, messages string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.commands, "\n"), strings.Join(s.messages, "\n")
}

func TestComputerBrowserView(t *testing.T) {
	eachComputer(t, func(t *testing.T, dir string, dial func(map[string]*MCPConfig) *Client) {
		stream := newTestBrowserStream(t, dir)
		client := dial(map[string]*MCPConfig{"browser": {Transport: "stdio", Command: executable(t), Args: []string{"browser-driver"},
			Env: map[string]string{"TEST_BROWSER_CLOSED": filepath.Join(dir, "closed"), "TEST_BROWSER_STREAM_PORT": stream.port(),
				"TEST_BROWSER_SESSION_FILE": stream.sessionFile}}})
		ctx := context.Background()
		stream.wrong.Store(true)
		if _, err := client.OpenView(ctx, dir, "browser"); err == nil || !strings.Contains(err.Error(), "another browser session") {
			t.Fatalf("view of another session: %v", err)
		}
		stream.wrong.Store(false)
		view, err := client.OpenView(ctx, dir, "browser")
		if err != nil {
			t.Fatal(err)
		}
		frame, err := view.Next(ctx)
		if err != nil || frame.Seq != 1 || string(frame.Data) != "jpeg 1" || frame.MimeType != "image/jpeg" || frame.Width != 1280 || frame.Height != 720 || frame.Timestamp != 1.5 {
			t.Fatalf("first frame: %+v, %v", frame, err)
		}
		if err := view.Send(ctx, ViewMessage{Type: "ack", Seq: 3}); code(err) != "EINVAL" {
			t.Fatalf("stale acknowledgement: %v", err)
		}
		if err := view.Send(ctx, ViewMessage{Type: "ack", Seq: 1, MaxFps: 10}); err != nil {
			t.Fatal(err)
		}
		if frame, err := view.Next(ctx); err != nil || frame.Seq != 2 {
			t.Fatalf("frame after acknowledgement: %+v, %v", frame, err)
		}
		for _, message := range []ViewMessage{
			{Type: "text", Text: "hi"},
			{Type: "key", Key: "a", Modifiers: 4},
			{Type: "key", Key: "k", Modifiers: 2 | 8},
			{Type: "pointer", Event: "down", X: 1.4, Y: 2.6, Button: "left"},
			{Type: "pointer", Event: "move", X: 9, Y: 9},
			{Type: "pointer", Event: "up", X: 10, Y: 11},
			{Type: "pointer", Event: "scroll", X: 3, Y: 4, DeltaY: 99.6},
			{Type: "pointer", Event: "down", X: 5, Y: 5, Button: "right"},
		} {
			if err := view.Send(ctx, message); err != nil {
				t.Fatalf("send %+v: %v", message, err)
			}
		}
		if err := view.Close(); err != nil {
			t.Fatal(err)
		}
		commands, messages := stream.log()
		for _, want := range []string{
			`{"action":"keyboard","subaction":"insertText","text":"hi"}`,
			`{"action":"selectall","selector":":focus"}`,
			`{"action":"press","key":"Control+Shift+k"}`,
			`{"action":"input_mouse","button":"left","clickCount":1,"modifiers":0,"type":"mousePressed","x":1,"y":3}`,
			`{"action":"input_mouse","button":"left","clickCount":1,"modifiers":0,"type":"mouseMoved","x":9,"y":9}`,
			`{"action":"input_mouse","button":"left","clickCount":1,"type":"mouseReleased","x":10,"y":11}`,
			`{"action":"input_mouse","button":"none","clickCount":1,"deltaX":0,"deltaY":100,"modifiers":0,"type":"mouseWheel","x":3,"y":4}`,
			`{"action":"input_mouse","button":"right","clickCount":1,"type":"mouseReleased","x":-1,"y":-1}`,
		} {
			if !strings.Contains(commands, want) {
				t.Fatalf("browser commands %s lack %s", commands, want)
			}
		}
		if !strings.Contains(messages, `{"seq":1,"type":"ack"}`) || !strings.Contains(messages, `{"maxFps":10,"type":"config"}`) {
			t.Fatalf("stream messages: %s", messages)
		}
		if _, err := view.Next(ctx); code(err) != "ENOENT" {
			t.Fatalf("next after close: %v", err)
		}

		view, err = client.OpenView(ctx, dir, "browser")
		if err != nil {
			t.Fatal(err)
		}
		if err := view.Send(ctx, ViewMessage{Type: "key", Key: "fail"}); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("unconfirmed input: %v", err)
		}
		if err := view.Send(ctx, ViewMessage{Type: "text", Text: "more"}); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("input after unconfirmed input: %v", err)
		}
		if _, err := act(client, dir, "a", "browser", "agent_browser_click"); code(err) != "COMPUTER_UNCERTAIN" {
			t.Fatalf("agent action after unconfirmed input: %v", err)
		}
		if err := client.RestartBrowser(ctx, dir, 7); err != nil {
			t.Fatal(err)
		}
	})
}
