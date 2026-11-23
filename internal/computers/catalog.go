package computers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"unicode/utf16"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/store"
)

// Catalog is the Computers class of catalog.ts: the work-computer directory,
// per-Bot bindings, and lifecycle of its targets.
type Catalog struct {
	h     *agent.Harness
	store store.Store
	cfg   ServiceConfig

	mu        sync.Mutex
	targets   map[string]*Target
	changing  map[string]bool
	transfers map[int64]int
	starting  map[string]*startOp

	watchMu sync.Mutex
	watches []chan struct{}

	pendingMu sync.Mutex
	pending   map[int64]*pendingOp
	keyring   *Keyring
}

type startOp struct {
	done chan struct{}
	err  error
}

// ServiceConfig is the service-side environment the catalog binds to.
type ServiceConfig struct {
	DataDir    string
	Deployment string // "native" or "container"
	// Computer is the local service's own target configuration.
	Computer Config
	// Template provisions shared/dedicated isolation targets (preparation).
	Template *Config
	// Legacy marks an upgrade from the TypeScript service: the local computer
	// binds computer.json and authorized bots.
	Legacy bool
}

// New returns a catalog over the agent harness.
func New(cfg ServiceConfig, h *agent.Harness) *Catalog {
	return &Catalog{
		h: h, store: h.Store(), cfg: cfg,
		targets: map[string]*Target{}, changing: map[string]bool{},
		transfers: map[int64]int{}, starting: map[string]*startOp{},
		pending: map[int64]*pendingOp{}, keyring: NewKeyring(cfg.DataDir),
	}
}

// Initialize seeds the catalog document on first boot, as Computers.initialize.
func (c *Catalog) Initialize(ctx context.Context, botIDs []int64) error {
	if _, ok, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0); err != nil || ok {
		return err
	}
	configured := c.cfg.Computer
	id := configured.ID
	if configured.Kind == "process" {
		id = "process"
	}
	return c.h.Commit(ctx, func(tx *agent.Tx) error {
		doc, err := agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		if c.cfg.Legacy {
			doc.AttachmentComputerID = &id
			if err := newTarget(c.cfg, Record{Config: configured}).bind(); err != nil {
				return err
			}
		}
		doc.Items = append(doc.Items, Record{
			ID: "process", Name: "本机",
			Config:     Config{Kind: "process"},
			Authorized: c.cfg.Legacy && id == "process",
		})
		if id != "process" {
			doc.Items = append(doc.Items, Record{
				ID: id, Name: "工作电脑", Config: configured, Authorized: true,
			})
		}
		for _, bot := range botIDs {
			if c.cfg.Legacy {
				if err := c.attach(tx, bot, &id); err != nil {
					return err
				}
			} else {
				if err := c.attach(tx, bot, nil); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// attach assigns a Bot, Attach of catalog.ts. computerID nil means the default.
func (c *Catalog) attach(tx *agent.Tx, botID int64, computerID *string) error {
	doc, err := agent.Edit(tx, ComputersDoc, 0)
	if err != nil {
		return err
	}
	id := computerID
	if id == nil {
		id = doc.DefaultID
	}
	var item *Record
	if id != nil {
		item = requireComputer(doc.Items, *id)
	}
	if id != nil && c.changing[*id] {
		return rpc.NewError("CONFLICT", "工作电脑正在操作中。")
	}
	if item != nil && !item.Authorized {
		return rpc.NewError("BAD_REQUEST", "请先授权这台电脑。")
	}
	if item != nil && item.Scope != nil && item.Scope.Kind == "dedicated" && item.Scope.BotID != botID {
		return rpc.NewError("BAD_REQUEST", "这台独立电脑属于另一个 Bot。")
	}
	key := strconv.FormatInt(botID, 10)
	doc.Bindings[key] = Binding{ComputerID: id, Revision: 0, RequestID: ""}
	return nil
}

func requireComputer(items []Record, id string) *Record {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

func requireIdle(ctx context.Context, c *Catalog, botID int64) error {
	live, _, err := agent.Snapshot(ctx, c.h, agent.LiveDoc, botID)
	if err != nil {
		return err
	}
	if len(live.Run) != 0 {
		return rpc.NewError("CONFLICT", "请先停止 Bot 的工作，再切换或操作电脑。")
	}
	for _, status := range []store.SubmissionStatus{store.SubmissionQueued, store.SubmissionPlaced} {
		pending, err := c.store.Submissions(ctx, store.SubmissionQuery{
			ConversationID: store.ConversationID(botID), Status: status,
		}, store.Scan{Limit: 1})
		if err != nil {
			return err
		}
		if len(pending.Items) != 0 {
			return rpc.NewError("CONFLICT", "请先停止 Bot 的工作，再切换或操作电脑。")
		}
	}
	return nil
}

func requireRevision(actual, expected int64) error {
	if actual != expected {
		return rpc.NewError("CONFLICT", "内容已更新，请刷新后重试。")
	}
	return nil
}

func requiredText(value string, maximum int) (string, error) {
	if length := len(utf16.Encode([]rune(value))); length == 0 || length > maximum {
		return "", rpc.NewError("BAD_REQUEST", fmt.Sprintf("内容长度须为1至%d个字符。", maximum))
	}
	return value, nil
}

// View is the wire shape of one catalog item, ComputerView of catalog.ts.
type View struct {
	Status
	Name         string       `json:"name"`
	Authorized   bool         `json:"authorized"`
	Capabilities capabilities `json:"capabilities"`
	Scope        *Scope       `json:"scope,omitempty"`
	Busy         bool         `json:"busy"`
}

type capabilities struct {
	Commands bool  `json:"commands"`
	Files    bool  `json:"files"`
	Desktop  bool  `json:"desktop"`
}

func (c *Catalog) view(ctx context.Context, item Record) (View, error) {
	status, err := c.target(item).status(ctx)
	if err != nil {
		return View{}, err
	}
	available := item.Authorized && status.State == "running" && !c.changing[item.ID]
	name := item.Name
	if item.Config.Kind == "process" {
		if c.cfg.Deployment == "container" {
			name = "服务所在容器"
		} else {
			name = "服务所在电脑"
		}
	}
	return View{
		Status:     status,
		Name:       name,
		Authorized: item.Authorized,
		Scope:      item.Scope,
		Busy:       c.changing[item.ID],
		Capabilities: capabilities{
			Commands: available, Files: available, Desktop: false,
		},
	}, nil
}

// List is the result of computer.list, list() of catalog.ts.
type List struct {
	Items     []View             `json:"items"`
	DefaultID *string            `json:"defaultId"`
	Bindings  map[string]Binding `json:"bindings"`
}

// List returns the catalog view for computer.list.
func (c *Catalog) List(ctx context.Context) (List, error) {
	doc, ok, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		return List{}, err
	}
	if !ok || len(doc.Items) == 0 {
		return List{}, rpc.NewError("SERVICE_UNAVAILABLE", "电脑接入尚未配置。")
	}
	out := List{DefaultID: doc.DefaultID, Bindings: doc.Bindings}
	if out.Bindings == nil {
		out.Bindings = map[string]Binding{}
	}
	for _, item := range doc.Items {
		v, err := c.view(ctx, item)
		if err != nil {
			return List{}, err
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

// BindingView is a Bot's binding with its computer's view.
type BindingView struct {
	ComputerID *string `json:"computerId"`
	Revision   int64   `json:"revision"`
	RequestID  string  `json:"requestId"`
	ApprovalMode string `json:"approvalMode,omitempty"`
	Services     map[string]bool `json:"services,omitempty"`
	Assistance   *Assistance     `json:"assistance,omitempty"`
	Computer     *View           `json:"computer"`
}

// Binding returns the Bot's assignment for computer.get.
func (c *Catalog) Binding(ctx context.Context, botID int64) (BindingView, error) {
	doc, _, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		return BindingView{}, err
	}
	b, ok := doc.Bindings[strconv.FormatInt(botID, 10)]
	out := BindingView{Revision: 0, RequestID: ""}
	if ok {
		out.ComputerID = b.ComputerID
		out.Revision = b.Revision
		out.RequestID = b.RequestID
		out.ApprovalMode = b.ApprovalMode
		out.Services = b.Services
		out.Assistance = b.Assistance
	}
	if b.ComputerID != nil {
		item := requireComputer(doc.Items, *b.ComputerID)
		if item == nil {
			return BindingView{}, rpc.NewError("NOT_FOUND", "电脑不存在。")
		}
		v, err := c.view(ctx, *item)
		if err != nil {
			return BindingView{}, err
		}
		out.Computer = &v
	}
	return out, nil
}

// AssignInput is computer.assign's payload.
type AssignInput struct {
	ComputerID *string `json:"computerId"`
	Revision   int64   `json:"revision"`
	RequestID  string  `json:"requestId"`
}

// Assign sets a Bot's binding; a repeated requestId answers the current binding.
func (c *Catalog) Assign(ctx context.Context, botID int64, input AssignInput) (BindingView, error) {
	key := strconv.FormatInt(botID, 10)
	if err := c.h.Commit(ctx, func(tx *agent.Tx) error {
		doc, err := agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		previous, ok := doc.Bindings[key]
		if ok && previous.RequestID == input.RequestID {
			return nil
		}
		var revision int64
		if ok {
			revision = previous.Revision
		}
		if err := requireRevision(revision, input.Revision); err != nil {
			return err
		}
		if c.transfers[botID] > 0 {
			return rpc.NewError("CONFLICT", "请等待文件传输或消息提交完成。")
		}
		if err := requireIdle(ctx, c, botID); err != nil {
			return err
		}
		if err := c.attach(tx, botID, input.ComputerID); err != nil {
			return err
		}
		doc, err = agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		doc.Bindings[key] = Binding{
			ComputerID: input.ComputerID, Revision: input.Revision + 1, RequestID: input.RequestID,
		}
		return nil
	}); err != nil {
		return BindingView{}, err
	}
	c.emit()
	return c.Binding(ctx, botID)
}

// Approval sets a binding's approval mode (computer.approval).
func (c *Catalog) Approval(ctx context.Context, botID int64, computerID string, revision int64, mode string) (List, error) {
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
		item := requireComputer(doc.Items, computerID)
		if item == nil {
			return rpc.NewError("NOT_FOUND", "电脑不存在。")
		}
		if !item.Authorized {
			return rpc.NewError("BAD_REQUEST", "请先授权这台电脑。")
		}
		binding.ApprovalMode = mode
		doc.Bindings[key] = binding
		return nil
	}); err != nil {
		return List{}, err
	}
	c.emit()
	return c.List(ctx)
}

// AddInput is computer.add's payload.
type AddInput struct {
	Name   string `json:"name"`
	Config Config `json:"config"`
}

// Add registers a docker or controller computer (computer.add).
func (c *Catalog) Add(ctx context.Context, input AddInput) (List, error) {
	if input.Config.Kind == "process" {
		return List{}, rpc.NewError("BAD_REQUEST", "本机已在电脑目录中。")
	}
	name, err := requiredText(input.Name, 80)
	if err != nil {
		return List{}, err
	}
	item := Record{ID: input.Config.ID, Name: name, Config: input.Config, Authorized: true}
	if err := c.h.Commit(ctx, func(tx *agent.Tx) error {
		doc, err := agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		previous := requireComputer(doc.Items, item.ID)
		if previous != nil {
			a, _ := json.Marshal(*previous)
			b, _ := json.Marshal(item)
			if string(a) == string(b) {
				return nil
			}
			return rpc.NewError("CONFLICT", "电脑标识已存在。")
		}
		doc.Items = append(doc.Items, item)
		return nil
	}); err != nil {
		return List{}, err
	}
	c.emit()
	return c.List(ctx)
}

// Authorize grants or revokes a computer (computer.authorize).
func (c *Catalog) Authorize(ctx context.Context, id string, authorized bool) (List, error) {
	if c.changing[id] {
		return List{}, rpc.NewError("CONFLICT", "工作电脑正在操作中。")
	}
	if err := c.h.Commit(ctx, func(tx *agent.Tx) error {
		doc, err := agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		index := -1
		for i := range doc.Items {
			if doc.Items[i].ID == id {
				index = i
			}
		}
		if index < 0 {
			return rpc.NewError("NOT_FOUND", "电脑不存在。")
		}
		var bound []string
		for botID, binding := range doc.Bindings {
			if binding.ComputerID != nil && *binding.ComputerID == id {
				bound = append(bound, botID)
			}
		}
		for _, botID := range bound {
			n, _ := strconv.ParseInt(botID, 10, 64)
			if c.transfers[n] > 0 {
				return rpc.NewError("CONFLICT", "请等待文件传输或消息提交完成。")
			}
			if err := requireIdle(ctx, c, n); err != nil {
				return err
			}
		}
		doc.Items[index].Authorized = authorized
		if !authorized {
			for _, botID := range bound {
				binding := doc.Bindings[botID]
				binding.ApprovalMode = ""
				binding.Assistance = nil
				binding.Revision++
				doc.Bindings[botID] = binding
			}
			if doc.DefaultID != nil && *doc.DefaultID == id {
				doc.DefaultID = nil
			}
		}
		return nil
	}); err != nil {
		return List{}, err
	}
	c.mu.Lock()
	target := c.targets[id]
	c.mu.Unlock()
	if target != nil {
		_ = target.close(ctx)
	}
	c.emit()
	return c.List(ctx)
}

// SetDefault sets the shared default computer (computer.default).
func (c *Catalog) SetDefault(ctx context.Context, id *string) (List, error) {
	if err := c.h.Commit(ctx, func(tx *agent.Tx) error {
		doc, err := agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		if id != nil {
			item := requireComputer(doc.Items, *id)
			if item == nil {
				return rpc.NewError("NOT_FOUND", "电脑不存在。")
			}
			if !item.Authorized {
				return rpc.NewError("BAD_REQUEST", "请先授权这台电脑。")
			}
			if item.Scope != nil && item.Scope.Kind == "dedicated" {
				return rpc.NewError("BAD_REQUEST", "独立电脑不能设为共享电脑。")
			}
		}
		doc.DefaultID = id
		return nil
	}); err != nil {
		return List{}, err
	}
	c.emit()
	return c.List(ctx)
}

// Manage starts, stops or rebuilds a computer (computer.manage).
func (c *Catalog) Manage(ctx context.Context, id, action string) (View, error) {
	doc, _, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		return View{}, err
	}
	item := requireComputer(doc.Items, id)
	if item == nil {
		return View{}, rpc.NewError("NOT_FOUND", "电脑不存在。")
	}
	if !item.Authorized {
		return View{}, rpc.NewError("BAD_REQUEST", "请先授权这台电脑。")
	}
	c.mu.Lock()
	if c.changing[id] {
		c.mu.Unlock()
		return View{}, rpc.NewError("CONFLICT", "工作电脑正在操作中。")
	}
	c.changing[id] = true
	c.mu.Unlock()
	c.emit()
	defer func() {
		c.mu.Lock()
		delete(c.changing, id)
		c.mu.Unlock()
		c.emit()
	}()
	requireIdleBound := func(ctx context.Context) error {
		return c.h.Commit(ctx, func(tx *agent.Tx) error {
			doc, err := agent.Edit(tx, ComputersDoc, 0)
			if err != nil {
				return err
			}
			for botID, binding := range doc.Bindings {
				if binding.ComputerID == nil || *binding.ComputerID != id {
					continue
				}
				n, _ := strconv.ParseInt(botID, 10, 64)
				if c.transfers[n] > 0 {
					return rpc.NewError("CONFLICT", "请等待文件传输或消息提交完成。")
				}
				if err := requireIdle(ctx, c, n); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := c.target(*item).manage(ctx, action, requireIdleBound); err != nil {
		return View{}, err
	}
	return c.view(ctx, *item)
}

// ServiceSettings stores a binding's service toggle (computer.service).
func (c *Catalog) ServiceSettings(ctx context.Context, botID int64, service string, enabled bool) error {
	key := strconv.FormatInt(botID, 10)
	return c.h.Commit(ctx, func(tx *agent.Tx) error {
		doc, err := agent.Edit(tx, ComputersDoc, 0)
		if err != nil {
			return err
		}
		binding, ok := doc.Bindings[key]
		if !ok || binding.ComputerID == nil {
			return rpc.NewError("BAD_REQUEST", "请先选择工作电脑。")
		}
		if binding.Services == nil {
			binding.Services = map[string]bool{}
		}
		binding.Services[service] = enabled
		doc.Bindings[key] = binding
		return nil
	})
}

// AttachmentComputerID reports which computer file attachments use.
func (c *Catalog) AttachmentComputerID(ctx context.Context) (*string, error) {
	doc, _, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		return nil, err
	}
	return doc.AttachmentComputerID, nil
}

// ComputerID returns the Bot's bound computer, or nil.
func (c *Catalog) ComputerID(ctx context.Context, botID int64) (*string, error) {
	doc, _, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		return nil, err
	}
	return doc.Bindings[strconv.FormatInt(botID, 10)].ComputerID, nil
}

// ensureReady starts a scoped computer when it is not running.
func (c *Catalog) ensureReady(ctx context.Context, id string) error {
	c.mu.Lock()
	if op := c.starting[id]; op != nil {
		c.mu.Unlock()
		select {
		case <-op.done:
			return op.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	op := &startOp{done: make(chan struct{})}
	c.starting[id] = op
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.starting, id)
		c.mu.Unlock()
		close(op.done)
	}()
	doc, _, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		op.err = err
		return err
	}
	item := requireComputer(doc.Items, id)
	if item == nil {
		op.err = rpc.NewError("NOT_FOUND", "电脑不存在。")
		return op.err
	}
	if !item.Authorized {
		op.err = rpc.NewError("BAD_REQUEST", "这台电脑的授权已撤销。")
		return op.err
	}
	status, err := c.target(*item).status(ctx)
	if err != nil {
		op.err = err
		return err
	}
	if status.State != "running" {
		if _, err := c.Manage(ctx, id, "start"); err != nil {
			op.err = err
			return err
		}
	}
	return nil
}

func (c *Catalog) target(item Record) *Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.targets[item.ID]
	if t == nil {
		t = newTarget(c.cfg, item)
		c.targets[item.ID] = t
	}
	return t
}

// emit wakes catalog watchers (change events of catalog.ts).
func (c *Catalog) emit() {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	for _, ch := range c.watches {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Close stops every live target (close of catalog.ts).
func (c *Catalog) Close(ctx context.Context) error {
	c.mu.Lock()
	targets := make([]*Target, 0, len(c.targets))
	for _, t := range c.targets {
		targets = append(targets, t)
	}
	c.mu.Unlock()
	var errs []error
	for _, t := range targets {
		errs = append(errs, t.close(ctx))
	}
	return errors.Join(errs...)
}

// hostArch reports the local target's OS/arch labels for the process computer.
func hostLabels() (string, string) {
	return runtime.GOOS, runtime.GOARCH
}
