package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// LoginTimeout ends a login nobody finishes.
const LoginTimeout = 10 * time.Minute

const authPrefix = "persona-auth:"

// ErrLoginOver reports an answer to a login that already ended or asks nothing.
var ErrLoginOver = errors.New("这次登录已结束。请重新登录。")

// LoginState is EngineLoginState from harness/logins.ts.
type LoginState struct {
	State        string   `json:"state"` // waiting, done or failed
	URL          *string  `json:"url"`
	Code         *string  `json:"code"`
	Prompt       *string  `json:"prompt"`
	Instructions *string  `json:"instructions"`
	Options      []string `json:"options,omitempty"`
	Secret       bool     `json:"secret,omitempty"`
}

// Login is one Pi provider login driven over RPC through the pi-auth.mjs
// extension's /persona-auth command. Its Pi process ends with the login.
type Login struct {
	proc   *Process
	client *Client
	grace  time.Duration
	done   chan struct{}

	mu      sync.Mutex
	state   LoginState
	request *loginRequest
	changed chan struct{}
}

// Login starts a provider login. extension is the path of pi-auth.mjs; Pi
// runs without a session and with PERSONA_PI_LOGIN=1, as the worker does.
func (e *Engine) Login(ctx context.Context, extension string) (*Login, error) {
	args := append([]string{"--mode", "rpc", "--no-session"}, e.config.Args...)
	p, err := Start(Command{
		Path: e.config.Command,
		Args: append(args, "--extension", extension),
		Env:  mergeEnv(e.config.Env, []string{"PERSONA_PI_LOGIN=1"}),
	})
	if err != nil {
		return nil, fmt.Errorf("pi: start: %w", err)
	}
	l := &Login{
		proc: p, client: NewClient(p.Stdout(), p.Stdin()), grace: e.config.StopGrace,
		done: make(chan struct{}), state: LoginState{State: "waiting"}, changed: make(chan struct{}),
	}
	go l.watch()
	ctx, cancel := e.bound(ctx)
	defer cancel()
	data, err := l.client.Request(ctx, "get_commands", nil)
	if err == nil {
		var commands struct {
			Commands []command `json:"commands"`
		}
		_ = json.Unmarshal(data, &commands)
		if !slices.ContainsFunc(commands.Commands, func(c command) bool { return c.Name == "persona-auth" }) {
			err = errors.New("此 Pi 版本未加载登录接口。请更新 CLI。")
		}
	}
	if err == nil {
		// The prompt is answered when /persona-auth returns, which is after the
		// whole login, so it is not awaited here.
		go func() {
			if _, err := l.client.Request(context.Background(), "prompt", map[string]string{"message": "/persona-auth"}); err != nil {
				l.finish(false)
			}
		}()
	}
	if err != nil {
		l.finish(false)
		return nil, err
	}
	return l, nil
}

func (l *Login) watch() {
	timeout := time.NewTimer(LoginTimeout)
	defer timeout.Stop()
	for {
		select {
		case frame, ok := <-l.client.Events():
			if !ok {
				l.finish(false)
				return
			}
			l.handle(frame)
		case <-timeout.C:
			l.finish(false)
		case <-l.done:
			return
		}
	}
}

type command struct {
	Name string `json:"name"`
}

type loginRequest struct {
	ID      string   `json:"id"`
	Method  string   `json:"method"`
	Title   string   `json:"title"`
	Message string   `json:"message"`
	Options []string `json:"options"`
}

func (l *Login) handle(frame Frame) {
	var r loginRequest
	if frame.Type != "extension_ui_request" || json.Unmarshal(frame.Raw, &r) != nil {
		return
	}
	switch {
	case r.Method == "notify" && strings.HasPrefix(r.Message, authPrefix):
		var v struct {
			State           string  `json:"state"`
			Secret          *bool   `json:"secret"`
			Type            string  `json:"type"`
			URL             string  `json:"url"`
			VerificationURI string  `json:"verificationUri"`
			UserCode        *string `json:"userCode"`
			Instructions    *string `json:"instructions"`
			Message         *string `json:"message"`
		}
		if json.Unmarshal([]byte(r.Message[len(authPrefix):]), &v) != nil {
			return
		}
		if v.State != "" {
			l.finish(v.State == "done")
			return
		}
		l.update(func(s *LoginState) {
			if v.Secret != nil {
				s.Secret = *v.Secret
			}
			if v.Type == "auth_url" || v.Type == "device_code" {
				raw := v.URL
				if raw == "" {
					raw = v.VerificationURI
				}
				// Only a plain https link is shown to the user.
				if u, err := url.Parse(raw); err == nil && u.Scheme == "https" && u.User == nil {
					href := u.String()
					s.URL = &href
				}
				s.Code = v.UserCode
			}
			if v.Instructions != nil {
				s.Instructions = v.Instructions
			} else if v.Message != nil {
				s.Instructions = v.Message
			}
		})
	case r.Method == "select" || r.Method == "input":
		l.update(func(s *LoginState) {
			title := r.Title
			s.Prompt, s.Options = &title, r.Options
			l.request = &r
		})
	}
}

func (l *Login) update(change func(*LoginState)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.State != "waiting" {
		return
	}
	change(&l.state)
	close(l.changed)
	l.changed = make(chan struct{})
}

// State returns the login's current state and a channel that closes at the next change.
func (l *Login) State() (LoginState, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state, l.changed
}

// Done closes when the login has ended and its Pi process is gone.
func (l *Login) Done() <-chan struct{} { return l.done }

// Answer sends the user's choice or code for the current prompt.
func (l *Login) Answer(ctx context.Context, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return errors.New("请输入登录页面显示的代码。")
	}
	l.mu.Lock()
	r := l.request
	if l.state.State != "waiting" || r == nil {
		l.mu.Unlock()
		return ErrLoginOver
	}
	if r.Options != nil && !slices.Contains(r.Options, value) {
		l.mu.Unlock()
		return errors.New("请选择可用选项。")
	}
	l.request = nil
	l.state.Prompt, l.state.Options = nil, nil
	l.mu.Unlock()
	if err := l.client.Send(ctx, map[string]string{"type": "extension_ui_response", "id": r.ID, "value": value}); err != nil {
		l.finish(false)
		return errors.New("登录未完成。请重新登录。")
	}
	return nil
}

// Cancel ends the login as failed and stops its Pi process.
func (l *Login) Cancel() { l.finish(false) }

func (l *Login) finish(success bool) {
	l.mu.Lock()
	if l.state.State != "waiting" {
		l.mu.Unlock()
		<-l.done
		return
	}
	l.state = LoginState{State: "failed"}
	if success {
		l.state.State = "done"
	}
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
	_ = l.proc.Stop(l.grace)
	close(l.done)
}
