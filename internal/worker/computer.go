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
	Action    string          `json:"action"` // catalog, call, verify, restart, prepare, install, mode, settle, view
	Service   string          `json:"service"`
	Dir       string          `json:"dir"`
	TaskID    int64           `json:"taskId,omitempty"`
	CallID    string          `json:"callId,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// View sub-operations of the "view" action: open, next, send, close.
	View        string          `json:"view,omitempty"`
	ViewID      string          `json:"viewId,omitempty"`
	Message     json.RawMessage `json:"message,omitempty"`
	Mode        string          `json:"mode,omitempty"`
	CursorColor string          `json:"cursorColor,omitempty"`
	// Driver replaces the default `cua-driver mcp` or `agent-browser mcp`.
	Driver *MCPConfig `json:"driver,omitempty"`
}

// ComputerResult answers a ComputerInput. Result is the native CallToolResult,
// or the ComputerStatus that prepare, install and mode report.
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

// Observation reports whether a native service tool only observes; every other
// call routes through the shared approver, as computer.tools.invoke does.
func Observation(service, name string) bool { return observation(service, name) }

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
	case "view":
		return s.view(ctx, input, key, marker)
	case "prepare", "install", "mode", "settle":
		return s.computerStatus(ctx, input, key, journal, marker)
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
	if s.browserRestarts == nil {
		s.browserRestarts = map[string]uint64{}
	}
	s.browserRestarts[key]++
	s.computersMu.Unlock()
	if old != nil {
		old.session.close()
	}
	if err := closeBrowser(ctx, config); err != nil {
		return nil, err
	}
	if _, err := s.computerDriver(ctx, key, input); err != nil {
		return nil, mcpError(ctx, err)
	}
	// The restart completes only once the new browser takes the viewport.
	if err := prepareBrowser(ctx, config); err != nil {
		return nil, codeError("EIO", "the browser restarted but did not prepare: "+err.Error(), "")
	}
	if err := appendJournal(journal, input.TaskID, journalEntry{State: "restarted", Service: input.Service}); err != nil {
		return nil, err
	}
	if err := os.Remove(marker); err != nil {
		return nil, err
	}
	return &ComputerResult{}, nil
}

// closeBrowser runs `agent-browser close` for the Bot session and confirms
// that its daemon, which may still run earlier commands, exited before the
// client reconnects. It stops a daemon that outlives the close.
func closeBrowser(ctx context.Context, config *MCPConfig) error {
	pid := 0
	data, err := os.ReadFile(filepath.Join(config.Env["AGENT_BROWSER_SOCKET_DIR"], config.Env["AGENT_BROWSER_SESSION"]+".pid"))
	if err == nil {
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid < 1 || pid == os.Getpid() {
			return codeError("EIO", "the browser daemon's process is not confirmed", "")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	closing, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, _ = browserCLI(closing, config, "close")
	cancel()
	if pid == 0 {
		return nil
	}
	if exited, err := waitExit(ctx, pid, time.Second); exited || err != nil {
		return err
	}
	if err := stopBrowserDaemon(ctx, config, pid); err != nil {
		return err
	}
	if exited, err := waitExit(ctx, pid, 5*time.Second); exited || err != nil {
		return err
	}
	return codeError("EIO", "the browser daemon did not exit", "")
}

func waitExit(ctx context.Context, pid int, wait time.Duration) (bool, error) {
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); {
		if !processAlive(pid) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return !processAlive(pid), nil
}

// stopBrowserDaemon force-stops the daemon's process tree only when pid is
// still agent-browser run by the worker's user: a crashed daemon leaves its
// pid file, and the system may have given that pid to another program.
func stopBrowserDaemon(ctx context.Context, config *MCPConfig, pid int) error {
	executable, owned, found, err := processIdentity(ctx, pid)
	if err != nil || !found {
		return err
	}
	launcher, err := ResolveProgram(ctx, config.Command, stdioEnvironment(config.Env))
	if err != nil {
		return err
	}
	if !owned || executable == "" || launcher == "" || !browserDaemonExecutable(executable, launcher) {
		return codeError("EIO", "the browser daemon's process identity is not confirmed; it was not stopped", "")
	}
	return killTree(pid)
}

// browserDaemonExecutable reports whether executable runs the browser
// daemon that launcher starts: the launcher itself, or the native binary the
// npm package's script launcher runs from its own bin directory.
func browserDaemonExecutable(executable, launcher string) bool {
	if samePath(executable, launcher) {
		return true
	}
	launcher, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		return false
	}
	for _, name := range browserNativeNames() {
		if samePath(executable, filepath.Join(filepath.Dir(launcher), name)) {
			return true
		}
	}
	return false
}

// browserNativeNames are agent-browser's published native binary names for
// this OS. The npm launcher picks one by Node.js's architecture, which can
// differ from the worker's (x64 Node.js under Rosetta, for one).
func browserNativeNames() []string {
	var names []string
	for _, arch := range []string{"x64", "arm64"} {
		switch runtime.GOOS {
		case "windows":
			names = append(names, "agent-browser-win32-"+arch+".exe")
		case "linux":
			names = append(names, "agent-browser-linux-"+arch, "agent-browser-linux-musl-"+arch)
		default:
			names = append(names, "agent-browser-"+runtime.GOOS+"-"+arch)
		}
	}
	return names
}

func samePath(a, b string) bool {
	a, errA := filepath.EvalSymlinks(a)
	b, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// browserCLI runs an agent-browser command for the Bot session with the
// driver's environment and returns its output.
func browserCLI(ctx context.Context, config *MCPConfig, args ...string) ([]byte, error) {
	env := stdioEnvironment(config.Env)
	path, err := ResolveProgram(ctx, config.Command, env)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, codeError("ENOENT", config.Command+" is not installed for this user", config.Command)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return stdout.Bytes(), errors.New(message)
	}
	return stdout.Bytes(), nil
}

// ComputerStatus reports whether a computer service can be used, as prepare,
// install and mode do in browser-service.mjs and computer-service.mjs.
type ComputerStatus struct {
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	// Mode is the browser's "headless" or "headed" mode.
	Mode string `json:"mode,omitempty"`
	// SharedDesktop reports whether the headed browser shares desktop control;
	// headless reports false, which marks the browser's own control lane
	// (browser-service.mjs sharedDesktop).
	SharedDesktop bool `json:"sharedDesktop"`
}

func browserMode(dir string) string {
	mode := "headless"
	if data, err := os.ReadFile(filepath.Join(dir, ".persona", "browser-mode.json")); err == nil {
		_ = json.Unmarshal(data, &mode)
	}
	return mode
}

// computerStatus serves prepare, install, mode and settle. Each waits for the
// service's dispatched actions to settle first.
func (s *server) computerStatus(ctx context.Context, input ComputerInput, key, journal, marker string) (*ComputerResult, error) {
	if (input.Action == "install" || input.Action == "mode") && input.Service != "browser" {
		return nil, codeError("EINVAL", input.Action+" is a browser operation", "")
	}
	lane := s.computerLane(key)
	lane.Lock()
	defer lane.Unlock()
	_, statErr := os.Stat(marker)
	uncertainNow := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if input.Action == "settle" {
		if uncertainNow {
			return nil, uncertain(input.Service)
		}
		return &ComputerResult{}, nil
	}
	if input.Service == "desktop" {
		return statusResult(s.prepareDesktop(ctx, key, input)), nil
	}
	status := func(err error) *ComputerResult {
		mode := browserMode(input.Dir)
		result := &ComputerStatus{Available: err == nil, Mode: mode, SharedDesktop: mode == "headed"}
		if err != nil {
			result.Error = err.Error()
		}
		return statusResult(result)
	}
	config, err := driverConfig(input)
	if err != nil {
		return nil, err
	}
	switch input.Action {
	case "install":
		_, err := browserCLI(ctx, config, "install", "--use-system-ca")
		if err != nil {
			return statusResult(&ComputerStatus{Error: err.Error()}), nil
		}
		return statusResult(&ComputerStatus{Available: true}), nil
	case "mode":
		if input.Mode != "headless" && input.Mode != "headed" {
			return nil, codeError("EINVAL", "browser mode must be headless or headed", "")
		}
		if input.Mode == browserMode(input.Dir) && !uncertainNow {
			return status(prepareBrowser(ctx, config)), nil
		}
		// A mode change is a confirmed restart: tabs and unsaved input close.
		mode, _ := json.Marshal(input.Mode)
		if err := os.WriteFile(filepath.Join(input.Dir, ".persona", "browser-mode.json"), mode, 0o600); err != nil {
			return nil, err
		}
		_, err := s.restartBrowser(ctx, input, journal, marker)
		return status(err), nil
	}
	if uncertainNow {
		return status(nil), nil
	}
	return status(prepareBrowser(ctx, config)), nil
}

// prepareBrowser sets the Bot browser viewport, retrying while the browser
// starts and reports an unknown outcome, as browser-service.mjs does.
func prepareBrowser(ctx context.Context, config *MCPConfig) error {
	for tries := 0; ; tries++ {
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := browserCLI(attempt, config, "set", "viewport", "1280", "720", "--json")
		cancel()
		if err == nil || tries >= 2 || !strings.Contains(err.Error(), "outcome_unknown") {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// prepareDesktop checks that the graphical session provides a screenshot.
func statusResult(status *ComputerStatus) *ComputerResult {
	data, _ := json.Marshal(status)
	return &ComputerResult{Result: data}
}

func (s *server) prepareDesktop(ctx context.Context, key string, input ComputerInput) *ComputerStatus {
	driver, err := s.computerDriver(ctx, key, input)
	if err != nil {
		return &ComputerStatus{Error: mcpError(ctx, err).Error()}
	}
	result, err := driver.session.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_desktop_state", Arguments: map[string]any{"max_image_dimension": 320},
	})
	if err != nil {
		s.dropBroken(ctx, key, driver, err)
		return &ComputerStatus{Error: mcpError(ctx, err).Error()}
	}
	for _, part := range result.Content {
		if _, ok := part.(*mcp.ImageContent); ok && !result.IsError {
			return &ComputerStatus{Available: true}
		}
	}
	message := "The graphical session cannot provide a screen. Sign in to the work computer and check screen recording permission."
	for _, part := range result.Content {
		if text, ok := part.(*mcp.TextContent); ok && text.Text != "" {
			message = text.Text
			break
		}
	}
	return &ComputerStatus{Error: message}
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
	// PERSONA_COMPUTER_DRIVER_<SERVICE> holds a JSON MCPConfig replacing the
	// default driver, which test fixtures use to serve a scripted service.
	if input.Driver == nil {
		if override := os.Getenv("PERSONA_COMPUTER_DRIVER_" + strings.ToUpper(input.Service)); override != "" {
			var parsed MCPConfig
			if err := json.Unmarshal([]byte(override), &parsed); err != nil {
				return nil, fmt.Errorf("worker: PERSONA_COMPUTER_DRIVER_%s: %w", strings.ToUpper(input.Service), err)
			}
			input.Driver = &parsed
		}
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
	mode := browserMode(input.Dir)
	settings, _ := json.Marshal(map[string]any{
		"profile": filepath.Join(state, "browser"), "headed": mode == "headed", "downloadPath": downloads,
		"contentBoundaries": true, "maxOutput": 32000, "useSystemCa": true,
	})
	configPath := filepath.Join(state, "browser.json")
	if err := os.WriteFile(configPath, settings, 0o600); err != nil {
		return nil, err
	}
	socket := browserSocketDir(input.Dir)
	if err := privateSocketDir(socket); err != nil {
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

// browserSocketDir is the agent-browser socket directory of a work directory.
func browserSocketDir(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	// macOS per-user temp paths can exceed the Unix socket path limit.
	base := os.TempDir()
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	return filepath.Join(base, "persona-browser-"+hex.EncodeToString(sum[:])[:24])
}

// privateSocketDir creates the socket directory, or accepts an existing one
// only when it is a real directory that this user owns with mode 0700. The
// path is predictable in a shared temp directory, so another local user can
// create it first.
func privateSocketDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || !ownedPrivate(info, os.Getuid()) {
		return codeError("EACCES", "the browser socket directory is not a private directory of this user", path)
	}
	return nil
}

func (s *server) closeComputers() {
	s.closeViews()
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

// ComputerRequest sends a ComputerRequest operation (service-types.ts):
// settle, catalog, prepare, install, mode, view and call. The view
// sub-operations ride in View ("open", "next", "send", "close") with the
// viewer id and message.
func (c *Client) ComputerRequest(ctx context.Context, input ComputerInput) (ComputerResult, error) {
	return c.computerCall(ctx, input)
}

func (c *Client) computerStatus(ctx context.Context, input ComputerInput) (ComputerStatus, error) {
	result, err := c.computerCall(ctx, input)
	if err != nil {
		return ComputerStatus{}, err
	}
	var status ComputerStatus
	err = json.Unmarshal(result.Result, &status)
	return status, err
}

// PrepareComputer reports whether service can be used: the desktop provides
// a screenshot, or the Bot browser starts with its viewport.
func (c *Client) PrepareComputer(ctx context.Context, dir, service string) (ComputerStatus, error) {
	return c.computerStatus(ctx, ComputerInput{Action: "prepare", Service: service, Dir: dir})
}

// InstallBrowser installs the browser agent-browser drives.
func (c *Client) InstallBrowser(ctx context.Context, dir string) (ComputerStatus, error) {
	return c.computerStatus(ctx, ComputerInput{Action: "install", Service: "browser", Dir: dir})
}

// SetBrowserMode switches the Bot browser to "headless" or "headed". A change,
// or an unknown browser outcome, restarts the browser with the same profile.
func (c *Client) SetBrowserMode(ctx context.Context, dir, mode string) (ComputerStatus, error) {
	return c.computerStatus(ctx, ComputerInput{Action: "mode", Service: "browser", Dir: dir, Mode: mode})
}

// SettleComputer waits for service's dispatched actions to settle. It fails
// with COMPUTER_UNCERTAIN when an outcome is unknown.
func (c *Client) SettleComputer(ctx context.Context, dir, service string) error {
	_, err := c.computerCall(ctx, ComputerInput{Action: "settle", Service: service, Dir: dir})
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
