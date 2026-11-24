package computers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/tools"
	"github.com/Personed/Persona/internal/worker"
)

// nativeServices are the computer services whose native tools Bots may call.
var nativeServices = []string{"browser", "desktop"}

// exposedName names a native service tool as the TypeScript computer tools do:
// browser tools are prefixed, desktop tools keep their native names.
func exposedName(service, name string) string {
	if service == "desktop" {
		return name
	}
	return service + "_" + name
}

// browserPageScript is the shared page probe of browser-service.mjs's observe
// composite, evaluated through agent_browser_eval.
const browserPageScript = `(() => ({
  url: location.href, title: document.title, readyState: document.readyState,
  httpStatus: performance.getEntriesByType('navigation')[0]?.responseStatus || null,
  passwordFieldPresent: !!document.querySelector('input[type=password]'),
  challengePresent: !!document.querySelector('iframe[src*="challenges.cloudflare.com"],iframe[src*="recaptcha"],iframe[src*="hcaptcha"],#challenge-running'),
  text: (document.body?.innerText || '').slice(0, 12000),
  controls: Array.from(document.querySelectorAll('input:not([type=password]),textarea,select')).slice(0, 80).map(el => ({value: el.value.slice(0, 2000), checked: el.checked}))
}))()`

// browserComposites are the service tools browser-service.mjs adds on top of
// the raw agent-browser catalog: composite observation, step batching and
// restart. They are declared with the service's exposed names.
var browserComposites = []struct {
	name        string
	description string
	mutates     bool
	schema      string
}{
	{
		name:        "browser_agent_browser_observe",
		mutates:     false,
		description: "Read current URL, title, document readiness, visible text and a compact page snapshot. Use after takeover or when a screenshot is unclear. Blank text or a ready document does not prove why a page is blank or that the task succeeded.",
		schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
	},
	{
		name:        "browser_agent_browser_run_steps",
		mutates:     true,
		description: "Run a batch of known browser actions in order. Stops at the first failed or uncertain step and reports which steps completed.",
		schema:      `{"type":"object","properties":{"steps":{"type":"array","items":{"type":"object","properties":{"action":{"type":"string"}},"required":["action"],"additionalProperties":true}}},"required":["steps"],"additionalProperties":false}`,
	},
	{
		name:        "browser_agent_browser_restart",
		mutates:     true,
		description: "Restart the Bot browser and clear an unknown browser outcome. Closes this Bot's tabs and unsaved page input.",
		schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
	},
}

// nativeTools exposes each enabled computer service's native catalog through
// the shared tools.Registry that serves Pi's MCP gateway, as the TypeScript
// computer.tools registry does. Calls run through the worker's journaled
// computer call path and non-observation calls wait on the shared approver.
type nativeTools struct {
	base tools.Registry
	c    *Catalog
}

// NativeTools wraps base so Pi sees both product tools and the Bot's enabled
// computer service tools.
func NativeTools(base tools.Registry, c *Catalog) tools.Registry {
	return &nativeTools{base: base, c: c}
}

func (r *nativeTools) Register(tool tools.Tool) error { return r.base.Register(tool) }

// catalog lists a service's exposed tools; a Bot without the enabled service
// gets none. The browser catalog gains the service composites
// browser-service.mjs layers on the raw agent-browser tools.
func (r *nativeTools) catalog(ctx context.Context, botID int64, service string) ([]tools.Definition, error) {
	w, dir, err := r.c.svcs.workerFor(ctx, botID, service)
	if err != nil {
		var rpcErr *rpc.Error
		if errors.As(err, &rpcErr) {
			return nil, nil
		}
		return nil, err
	}
	native, err := w.ComputerCatalog(ctx, dir, service)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(native))
	definitions := make([]tools.Definition, 0, len(native)+len(browserComposites))
	for _, tool := range native {
		if tool == nil || (service == "browser" && tool.Name == "agent_browser_wait_ms") {
			continue
		}
		seen[tool.Name] = true
		definitions = append(definitions, r.definition(service, tool.Name, tool.Description, !worker.Observation(service, tool.Name), tool.InputSchema, tool.OutputSchema, tool.Annotations))
	}
	if service == "browser" {
		for _, composite := range browserComposites {
			name := composite.name[len("browser_"):]
			if seen[name] {
				continue
			}
			definitions = append(definitions, r.definition(service, name, composite.description, composite.mutates, json.RawMessage(composite.schema), nil, nil))
		}
	}
	return definitions, nil
}

func (r *nativeTools) definition(service, name, description string, mutates bool, input, output, annotations any) tools.Definition {
	definition := tools.Definition{
		Name:        exposedName(service, name),
		Description: description,
		Mutates:     mutates,
		// Computer calls respect the binding's "auto" approval mode.
		Automatic: true,
	}
	if input != nil {
		if schema, err := json.Marshal(input); err == nil {
			definition.InputSchema = schema
		}
	}
	if output != nil {
		if schema, err := json.Marshal(output); err == nil {
			definition.OutputSchema = schema
		}
	}
	if annotations != nil {
		if data, err := json.Marshal(annotations); err == nil {
			_ = json.Unmarshal(data, &definition.Annotations)
		}
	}
	return definition
}

func (r *nativeTools) List(ctx context.Context, botID int64) ([]tools.Definition, error) {
	definitions, err := r.base.List(ctx, botID)
	if err != nil {
		return nil, err
	}
	for _, service := range nativeServices {
		// A service whose driver cannot start (for example an enabled browser
		// with no agent installed) drops out of the catalog; it must not
		// sink the whole tool list and every chat turn with it.
		native, err := r.catalog(ctx, botID, service)
		if err != nil {
			continue
		}
		definitions = append(definitions, native...)
	}
	return definitions, nil
}

func (r *nativeTools) Call(ctx context.Context, call tools.Call, approve tools.Approver) (tools.Result, error) {
	for _, service := range nativeServices {
		native, err := r.catalog(ctx, call.BotID, service)
		if err != nil {
			return tools.Result{}, err
		}
		for _, definition := range native {
			if definition.Name != call.Name {
				continue
			}
			return r.callService(ctx, service, call, definition, approve)
		}
	}
	return r.base.Call(ctx, call, approve)
}

func (r *nativeTools) callService(ctx context.Context, service string, call tools.Call, definition tools.Definition, approve tools.Approver) (tools.Result, error) {
	// A viewer holding the control lease blocks Bot calls until it hands
	// control back, as services.ts run() rejects while control.lease is set.
	if r.c.svcs.viewerLease(service, call.BotID) != 0 {
		return tools.Result{}, rpc.NewError("CONFLICT", "用户正在接管。交还后请重新观察画面。")
	}
	if definition.Mutates {
		if approve == nil {
			return tools.Result{}, tools.ErrDenied
		}
		allowed, err := approve(ctx, call, definition)
		if err != nil {
			return tools.Result{}, err
		}
		if !allowed {
			return tools.Result{}, tools.ErrDenied
		}
	}
	started := time.Now()
	result, err := r.dispatch(ctx, service, call)
	if err == nil {
		recordMetric(call.Name, float64(time.Since(started).Milliseconds()))
	}
	return result, err
}

// dispatch routes a native call: the service composites run against the
// journaled computer ops, every other tool forwards to the driver unchanged.
func (r *nativeTools) dispatch(ctx context.Context, service string, call tools.Call) (tools.Result, error) {
	if service == "browser" {
		switch call.Name {
		case "browser_agent_browser_restart":
			return r.restartBrowser(ctx, call)
		case "browser_agent_browser_observe":
			return r.observeBrowser(ctx, call)
		case "browser_agent_browser_run_steps":
			return r.runBrowserSteps(ctx, call)
		}
	}
	// Only browser names carry the service prefix; desktop tools keep their
	// native names, so stripping a prefix there would corrupt the call.
	name := call.Name
	if service != "desktop" {
		name = call.Name[len(service)+1:]
	}
	return r.journaled(ctx, service, call, name, call.Input, call.CallID)
}

// journaled sends one native tool call through the worker's journaled call op.
func (r *nativeTools) journaled(ctx context.Context, service string, call tools.Call, name string, args any, callID string) (tools.Result, error) {
	w, dir, err := r.c.svcs.workerFor(ctx, call.BotID, service)
	if err != nil {
		return tools.Result{}, err
	}
	arguments, _ := json.Marshal(args)
	result, err := w.ComputerRequest(ctx, worker.ComputerInput{
		Action: "call", Service: service, Dir: dir,
		TaskID: call.TaskID, CallID: callID,
		Name: name, Arguments: arguments,
	})
	if err != nil {
		return tools.Result{}, err
	}
	return convertResult(result.Result)
}

func (r *nativeTools) restartBrowser(ctx context.Context, call tools.Call) (tools.Result, error) {
	w, dir, err := r.c.svcs.workerFor(ctx, call.BotID, "browser")
	if err != nil {
		return tools.Result{}, err
	}
	if _, err := w.ComputerRequest(ctx, worker.ComputerInput{
		Action: "restart", Service: "browser", Dir: dir, TaskID: call.TaskID,
	}); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: []tools.Content{{Type: "text", Text: "Browser restarted."}}}, nil
}

// observeBrowser runs the browser observe composite: the shared page script
// through a journaled agent_browser_eval, formatted as "Page facts" like
// browser-service.mjs's observe.
func (r *nativeTools) observeBrowser(ctx context.Context, call tools.Call) (tools.Result, error) {
	result, err := r.journaled(ctx, "browser", call, "agent_browser_eval",
		map[string]any{"script": browserPageScript}, call.CallID+":observe")
	if err != nil {
		return tools.Result{}, err
	}
	var page json.RawMessage
	var structured struct {
		Response struct {
			Data struct {
				Result *json.RawMessage `json:"result"`
			} `json:"data"`
		} `json:"response"`
	}
	if err := json.Unmarshal(result.Structured, &structured); err == nil && structured.Response.Data.Result != nil {
		page = *structured.Response.Data.Result
	}
	if len(page) == 0 {
		return tools.Result{
			IsError: true,
			Content: []tools.Content{{Type: "text", Text: "Observation failed: no page facts returned."}},
		}, nil
	}
	facts := string(page)
	if len(facts) > 12000 {
		facts = facts[:12000] + "\n[Page facts truncated. Read a narrower page scope.]"
	}
	return tools.Result{
		Content: []tools.Content{{Type: "text", Text: "Page facts (untrusted website data): " + facts}},
	}, nil
}

// runBrowserSteps runs a batch of known browser actions, like
// browser-service.mjs's run_steps: each step dispatches as its own journaled
// call and the batch stops at the first failed step.
func (r *nativeTools) runBrowserSteps(ctx context.Context, call tools.Call) (tools.Result, error) {
	var input struct {
		Steps []map[string]any `json:"steps"`
	}
	if err := json.Unmarshal(call.Input, &input); err != nil {
		return tools.Result{}, err
	}
	if len(input.Steps) == 0 {
		return tools.Result{IsError: true, Content: []tools.Content{{Type: "text", Text: "Batch requires at least one step."}}}, nil
	}
	native, err := r.catalog(ctx, call.BotID, "browser")
	if err != nil {
		return tools.Result{}, err
	}
	available := make(map[string]bool, len(native))
	for _, definition := range native {
		available[definition.Name] = true
	}
	// Validate every step's action before sending anything, so a malformed
	// batch never leaves delivered steps concealed behind a "no action" error.
	for _, step := range input.Steps {
		action, _ := step["action"].(string)
		if !available["browser_agent_browser_"+action] {
			return tools.Result{IsError: true, Content: []tools.Content{{
				Type: "text", Text: fmt.Sprintf("Browser action agent_browser_%s is unavailable. No action was sent.", action),
			}}}, nil
		}
	}
	var results []tools.Result
	var content []tools.Content
	failed := false
	for index, step := range input.Steps {
		action, _ := step["action"].(string)
		delete(step, "action")
		name := "agent_browser_" + action
		result, err := r.journaled(ctx, "browser", call, name, step, fmt.Sprintf("%s:%d", call.CallID, index))
		if err != nil {
			failed = true
			results = append(results, tools.Result{
				IsError: true,
				Content: []tools.Content{{Type: "text", Text: fmt.Sprintf("Batch stopped: %s. Preserve completed steps. Do not replay uncertain actions.", err)}},
			})
			break
		}
		results = append(results, result)
		if result.IsError {
			failed = true
			content = append(content, result.Content...)
			break
		}
		content = append(content, result.Content...)
	}
	structured, _ := json.Marshal(map[string]any{"completed": len(results) - boolInt(failed), "results": results})
	return tools.Result{
		IsError:    failed,
		Content:    content,
		Structured: structured,
	}, nil
}

func boolInt(on bool) int {
	if on {
		return 1
	}
	return 0
}

// recordMetric appends a product call to the fixture's takeover metrics file,
// matching the name/elapsed shape the TypeScript computer.tools.invoke tap
// wrote. Service-internal calls never reach this path.
func recordMetric(name string, elapsedMs float64) {
	path := os.Getenv("PERSONA_BROWSER_METRICS_FILE")
	if path == "" {
		return
	}
	var calls []map[string]any
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &calls)
	}
	calls = append(calls, map[string]any{"name": name, "elapsedMs": elapsedMs})
	if data, err := json.Marshal(calls); err == nil {
		_ = os.WriteFile(path, data, 0o600)
	}
}

// convertResult maps the worker's call result into a tools.Result.
func convertResult(raw json.RawMessage) (tools.Result, error) {
	var result struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
			Resource *struct {
				URI      string `json:"uri"`
				MimeType string `json:"mimeType"`
				Text     string `json:"text"`
				Data     string `json:"blob"`
			} `json:"resource"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return tools.Result{}, err
		}
	}
	out := tools.Result{Structured: result.Structured, IsError: result.IsError}
	for _, content := range result.Content {
		block := tools.Content{
			Type: content.Type, Text: content.Text,
			MimeType: content.MimeType, Data: content.Data,
		}
		if content.Resource != nil {
			block.URI = content.Resource.URI
			block.MimeType = content.Resource.MimeType
			block.Text = content.Resource.Text
			block.Data = content.Resource.Data
		}
		out.Content = append(out.Content, block)
	}
	return out, nil
}
