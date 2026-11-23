package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // screenshot dimensions
	_ "image/png"  // screenshot dimensions
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/computer"
)

// ComputerInput is one call to the work computer's desktop (cua-driver) or
// browser (agent-browser) driver. Dir is the Bot work directory; its .persona
// folder holds the browser profile and the computer-use journal.
type ComputerInput struct {
	Action    string          `json:"action"` // catalog, call, verify, restart
	Service   string          `json:"service"`
	Dir       string          `json:"dir"`
	TaskID    int64           `json:"taskId,omitempty"`
	CallID    string          `json:"callId,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Driver replaces the default `cua-driver mcp` or `agent-browser mcp`.
	Driver *MCPConfig `json:"driver,omitempty"`
}

// ComputerResult answers a ComputerInput. Result is the native CallToolResult.
type ComputerResult struct {
	Tools  []*mcp.Tool     `json:"tools,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	// Replayed is set when Result is the journaled result of an earlier dispatch.
	Replayed bool `json:"replayed,omitempty"`
}

// journalEntry is one line of a task's computer-use journal: intent before
// dispatch, then result, uncertain, or cancelled (not sent). verified (desktop) and restarted
// (browser) clear a service's uncertainty.
type journalEntry struct {
	State     string          `json:"state"`
	CallID    string          `json:"callId,omitempty"`
	Service   string          `json:"service"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	// Error is the worker error a driver's protocol rejection returned.
	Error *Error `json:"error,omitempty"`
}

type computerDriver struct {
	session *mcpSession
	done    chan struct{} // closed when the driver connection ends
}

// browserObservations read the page without changing it, as in browser-tools.mjs.
var browserObservations = map[string]bool{
	"agent_browser_observe": true, "agent_browser_snapshot": true, "agent_browser_screenshot": true,
	"agent_browser_get_text": true, "agent_browser_get_url": true, "agent_browser_get_title": true,
	"agent_browser_tab_list": true,
}

func observation(service, name string) bool {
	if service == "browser" {
		return browserObservations[name]
	}
	return strings.HasPrefix(name, "get_") || strings.HasPrefix(name, "list_") || name == "screenshot" || name == "zoom"
}

var errComputerUncertain = &Error{Code: "COMPUTER_UNCERTAIN", Message: "A computer action's outcome is unknown. Observe the computer, confirm the task state, then verify before more actions. Do not replay the action."}

var errBrowserUncertain = &Error{Code: "COMPUTER_UNCERTAIN", Message: "A browser action's outcome is unknown. Observe the page, then use agent_browser_restart before more browser actions. Restart closes this Bot's tabs and unsaved input. Do not replay the action."}

var errComputerBusy = &Error{Code: "COMPUTER_BUSY", Message: "A computer action is still running. Wait for it to settle and observe before recovery."}

var errBrowserRestart = &Error{Code: "COMPUTER_RESTART_REQUIRED", Message: "An unknown browser outcome is cleared only by agent_browser_restart. Restart closes this Bot's tabs and unsaved input."}

// browserCallTimeout bounds a dispatched browser action, which cancellation
// does not abandon, as in browser-service.mjs.
const browserCallTimeout = 125 * time.Second

func uncertain(service string) *Error {
	if service == "browser" {
		return errBrowserUncertain
	}
	return errComputerUncertain
}

func (s *server) computer(ctx context.Context, input ComputerInput) (*ComputerResult, error) {
	if input.Service != "desktop" && input.Service != "browser" {
		return nil, codeError("EINVAL", "unknown computer service "+input.Service, "")
	}
	if input.Dir == "" || !filepath.IsAbs(input.Dir) {
		return nil, codeError("EINVAL", "an absolute work directory is required", input.Dir)
	}
	journal := filepath.Join(input.Dir, ".persona", "computer")
	if err := os.MkdirAll(journal, 0o700); err != nil {
		return nil, err
	}
	marker := filepath.Join(journal, input.Service+"-uncertain")
	key := input.Service + "\n" + input.Dir
	switch input.Action {
	case "verify":
		// Recovery never waits behind an action: one that settles later must
		// not have its uncertainty cleared by a verification made before it.
		lane := s.computerLane(key)
		if !lane.TryLock() {
			return nil, errComputerBusy
		}
		defer lane.Unlock()
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			return &ComputerResult{}, nil
		} else if err != nil {
			return nil, err
		}
		if input.Service == "browser" {
			return nil, errBrowserRestart
		}
		if err := appendJournal(journal, input.TaskID, journalEntry{State: "verified", Service: input.Service}); err != nil {
			return nil, err
		}
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return &ComputerResult{}, nil
	case "restart":
		if input.Service != "browser" {
			return nil, codeError("EINVAL", "only the browser restarts", "")
		}
		lane := s.computerLane(key)
		if !lane.TryLock() {
			return nil, errComputerBusy
		}
		defer lane.Unlock()
		return s.restartBrowser(ctx, input, journal, marker)
	case "catalog":
		driver, err := s.computerDriver(ctx, key, input)
		if err != nil {
			return nil, mcpError(ctx, err)
		}
		var tools []*mcp.Tool
		for tool, err := range driver.session.session.Tools(ctx, nil) {
			if err != nil {
				s.dropBroken(ctx, key, driver, err)
				return nil, mcpError(ctx, err)
			}
			tools = append(tools, tool)
		}
		return &ComputerResult{Tools: tools}, nil
	case "call":
	default:
		return nil, codeError("EINVAL", "unknown computer action "+input.Action, "")
	}
	if input.Name == "" {
		return nil, codeError("EINVAL", "tool name is required", "")
	}
	params := &mcp.CallToolParams{Name: input.Name}
	if len(input.Arguments) > 0 {
		params.Arguments = input.Arguments
	}
	if observation(input.Service, input.Name) {
		driver, err := s.computerDriver(ctx, key, input)
		if err != nil {
			return nil, mcpError(ctx, err)
		}
		result, err := driver.session.session.CallTool(ctx, params)
		if err != nil {
			s.dropBroken(ctx, key, driver, err)
			return nil, mcpError(ctx, err)
		}
		raw, err := json.Marshal(result)
		return &ComputerResult{Result: raw}, err
	}
	if input.CallID == "" {
		return nil, codeError("EINVAL", "a call ID is required for computer actions", "")
	}
	lane := s.computerLane(key)
	lane.Lock()
	defer lane.Unlock()
	// Saved outcomes come from the journal alone, so recovery works while the
	// driver cannot start.
	entries, err := readJournal(journal, input.TaskID)
	if err != nil {
		return nil, err
	}
	if previous, ok := entries[input.CallID]; ok && previous.State != "cancelled" {
		switch {
		case previous.State == "result" && previous.Error != nil:
			return nil, previous.Error
		case previous.State == "result":
			return &ComputerResult{Result: previous.Result, Replayed: true}, nil
		}
		return nil, uncertain(input.Service)
	}
	if _, err := os.Stat(marker); err == nil {
		return nil, uncertain(input.Service)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	driver, err := s.computerDriver(ctx, key, input)
	if err != nil {
		return nil, mcpError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, toError(err)
	}
	// The marker outlives a worker crash, so an action interrupted mid-flight
	// stays uncertain after restart.
	if err := os.WriteFile(marker, []byte(input.CallID), 0o600); err != nil {
		return nil, err
	}
	intent := journalEntry{State: "intent", CallID: input.CallID, Service: input.Service, Name: input.Name, Arguments: input.Arguments}
	if err := appendJournal(journal, input.TaskID, intent); err != nil {
		_ = os.Remove(marker)
		return nil, err
	}
	// Cancellation that arrives while the intent syncs still wins: nothing was sent.
	if err := ctx.Err(); err != nil {
		if err := appendJournal(journal, input.TaskID, journalEntry{State: "cancelled", CallID: input.CallID, Service: input.Service}); err != nil {
			return nil, err
		}
		if err := os.Remove(marker); err != nil {
			return nil, err
		}
		return nil, toError(ctx.Err())
	}
	callCtx := ctx
	if input.Service == "browser" {
		// Cancellation stops queued browser work; dispatched work settles first.
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), browserCallTimeout)
		defer cancel()
	}
	result, callErr := driver.session.session.CallTool(callCtx, params)
	var wire *jsonrpc.Error
	if callErr != nil && !errors.As(callErr, &wire) || callErr == nil && uncertainResult(result) {
		switch {
		case callErr != nil && input.Service == "desktop":
			// Cua finishes a direct action and ignores MCP cancellation, so
			// stopping its process is what cancels the action.
			s.dropBroken(context.WithoutCancel(callCtx), key, driver, callErr)
		case callErr != nil:
			s.dropBroken(callCtx, key, driver, callErr)
		}
		entry := journalEntry{State: "uncertain", CallID: input.CallID, Service: input.Service}
		if callErr == nil {
			entry.Result, _ = json.Marshal(result)
		}
		if err := appendJournal(journal, input.TaskID, entry); err != nil {
			return nil, err
		}
		if callErr != nil {
			return nil, uncertain(input.Service)
		}
		return &ComputerResult{Result: entry.Result}, nil
	}
	// A protocol error means the driver rejected the request without acting.
	entry := journalEntry{State: "result", CallID: input.CallID, Service: input.Service}
	if callErr == nil {
		entry.Result, _ = json.Marshal(result)
	} else {
		entry.Error = mcpError(callCtx, callErr)
	}
	if err := appendJournal(journal, input.TaskID, entry); err != nil {
		return nil, err
	}
	if err := os.Remove(marker); err != nil {
		return nil, err
	}
	if entry.Error != nil {
		return nil, entry.Error
	}
	return &ComputerResult{Result: entry.Result}, nil
}

// restartBrowser closes the Bot browser and its daemon, then reconnects with
// the same profile. Only this clears an unknown browser outcome.
func (s *server) restartBrowser(ctx context.Context, input ComputerInput, journal, marker string) (*ComputerResult, error) {
	key := input.Service + "\n" + input.Dir
	config, err := driverConfig(input)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(marker, []byte("restart"), 0o600); err != nil {
		return nil, err
	}
	s.computersMu.Lock()
	old := s.computers[key]
	delete(s.computers, key)
	s.computersMu.Unlock()
	if old != nil {
		old.session.close()
	}
	closeBrowser(ctx, config)
	if _, err := s.computerDriver(ctx, key, input); err != nil {
		return nil, mcpError(ctx, err)
	}
	if err := appendJournal(journal, input.TaskID, journalEntry{State: "restarted", Service: input.Service}); err != nil {
		return nil, err
	}
	if err := os.Remove(marker); err != nil {
		return nil, err
	}
	return &ComputerResult{}, nil
}

// closeBrowser runs `agent-browser close` for the Bot session, so the daemon
// that may still run earlier commands exits before the client reconnects.
func closeBrowser(ctx context.Context, config *MCPConfig) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	env := stdioEnvironment(config.Env)
	path, err := ResolveProgram(ctx, config.Command, env)
	if err != nil || path == "" {
		return
	}
	cmd := exec.CommandContext(ctx, path, "close")
	cmd.Env = env
	_ = cmd.Run()
}

// uncertainResult recognizes agent-browser results that report an unknown outcome.
func uncertainResult(result *mcp.CallToolResult) bool {
	structured, _ := json.Marshal(result.StructuredContent)
	var response struct {
		Response struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		} `json:"response"`
	}
	_ = json.Unmarshal(structured, &response)
	return response.Response.Code == "outcome_unknown" || response.Response.Code == "page_wedged" ||
		strings.HasPrefix(response.Response.Error, "CDP command timed out:")
}

// computerLane serializes a service's mutations and recovery in one work directory.
func (s *server) computerLane(key string) *sync.Mutex {
	s.computersMu.Lock()
	defer s.computersMu.Unlock()
	if s.computerLanes == nil {
		s.computerLanes = map[string]*sync.Mutex{}
	}
	lane := s.computerLanes[key]
	if lane == nil {
		lane = &sync.Mutex{}
		s.computerLanes[key] = lane
	}
	return lane
}

// computerDriver returns the open driver session, replacing one whose driver exited.
func (s *server) computerDriver(ctx context.Context, key string, input ComputerInput) (*computerDriver, error) {
	s.computersMu.Lock()
	defer s.computersMu.Unlock()
	if driver := s.computers[key]; driver != nil {
		select {
		case <-driver.done:
			delete(s.computers, key)
			driver.session.close()
		default:
			return driver, nil
		}
	}
	config, err := driverConfig(input)
	if err != nil {
		return nil, err
	}
	session, err := openMCP(ctx, MCPInput{Config: config}, nil)
	if err != nil {
		return nil, err
	}
	driver := &computerDriver{session: session, done: make(chan struct{})}
	go func() {
		_ = session.session.Wait()
		close(driver.done)
	}()
	s.computers[key] = driver
	return driver, nil
}

// dropBroken discards a driver session after a transport failure. Protocol
// errors and the end of the call's own context keep the session, which other
// calls may still be using.
func (s *server) dropBroken(ctx context.Context, key string, driver *computerDriver, err error) {
	var wire *jsonrpc.Error
	if errors.As(err, &wire) || ctx.Err() != nil {
		return
	}
	s.computersMu.Lock()
	if s.computers[key] == driver {
		delete(s.computers, key)
	}
	s.computersMu.Unlock()
	driver.session.kill()
}

func driverConfig(input ComputerInput) (*MCPConfig, error) {
	config := MCPConfig{Transport: "stdio", Command: "cua-driver", Args: []string{"mcp"}}
	if input.Service == "browser" {
		config.Command = "agent-browser"
	}
	if input.Driver != nil {
		config = *input.Driver
	}
	config.Env = map[string]string{}
	if input.Driver != nil {
		for key, value := range input.Driver.Env {
			config.Env[key] = value
		}
	}
	for _, key := range []string{"DISPLAY", "XAUTHORITY", "XDG_RUNTIME_DIR", "WAYLAND_DISPLAY", "TMPDIR", "AGENT_BROWSER_EXECUTABLE_PATH", "AGENT_BROWSER_ARGS"} {
		if value, ok := os.LookupEnv(key); ok {
			if _, set := config.Env[key]; !set {
				config.Env[key] = value
			}
		}
	}
	if input.Service != "browser" {
		return &config, nil
	}
	// The Bot browser keeps one profile and session per work directory.
	sum := sha256.Sum256([]byte(input.Dir))
	session := hex.EncodeToString(sum[:])[:24]
	state := filepath.Join(input.Dir, ".persona")
	downloads := filepath.Join(input.Dir, "downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return nil, err
	}
	mode := "headless"
	if data, err := os.ReadFile(filepath.Join(state, "browser-mode.json")); err == nil {
		_ = json.Unmarshal(data, &mode)
	}
	settings, _ := json.Marshal(map[string]any{
		"profile": filepath.Join(state, "browser"), "headed": mode == "headed", "downloadPath": downloads,
		"contentBoundaries": true, "maxOutput": 32000, "useSystemCa": true,
	})
	configPath := filepath.Join(state, "browser.json")
	if err := os.WriteFile(configPath, settings, 0o600); err != nil {
		return nil, err
	}
	// macOS per-user temp paths can exceed the Unix socket path limit.
	base := os.TempDir()
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	socket := filepath.Join(base, "persona-browser-"+session)
	if err := os.MkdirAll(socket, 0o700); err != nil {
		return nil, err
	}
	headed := "0"
	if mode == "headed" {
		headed = "1"
	}
	config.Env["AGENT_BROWSER_CONFIG"] = configPath
	config.Env["AGENT_BROWSER_SESSION"] = session
	config.Env["AGENT_BROWSER_SOCKET_DIR"] = socket
	config.Env["AGENT_BROWSER_HEADED"] = headed
	return &config, nil
}

func (s *server) closeComputers() {
	s.computersMu.Lock()
	drivers := s.computers
	s.computers = map[string]*computerDriver{}
	s.computersMu.Unlock()
	for _, driver := range drivers {
		driver.session.close()
	}
}

func journalPath(dir string, taskID int64) string {
	return filepath.Join(dir, "task-"+strconv.FormatInt(taskID, 10)+".jsonl")
}

// appendJournal writes one entry and syncs it before the caller continues.
func appendJournal(dir string, taskID int64, entry journalEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(journalPath(dir, taskID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// readJournal returns the last entry of each call. A torn final line from a
// crash is ignored; its call has no result and stays uncertain via the marker.
func readJournal(dir string, taskID int64) (map[string]journalEntry, error) {
	entries := map[string]journalEntry{}
	data, err := os.ReadFile(journalPath(dir, taskID))
	if errors.Is(err, os.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	// Lines are not bounded by the frame limit: a result too large to send is
	// still a recorded outcome.
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		var entry journalEntry
		if json.Unmarshal(line, &entry) == nil && entry.CallID != "" {
			entries[entry.CallID] = entry
		}
	}
	return entries, nil
}

// ComputerCall is one journaled computer action of a Bot task.
type ComputerCall struct {
	Dir    string
	TaskID int64
	// CallID is the engine's tool call ID, stable across recovery.
	CallID string
	Action computer.Action
}

func (c *Client) computerCall(ctx context.Context, input ComputerInput) (ComputerResult, error) {
	if driver := c.options.ComputerDrivers[input.Service]; driver != nil {
		input.Driver = driver
	}
	raw, _, err := c.call(ctx, "computer", input, nil, nil)
	var result ComputerResult
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

// Act runs one native action. Mutations are journaled on the work computer
// before dispatch; a call ID already dispatched returns its saved result or
// COMPUTER_UNCERTAIN, and is never sent to the driver again.
func (c *Client) Act(ctx context.Context, call ComputerCall) (computer.Observation, error) {
	result, err := c.computerCall(ctx, ComputerInput{
		Action: "call", Service: call.Action.Service, Dir: call.Dir, TaskID: call.TaskID, CallID: call.CallID,
		Name: call.Action.Name, Arguments: call.Action.Arguments,
	})
	if err != nil {
		return computer.Observation{}, err
	}
	return toObservation(result.Result)
}

// VerifyComputer records that the caller observed the desktop after an
// uncertain action and allows further actions. The browser needs
// RestartBrowser instead.
func (c *Client) VerifyComputer(ctx context.Context, dir string, taskID int64, service string) error {
	_, err := c.computerCall(ctx, ComputerInput{Action: "verify", Service: service, Dir: dir, TaskID: taskID})
	return err
}

// RestartBrowser closes the Bot browser and reconnects it with the same
// profile, the only recovery from an unknown browser outcome. Tabs and unsaved
// page input are lost.
func (c *Client) RestartBrowser(ctx context.Context, dir string, taskID int64) error {
	_, err := c.computerCall(ctx, ComputerInput{Action: "restart", Service: "browser", Dir: dir, TaskID: taskID})
	return err
}

// ComputerCatalog lists the driver's native tools with their schemas.
func (c *Client) ComputerCatalog(ctx context.Context, dir, service string) ([]*mcp.Tool, error) {
	result, err := c.computerCall(ctx, ComputerInput{Action: "catalog", Service: service, Dir: dir})
	return result.Tools, err
}

// toObservation keeps the native result and its first image unchanged.
func toObservation(raw json.RawMessage) (computer.Observation, error) {
	observation := computer.Observation{Raw: raw}
	var result struct {
		Content []struct {
			Type     string `json:"type"`
			Data     []byte `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return observation, fmt.Errorf("worker: native result: %w", err)
	}
	for _, part := range result.Content {
		if part.Type != "image" {
			continue
		}
		shot := &computer.Screenshot{MimeType: part.MimeType, Data: part.Data}
		if config, _, err := image.DecodeConfig(bytes.NewReader(part.Data)); err == nil {
			shot.Width, shot.Height = config.Width, config.Height
		}
		observation.Screenshot = shot
		break
	}
	return observation, nil
}
