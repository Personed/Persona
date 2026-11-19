package worker

import (
	"bufio"
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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/computer"
)

// ComputerInput is one call to the work computer's desktop (cua-driver) or
// browser (agent-browser) driver. Dir is the Bot work directory; its .persona
// folder holds the browser profile and the computer-use journal.
type ComputerInput struct {
	Action    string          `json:"action"` // catalog, call, verify
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
// dispatch, then result or uncertain. verify clears a service's uncertainty.
type journalEntry struct {
	State     string          `json:"state"`
	CallID    string          `json:"callId,omitempty"`
	Service   string          `json:"service"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

type computerDriver struct {
	mu      sync.Mutex // serializes mutations so each settles before the next
	session *mcpSession
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
	if input.Action == "verify" {
		if err := appendJournal(journal, input.TaskID, journalEntry{State: "verified", Service: input.Service}); err != nil {
			return nil, err
		}
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return &ComputerResult{}, nil
	}
	driver, err := s.computerDriver(ctx, input)
	if err != nil {
		return nil, mcpError(ctx, err)
	}
	switch input.Action {
	case "catalog":
		var tools []*mcp.Tool
		for tool, err := range driver.session.session.Tools(ctx, nil) {
			if err != nil {
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
		result, err := driver.session.session.CallTool(ctx, params)
		if err != nil {
			return nil, mcpError(ctx, err)
		}
		raw, err := json.Marshal(result)
		return &ComputerResult{Result: raw}, err
	}
	if input.CallID == "" {
		return nil, codeError("EINVAL", "a call ID is required for computer actions", "")
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	entries, err := readJournal(journal, input.TaskID)
	if err != nil {
		return nil, err
	}
	if previous, ok := entries[input.CallID]; ok {
		if previous.State == "result" {
			return &ComputerResult{Result: previous.Result, Replayed: true}, nil
		}
		return nil, errComputerUncertain
	}
	if _, err := os.Stat(marker); err == nil {
		return nil, errComputerUncertain
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
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
	result, callErr := driver.session.session.CallTool(ctx, params)
	var wire *jsonrpc.Error
	if callErr != nil && !errors.As(callErr, &wire) || callErr == nil && uncertainResult(result) {
		entry := journalEntry{State: "uncertain", CallID: input.CallID, Service: input.Service}
		if callErr == nil {
			entry.Result, _ = json.Marshal(result)
		}
		if err := appendJournal(journal, input.TaskID, entry); err != nil {
			return nil, err
		}
		if callErr != nil {
			return nil, errComputerUncertain
		}
		return &ComputerResult{Result: entry.Result}, nil
	}
	// A protocol error means the driver rejected the request without acting.
	var raw json.RawMessage
	if callErr == nil {
		raw, _ = json.Marshal(result)
	} else {
		raw, _ = json.Marshal(map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": wire.Message}}})
	}
	if err := appendJournal(journal, input.TaskID, journalEntry{State: "result", CallID: input.CallID, Service: input.Service, Result: raw}); err != nil {
		return nil, err
	}
	if err := os.Remove(marker); err != nil {
		return nil, err
	}
	if callErr != nil {
		return nil, mcpError(ctx, callErr)
	}
	return &ComputerResult{Result: raw}, nil
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

func (s *server) computerDriver(ctx context.Context, input ComputerInput) (*computerDriver, error) {
	key := input.Service + "\n" + input.Dir
	s.computersMu.Lock()
	defer s.computersMu.Unlock()
	if driver := s.computers[key]; driver != nil {
		return driver, nil
	}
	config, err := driverConfig(input)
	if err != nil {
		return nil, err
	}
	session, err := openMCP(ctx, MCPInput{Config: config}, nil)
	if err != nil {
		return nil, err
	}
	driver := &computerDriver{session: session}
	s.computers[key] = driver
	return driver, nil
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
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), MaxFrame)
	for scanner.Scan() {
		var entry journalEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.CallID != "" {
			entries[entry.CallID] = entry
		}
	}
	return entries, scanner.Err()
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

// VerifyComputer records that the caller observed the computer after an
// uncertain action and allows further actions on service.
func (c *Client) VerifyComputer(ctx context.Context, dir string, taskID int64, service string) error {
	_, err := c.computerCall(ctx, ComputerInput{Action: "verify", Service: service, Dir: dir, TaskID: taskID})
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
