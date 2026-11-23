package computers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/worker"
)

var randRead = rand.Read

// AssistanceView is computer.assistant's output, assistance.ts state().
type AssistanceView struct {
	ComputerID *string         `json:"computerId"`
	Revision   int64           `json:"revision"`
	Enabled    bool            `json:"enabled"`
	Provider   string          `json:"provider"`
	Configured map[string]bool `json:"configured"`
	Available  bool            `json:"available"`
}

// Assistant is computer.assistant: the Bot's Jev assistance state.
func (c *Catalog) Assistant(ctx context.Context, botID int64) (AssistanceView, error) {
	doc, _, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		return AssistanceView{}, err
	}
	binding := doc.Bindings[strconv.FormatInt(botID, 10)]
	view := AssistanceView{
		ComputerID: binding.ComputerID,
		Revision:   binding.Revision,
		Provider:   "typesafe",
		Configured: map[string]bool{
			"typesafe":   c.assistKey(ctx, "typesafe") != "",
			"openrouter": c.assistKey(ctx, "openrouter") != "",
		},
		Available: c.keyring != nil,
	}
	if binding.Assistance != nil {
		view.Enabled = binding.Assistance.Enabled
		view.Provider = binding.Assistance.Provider
	}
	return view, nil
}

// AssistantConfigure is computer.assistantConfigure.
func (c *Catalog) AssistantConfigure(ctx context.Context, botID int64, computerID string, revision int64, enabled bool, provider string) (AssistanceView, error) {
	key := strconv.FormatInt(botID, 10)
	if err := c.h.Commit(ctx, func(tx *agent.Tx) error {
		doc, err := agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		binding, ok := doc.Bindings[key]
		if err := requireRevision(binding.Revision, revision); err != nil {
			return err
		}
		if !ok || binding.ComputerID == nil || *binding.ComputerID != computerID {
			return rpc.NewError("CONFLICT", "工作电脑已改变。请刷新后重试。")
		}
		if enabled && c.assistKey(ctx, provider) == "" {
			return rpc.NewError("BAD_REQUEST", "请先保存这个来源的密钥。")
		}
		binding.Assistance = &Assistance{Enabled: enabled, Provider: provider}
		doc.Bindings[key] = binding
		return nil
	}); err != nil {
		return AssistanceView{}, err
	}
	return c.Assistant(ctx, botID)
}

// AssistantTestResult is computer.assistantTest's output.
type AssistantTestResult struct {
	Model        string `json:"model"`
	Milliseconds int64  `json:"milliseconds"`
}

// Setup is computer.setup: the preparation state of preparation.ts.
func (c *Catalog) Setup(context.Context) (map[string]any, error) {
	return map[string]any{
		"deployment": c.cfg.Deployment,
		"canIsolate": c.cfg.Template != nil,
	}, nil
}

// Prepare is computer.prepare: shared or dedicated isolation for the Bot.
func (c *Catalog) Prepare(ctx context.Context, botID int64, mode string, revision int64, requestID string) (BindingView, error) {
	return c.configureMode(ctx, botID, mode, revision, requestID)
}

// ConfigureMode is computer.configure: process or isolated work computer.
func (c *Catalog) ConfigureMode(ctx context.Context, botID int64, mode string, revision int64, requestID string) (BindingView, error) {
	return c.configureMode(ctx, botID, mode, revision, requestID)
}

func (c *Catalog) configureMode(ctx context.Context, botID int64, mode string, revision int64, requestID string) (BindingView, error) {
	c.pendingMu.Lock()
	if op := c.pending[botID]; op != nil {
		c.pendingMu.Unlock()
		if op.requestID != requestID {
			return BindingView{}, rpc.NewError("CONFLICT", "正在为这个 Bot 准备电脑。")
		}
		select {
		case <-op.done:
			return op.view, op.err
		case <-ctx.Done():
			return BindingView{}, ctx.Err()
		}
	}
	op := &pendingOp{requestID: requestID, done: make(chan struct{})}
	c.pending[botID] = op
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, botID)
		c.pendingMu.Unlock()
		close(op.done)
	}()
	binding, err := c.Binding(ctx, botID)
	if err != nil {
		op.err = err
		return BindingView{}, err
	}
	if binding.RequestID == requestID {
		return binding, nil
	}
	if err := requireRevision(binding.Revision, revision); err != nil {
		op.err = err
		return BindingView{}, err
	}
	if err := requireIdle(ctx, c, botID); err != nil {
		op.err = err
		return BindingView{}, err
	}
	item, err := c.record(ctx, botID, mode)
	if err != nil {
		op.err = err
		return BindingView{}, err
	}
	if !item.Authorized {
		if _, err := c.Authorize(ctx, item.ID, true); err != nil {
			op.err = err
			return BindingView{}, err
		}
	}
	if err := c.ensureReady(ctx, item.ID); err != nil {
		op.err = err
		return BindingView{}, err
	}
	view, err := c.Assign(ctx, botID, AssignInput{
		ComputerID: &item.ID, Revision: revision, RequestID: requestID,
	})
	op.view, op.err = view, err
	return view, err
}

type pendingOp struct {
	requestID string
	done      chan struct{}
	view      BindingView
	err       error
}

// record finds or creates the Bot's target for the mode, preparation.ts record.
func (c *Catalog) record(ctx context.Context, botID int64, mode string) (Record, error) {
	template := c.cfg.Template
	if mode != "process" && template == nil {
		return Record{}, rpc.NewError("BAD_REQUEST", "这台服务尚未配置隔离电脑。请联系管理员。")
	}
	var item Record
	if err := c.h.Commit(ctx, func(tx *agent.Tx) error {
		doc, err := agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		for _, existing := range doc.Items {
			match := mode == "process" && existing.Config.Kind == "process" ||
				mode != "process" && existing.Config.Kind != "process" &&
					(mode == "shared" && (existing.Scope != nil && existing.Scope.Kind == "shared" || existing.ID == template.ID) ||
						mode == "dedicated" && existing.Scope != nil && existing.Scope.Kind == "dedicated" && existing.Scope.BotID == botID)
			if match {
				item = existing
				return nil
			}
		}
		if template == nil {
			return rpc.NewError("NOT_FOUND", "未找到服务环境。")
		}
		id := mode + "-" + randomID()[:12]
		config := *template
		config.ID = id
		if config.Kind != "controller" {
			config.Volume = "persona-" + id + "-work"
		}
		name := "共用隔离电脑"
		if mode == "dedicated" {
			name = "独立隔离电脑"
		}
		scope := &Scope{Kind: "shared"}
		if mode == "dedicated" {
			scope = &Scope{Kind: "dedicated", BotID: botID}
		}
		item = Record{ID: id, Name: name, Config: config, Authorized: true, Scope: scope}
		doc.Items = append(doc.Items, item)
		return nil
	}); err != nil {
		return Record{}, err
	}
	c.emit()
	return item, nil
}

var randomID = func() string {
	b := make([]byte, 32)
	_, _ = randRead(b)
	const digits = "0123456789abcdef"
	out := make([]byte, 36)
	j := 0
	for i := 0; i < 36; i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			out[i] = '-'
			continue
		}
		out[i] = digits[b[j]&0xf]
		j++
	}
	return string(out)
}

// ConfigureService toggles a bound service (computer.service). The worker-side
// enable/install lands with the services layer; this stores the toggle and the
// prepare/install intent, as tools.ts configure does.
func (c *Catalog) ConfigureService(ctx context.Context, botID int64, service string, enabled bool, _ bool) (ServiceStates, error) {
	if service != "browser" && service != "desktop" {
		return ServiceStates{}, rpc.NewError("BAD_REQUEST", "服务无效。")
	}
	if err := c.ServiceSettings(ctx, botID, service, enabled); err != nil {
		return ServiceStates{}, err
	}
	c.emit()
	return c.Services(ctx, botID)
}

// ServiceState is one service's state, ServiceState of service-types.ts
// plus the enabled flag the workspace reads (tools.ts state).
type ServiceState struct {
	Enabled   bool    `json:"enabled"`
	Available bool    `json:"available"`
	Error     *string `json:"error"`
	Mode      string  `json:"mode,omitempty"`
}

// ServiceStates is computer.services' output: browser and desktop states.
type ServiceStates struct {
	Browser ServiceState `json:"browser"`
	Desktop ServiceState `json:"desktop"`
}

// Services reports the Bot's bound services (computer.services): the worker's
// prepare answer where reachable, the configured state otherwise.
func (c *Catalog) Services(ctx context.Context, botID int64) (ServiceStates, error) {
	doc, _, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		return ServiceStates{}, err
	}
	binding := doc.Bindings[strconv.FormatInt(botID, 10)]
	enabled := binding.Services
	if enabled == nil {
		enabled = map[string]bool{}
	}
	if binding.ComputerID == nil {
		return ServiceStates{}, nil
	}
	states := ServiceStates{
		Browser: ServiceState{Enabled: enabled["browser"]},
		Desktop: ServiceState{Enabled: enabled["desktop"]},
	}
	for _, service := range []string{"browser", "desktop"} {
		if !enabled[service] {
			continue
		}
		w, dir, err := c.svcs.workerFor(ctx, botID, service)
		if err != nil {
			text := err.Error()
			if service == "browser" {
				states.Browser.Error = &text
			} else {
				states.Desktop.Error = &text
			}
			continue
		}
		result, err := w.ComputerRequest(ctx, worker.ComputerInput{Action: "prepare", Service: service, Dir: dir})
		var prepared struct {
			Available     bool    `json:"available"`
			Error         *string `json:"error"`
			Mode          string  `json:"mode"`
			SharedDesktop bool    `json:"sharedDesktop"`
		}
		if err == nil && len(result.Result) > 0 {
			err = json.Unmarshal(result.Result, &prepared)
		}
		if err != nil {
			// The worker has no prepare op yet: report the configured state.
			continue
		}
		state := ServiceState{Enabled: enabled[service], Available: prepared.Available, Error: prepared.Error, Mode: prepared.Mode}
		if service == "browser" {
			states.Browser = state
		} else {
			states.Desktop = state
		}
	}
	return states, nil
}

// BrowserMode switches headless/headed (computer.browserMode). Only a
// confirmed restart may change it; the worker's mode op owns the restart.
// Browser and desktop control are held during the change.
func (c *Catalog) BrowserMode(ctx context.Context, botID int64, mode string) (ServiceStates, error) {
	if mode != "headless" && mode != "headed" {
		return ServiceStates{}, rpc.NewError("BAD_REQUEST", "浏览器模式无效。")
	}
	w, dir, err := c.svcs.workerFor(ctx, botID, "browser")
	if err != nil {
		return ServiceStates{}, err
	}
	result, err := w.ComputerRequest(ctx, worker.ComputerInput{Action: "mode", Service: "browser", Dir: dir, Mode: mode})
	if err != nil {
		return ServiceStates{}, err
	}
	var changed struct {
		Available bool    `json:"available"`
		Error     *string `json:"error"`
		Mode      string  `json:"mode"`
	}
	if len(result.Result) > 0 {
		if err := json.Unmarshal(result.Result, &changed); err != nil {
			return ServiceStates{}, err
		}
	}
	if !changed.Available {
		message := "浏览器模式未确认。"
		if changed.Error != nil {
			message = *changed.Error
		}
		return ServiceStates{}, rpc.NewError("SERVICE_UNAVAILABLE", message)
	}
	return c.Services(ctx, botID)
}

// Watch streams a service viewer (computer.watch): opens the worker's view
// session, then pumps frames until the stream or the viewer ends.
func (c *Catalog) Watch(ctx context.Context, botID int64, service string, emit func(any) error) error {
	if service != "browser" && service != "desktop" {
		return rpc.NewError("BAD_REQUEST", "服务无效。")
	}
	session, first, err := c.vw.open(ctx, botID, service, "")
	if err != nil {
		return err
	}
	defer c.vw.close(context.Background(), session.id)
	if err := emit(frame{Type: "opened", ViewID: session.id, Message: first}); err != nil {
		return err
	}
	for {
		batch, err := c.vw.next(ctx, session.id)
		if err != nil {
			return err
		}
		if batch.Closed {
			return emit(frame{Type: "closed", ViewID: session.id})
		}
		for _, message := range batch.Messages {
			if err := emit(frame{Type: "message", ViewID: session.id, Message: message}); err != nil {
				return err
			}
		}
	}
}

// Input sends a viewer action (computer.input), views.ts action().
func (c *Catalog) Input(ctx context.Context, botID int64, view string, action json.RawMessage) (any, error) {
	session := c.vw.get(view)
	if session == nil {
		return nil, rpc.NewError("NOT_FOUND", "viewer fechado.")
	}
	if session.botID != botID {
		return nil, rpc.NewError("FORBIDDEN", "viewer 不属于该 Bot。")
	}
	if err := c.vw.action(ctx, view, action); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// AssistantKey stores the provider key (computer.assistantKey).
func (c *Catalog) AssistantKey(ctx context.Context, botID int64, provider, key string) (AssistanceView, error) {
	if c.keyring == nil {
		return AssistanceView{}, errors.New("当前服务未配置凭据存储。")
	}
	if err := c.saveKey(ctx, provider, key); err != nil {
		return AssistanceView{}, err
	}
	return c.Assistant(ctx, botID)
}

// AssistantTest checks provider connectivity (computer.assistantTest).
func (c *Catalog) AssistantTest(ctx context.Context, _ int64, provider string) (AssistantTestResult, error) {
	key := c.assistKey(ctx, provider)
	if key == "" {
		return AssistantTestResult{}, errors.New("请先保存这个来源的密钥。")
	}
	return askJevTest(ctx, provider, key)
}
