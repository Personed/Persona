package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Personed/Persona/internal/engine"
)

// RequestTimeout bounds one setup or control command, like the TypeScript handshake.
const RequestTimeout = 30 * time.Second

// ErrBusy reports a Start for a Bot whose Pi already serves another open session.
var ErrBusy = errors.New("pi: bot already has an open session")

// Config describes how Persona runs Pi on the work computer.
type Config struct {
	// Command is the pi program; empty means "pi" from PATH.
	Command string
	// Args are extra flags for every Pi process, such as --extension.
	Args []string
	// SessionDir holds Pi's native session files.
	SessionDir string
	// StateDir holds per-Bot launch files such as the appended system prompt.
	StateDir string
	// Env is the base environment; nil means the current process environment.
	Env []string
	// IdleTimeout closes a Bot's Pi after its last session closes. Zero means IdleTimeout.
	IdleTimeout time.Duration
	// RequestTimeout bounds control commands. Zero means RequestTimeout.
	RequestTimeout time.Duration
	// StopGrace is the wait between shutdown steps. Zero means StopGrace.
	StopGrace time.Duration
}

// Engine implements engine.Engine with one Pi process per active Bot.
type Engine struct {
	config Config
	pool   *Pool[*conn]
}

var _ engine.Engine = (*Engine)(nil)

type launchKey struct{}

type launch struct {
	options engine.SessionOptions
	key     string
}

// New returns an Engine. Call Close to stop every Pi process.
func New(config Config) *Engine {
	if config.Command == "" {
		config.Command = "pi"
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = IdleTimeout
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = RequestTimeout
	}
	if config.StopGrace == 0 {
		config.StopGrace = StopGrace
	}
	e := &Engine{config: config}
	e.pool = NewPool(config.IdleTimeout, e.open)
	return e
}

// Live returns how many Bots currently have a Pi process.
func (e *Engine) Live() int { return e.pool.Live() }

// Close stops every Pi process and waits for them to exit.
func (e *Engine) Close() { e.pool.Close() }

// Info reports the installed Pi version.
func (e *Engine) Info(ctx context.Context) (engine.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, e.config.RequestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.config.Command, "--version")
	cmd.Env = e.config.Env
	out, err := cmd.Output()
	if err != nil {
		return engine.Info{}, fmt.Errorf("pi: --version: %w", err)
	}
	return engine.Info{
		ID:           "pi",
		Name:         "Pi",
		Version:      strings.TrimSpace(string(out)),
		Capabilities: map[string]bool{"approvals": true, "models": true, "recovery": true},
	}, nil
}

// Models lists the models Pi can use with its current credentials. It runs a
// short-lived Pi without a session, so no process remains afterwards.
func (e *Engine) Models(ctx context.Context) ([]engine.Model, error) {
	catalog, err := e.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]engine.Model, len(catalog))
	for i, m := range catalog {
		models[i] = engine.Model{Provider: m.Provider, ModelID: m.ID}
	}
	return models, nil
}

// CatalogModel is one entry of Pi's get_available_models.
type CatalogModel struct {
	Provider       string   `json:"provider"`
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Input          []string `json:"input"`
	Reasoning      bool     `json:"reasoning"`
	ContextWindow  int64    `json:"contextWindow"`
	ThinkingLevels []string `json:"thinkingLevels,omitempty"`
}

// Catalog returns Pi's full model definitions from get_available_models.
func (e *Engine) Catalog(ctx context.Context) ([]CatalogModel, error) {
	var result struct {
		Models []CatalogModel `json:"models"`
	}
	err := e.transient(ctx, nil, func(ctx context.Context, c *Client) error {
		data, err := c.Request(ctx, "get_available_models", nil)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, &result)
	})
	return result.Models, err
}

// transient runs one Pi without a session for a short exchange and stops it.
func (e *Engine) transient(ctx context.Context, env []string, run func(context.Context, *Client) error) error {
	ctx, cancel := e.bound(ctx)
	defer cancel()
	p, err := Start(Command{
		Path: e.config.Command,
		Args: append([]string{"--mode", "rpc", "--no-session"}, e.config.Args...),
		Env:  mergeEnv(e.config.Env, env),
	})
	if err != nil {
		return fmt.Errorf("pi: start: %w", err)
	}
	defer func() { _ = p.Stop(e.config.StopGrace) }()
	client := NewClient(p.Stdout(), p.Stdin())
	go func() {
		for range client.Events() {
		}
	}()
	return run(ctx, client)
}

func (e *Engine) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, e.config.RequestTimeout)
}

// Start opens or resumes a native session. The Bot's Pi process is reused when
// its launch settings match; otherwise it is replaced.
func (e *Engine) Start(ctx context.Context, options engine.SessionOptions) (engine.Session, error) {
	l := &launch{options: options, key: launchKeyOf(options)}
	ctx = context.WithValue(ctx, launchKey{}, l)
	for attempt := 0; ; attempt++ {
		c, release, err := e.pool.Acquire(ctx, options.BotID)
		if err != nil {
			return nil, err
		}
		if c.launch != l.key && attempt == 0 {
			busy := c.busy()
			release()
			if busy {
				return nil, ErrBusy
			}
			e.pool.Stop(options.BotID)
			continue
		}
		s := &session{engine: e, conn: c, botID: options.BotID, release: release, events: make(chan engine.Event), closed: make(chan struct{})}
		if !c.claim(s) {
			release()
			return nil, ErrBusy
		}
		if err := s.setup(ctx, options); err != nil {
			s.fail()
			return nil, err
		}
		return s, nil
	}
}

func (e *Engine) open(ctx context.Context, botID int64) (*conn, error) {
	l, ok := ctx.Value(launchKey{}).(*launch)
	if !ok {
		return nil, errors.New("pi: missing launch settings")
	}
	args := []string{"--mode", "rpc"}
	if e.config.SessionDir != "" {
		args = append(args, "--session-dir", e.config.SessionDir)
	}
	if prompt := l.options.SystemPrompt; prompt != "" {
		// A file keeps long instructions off the command line.
		dir := filepath.Join(e.config.StateDir, "bots", strconv.FormatInt(botID, 10))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "instructions.md")
		if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--append-system-prompt", path)
	}
	env := make([]string, 0, len(l.options.Env))
	for k, v := range l.options.Env {
		env = append(env, k+"="+v)
	}
	p, err := Start(Command{
		Path: e.config.Command,
		Args: append(args, e.config.Args...),
		Dir:  l.options.WorkDir,
		Env:  mergeEnv(e.config.Env, env),
	})
	if err != nil {
		return nil, fmt.Errorf("pi: start: %w", err)
	}
	c := &conn{proc: p, client: NewClient(p.Stdout(), p.Stdin()), launch: l.key, grace: e.config.StopGrace}
	go c.pump()
	return c, nil
}

func launchKeyOf(o engine.SessionOptions) string {
	h := sha256.New()
	env := make([]string, 0, len(o.Env))
	for k, v := range o.Env {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)
	for _, part := range append([]string{o.WorkDir, o.SystemPrompt}, env...) {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// mergeEnv overlays KEY=value pairs on base, or on the current environment when base is nil.
func mergeEnv(base, overlay []string) []string {
	if base == nil {
		base = os.Environ()
	}
	if len(overlay) == 0 {
		return base
	}
	out := slices.Clone(base)
	for _, kv := range overlay {
		key, _, _ := strings.Cut(kv, "=")
		out = slices.DeleteFunc(out, func(existing string) bool {
			k, _, _ := strings.Cut(existing, "=")
			return k == key
		})
		out = append(out, kv)
	}
	return out
}

// conn is one Bot's Pi process. At most one session owns it at a time; frames
// that arrive while nobody owns it are dropped.
type conn struct {
	proc   *Process
	client *Client
	launch string
	grace  time.Duration

	mu          sync.Mutex
	owner       *session
	sessionFile string
}

func (c *conn) Close() error          { return c.proc.Stop(c.grace) }
func (c *conn) Done() <-chan struct{} { return c.proc.Done() }

func (c *conn) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owner != nil
}

func (c *conn) claim(s *session) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner != nil {
		return false
	}
	c.owner = s
	return true
}

func (c *conn) unclaim(s *session) {
	c.mu.Lock()
	if c.owner == s {
		c.owner = nil
	}
	c.mu.Unlock()
}

func (c *conn) pump() {
	for frame := range c.client.Events() {
		c.mu.Lock()
		s := c.owner
		c.mu.Unlock()
		if s != nil {
			s.deliver(engine.Event{Type: frame.Type, Data: frame.Raw})
		}
	}
	// A broken protocol stream leaves Pi unusable; stop it so the pool replaces it.
	_ = c.proc.Stop(c.grace)
	c.mu.Lock()
	s := c.owner
	c.mu.Unlock()
	if s != nil {
		s.close()
	}
}

type session struct {
	engine  *Engine
	conn    *conn
	botID   int64
	release func()
	id      string

	events    chan engine.Event
	eventsMu  sync.RWMutex
	closeOnce sync.Once
	closed    chan struct{}
}

var _ engine.Session = (*session)(nil)

func (s *session) setup(ctx context.Context, o engine.SessionOptions) error {
	c := s.conn
	c.mu.Lock()
	current := c.sessionFile
	c.mu.Unlock()
	switch {
	case o.SessionID != "" && o.SessionID != current:
		var switched struct {
			Cancelled bool `json:"cancelled"`
		}
		if err := s.call(ctx, "switch_session", map[string]string{"sessionPath": o.SessionID}, &switched); err != nil {
			return err
		}
		if switched.Cancelled {
			return errors.New("pi: session switch cancelled")
		}
	case o.SessionID == "" && current != "":
		if err := s.call(ctx, "new_session", nil, nil); err != nil {
			return err
		}
	}
	if err := s.call(ctx, "set_steering_mode", map[string]string{"mode": "all"}, nil); err != nil {
		return err
	}
	if o.Model.Provider != "" {
		if err := s.SetModel(ctx, o.Model, o.ThinkingLevel); err != nil {
			return err
		}
	} else if o.ThinkingLevel != "" {
		if err := s.call(ctx, "set_thinking_level", map[string]string{"level": o.ThinkingLevel}, nil); err != nil {
			return err
		}
	}
	state, err := s.State(ctx)
	if err != nil {
		return err
	}
	if state.SessionID == "" {
		return errors.New("pi: no session file")
	}
	s.id = state.SessionID
	c.mu.Lock()
	c.sessionFile = s.id
	c.mu.Unlock()
	return nil
}

// fail ends a session whose setup failed and replaces the Pi process, whose
// session state is then unknown.
func (s *session) fail() {
	s.close()
	s.engine.pool.Stop(s.botID)
}

func (s *session) call(ctx context.Context, command string, params, out any) error {
	ctx, cancel := s.engine.bound(ctx)
	defer cancel()
	return s.request(ctx, command, params, out)
}

func (s *session) request(ctx context.Context, command string, params, out any) error {
	select {
	case <-s.closed:
		return fmt.Errorf("pi: %s: session closed", command)
	default:
	}
	data, err := s.conn.client.Request(ctx, command, params)
	if err != nil || out == nil || len(data) == 0 {
		return err
	}
	return json.Unmarshal(data, out)
}

func (s *session) deliver(event engine.Event) {
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	select {
	case <-s.closed:
		return
	default:
	}
	select {
	case s.events <- event:
	case <-s.closed:
	}
}

func (s *session) ID() string { return s.id }

type promptParams struct {
	Message           string       `json:"message"`
	Images            []imageParam `json:"images,omitempty"`
	StreamingBehavior string       `json:"streamingBehavior,omitempty"`
}

type imageParam struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

func images(in []engine.Image) []imageParam {
	out := make([]imageParam, len(in))
	for i, image := range in {
		out[i] = imageParam{Type: "image", Data: image.Data, MimeType: image.MimeType}
	}
	return out
}

// Prompt returns once Pi accepted the input; the turn's outcome arrives as events
// ending in agent_settled. Pi applies streamingBehavior only while a turn runs.
func (s *session) Prompt(ctx context.Context, prompt engine.Prompt) error {
	return s.call(ctx, "prompt", promptParams{Message: prompt.Text, Images: images(prompt.Images), StreamingBehavior: "followUp"}, nil)
}

func (s *session) Steer(ctx context.Context, prompt engine.Prompt) error {
	return s.call(ctx, "steer", promptParams{Message: prompt.Text, Images: images(prompt.Images)}, nil)
}

// Abort clears queued input and stops the turn. If Pi does not confirm, its
// process tree is stopped instead.
func (s *session) Abort(ctx context.Context) error {
	err := s.call(ctx, "clear_queue", nil, nil)
	if err == nil {
		err = s.call(ctx, "abort", nil, nil)
	}
	if err != nil {
		s.close()
		s.engine.pool.Stop(s.botID)
	}
	return nil
}

// Answer sends an extension_ui_response. value is the response body without
// type and id, such as {"value":"Allow"}, {"confirmed":true} or {"cancelled":true}.
func (s *session) Answer(_ context.Context, requestID string, value json.RawMessage) error {
	fields := map[string]json.RawMessage{}
	if len(value) > 0 {
		if err := json.Unmarshal(value, &fields); err != nil {
			return fmt.Errorf("pi: answer must be an object: %w", err)
		}
	}
	fields["type"], _ = json.Marshal("extension_ui_response")
	fields["id"], _ = json.Marshal(requestID)
	return s.conn.client.Send(fields)
}

func (s *session) SetModel(ctx context.Context, model engine.Model, thinkingLevel string) error {
	if err := s.call(ctx, "set_model", model, nil); err != nil {
		return err
	}
	if thinkingLevel == "" {
		return nil
	}
	return s.call(ctx, "set_thinking_level", map[string]string{"level": thinkingLevel}, nil)
}

// Compact waits for the compaction to finish, bounded only by ctx.
func (s *session) Compact(ctx context.Context, instructions string) error {
	var params any
	if instructions != "" {
		params = map[string]string{"customInstructions": instructions}
	}
	return s.request(ctx, "compact", params, nil)
}

func (s *session) State(ctx context.Context) (engine.State, error) {
	var state struct {
		Model *struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
		} `json:"model"`
		ThinkingLevel       string `json:"thinkingLevel"`
		IsStreaming         bool   `json:"isStreaming"`
		IsCompacting        bool   `json:"isCompacting"`
		SessionFile         string `json:"sessionFile"`
		PendingMessageCount int    `json:"pendingMessageCount"`
	}
	if err := s.call(ctx, "get_state", nil, &state); err != nil {
		return engine.State{}, err
	}
	out := engine.State{
		SessionID:     state.SessionFile,
		ThinkingLevel: state.ThinkingLevel,
		Streaming:     state.IsStreaming,
		Compacting:    state.IsCompacting,
		PendingInputs: state.PendingMessageCount,
	}
	if state.Model != nil {
		out.Model = &engine.Model{Provider: state.Model.Provider, ModelID: state.Model.ID}
	}
	return out, nil
}

func (s *session) Usage(ctx context.Context) (engine.Usage, error) {
	var stats struct {
		Tokens engine.Usage `json:"tokens"`
		Cost   float64      `json:"cost"`
	}
	if err := s.call(ctx, "get_session_stats", nil, &stats); err != nil {
		return engine.Usage{}, err
	}
	stats.Tokens.Cost = stats.Cost
	return stats.Tokens, nil
}

// Messages pages Pi's current context messages from get_messages.
func (s *session) Messages(ctx context.Context, offset, limit int) ([]json.RawMessage, error) {
	var result struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := s.call(ctx, "get_messages", nil, &result); err != nil {
		return nil, err
	}
	if offset < 0 || offset >= len(result.Messages) || limit <= 0 {
		return nil, nil
	}
	return result.Messages[offset:min(offset+limit, len(result.Messages))], nil
}

// Events closes when the session closes or its Pi exits.
func (s *session) Events() <-chan engine.Event { return s.events }

// Close detaches the session and releases the Bot's Pi, which exits after the
// idle timeout unless another session starts first.
func (s *session) Close() error {
	s.close()
	return nil
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		s.conn.unclaim(s)
		close(s.closed)
		s.eventsMu.Lock()
		close(s.events)
		s.eventsMu.Unlock()
		s.release()
	})
}
