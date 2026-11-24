package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPConfig is a saved MCP connection: a stdio program or an HTTP endpoint.
type MCPConfig struct {
	Provider  string            `json:"provider,omitempty"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// MCPInput is one managed MCP operation, in the shape the TypeScript worker
// accepted: inspect, connect, request, disconnect, authorize, complete or
// native. Config holds the connection config or native Harness config.
type MCPInput struct {
	Action            string          `json:"action"`
	ID                string          `json:"id,omitempty"`
	Config            *MCPConfig      `json:"config,omitempty"`
	Cwd               string          `json:"cwd,omitempty"`
	OAuth             *OAuthState     `json:"oauth,omitempty"`
	RedirectURL       string          `json:"redirectUrl,omitempty"`
	State             string          `json:"state,omitempty"`
	AuthorizationCode string          `json:"authorizationCode,omitempty"`
	Method            string          `json:"method,omitempty"`
	Params            json.RawMessage `json:"params,omitempty"`
}

// MCPResult answers an MCPInput. OAuth is the connection's OAuth state after
// the operation, which the service stores again. Items, Inheritance and
// Readonly answer a "native" operation.
type MCPResult struct {
	Capabilities     *mcp.ServerCapabilities `json:"capabilities,omitempty"`
	Tools            []*mcp.Tool             `json:"tools"`
	Result           json.RawMessage         `json:"result,omitempty"`
	AuthorizationURL string                  `json:"authorizationUrl,omitempty"`
	OAuth            *OAuthState             `json:"oauth"`
	Items            []MCPNativeItem         `json:"items,omitempty"`
	Inheritance      string                  `json:"inheritance,omitempty"`
	Readonly         bool                    `json:"readonly,omitempty"`
}

// MCPNativeItem is one connection the Bot's native Harness configures itself.
type MCPNativeItem struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
}

// MCPProgress is a progress notification for a forwarded request.
type MCPProgress struct {
	Progress float64 `json:"progress"`
	Total    float64 `json:"total,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// MCPNotification is a list-changed or resource-updated notification from a
// connected server.
type MCPNotification struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type mcpEvent struct {
	Type         string           `json:"type"`
	ID           string           `json:"id,omitempty"`
	Notification *MCPNotification `json:"notification,omitempty"`
	Progress     *MCPProgress     `json:"progress,omitempty"`
}

var errUnauthorized = errors.New("the MCP server requires authorization")

type mcpSession struct {
	session  *mcp.ClientSession
	oauth    *oauthSource
	group    *group
	progress sync.Map
}

var progressTokens atomic.Uint64

func (s *server) mcp(ctx context.Context, id uint32, input MCPInput) (*MCPResult, error) {
	result, err := s.mcpOperation(ctx, id, input)
	if err != nil {
		return nil, mcpError(ctx, err)
	}
	return result, nil
}

func (s *server) mcpOperation(ctx context.Context, id uint32, input MCPInput) (*MCPResult, error) {
	switch input.Action {
	case "native":
		return nativeMCP(ctx, input.Config, input.Cwd)
	case "authorize", "complete":
		return authorizeMCP(ctx, input)
	case "request":
		s.mu.Lock()
		session := s.mcpSessions[input.ID]
		s.mu.Unlock()
		if session == nil {
			return nil, codeError("EINVAL", "MCP session is not connected", "")
		}
		result, err := session.request(ctx, input.Method, input.Params, func(progress MCPProgress) {
			s.send(frameEvent, id, mcpEvent{Type: "progress", Progress: &progress}, nil)
		})
		if err != nil {
			return nil, err
		}
		return &MCPResult{Result: result, OAuth: session.oauthState()}, nil
	case "disconnect":
		s.mu.Lock()
		session := s.mcpSessions[input.ID]
		delete(s.mcpSessions, input.ID)
		s.mu.Unlock()
		if session != nil {
			session.close()
		}
		return &MCPResult{OAuth: &OAuthState{}}, nil
	case "inspect", "connect":
		if input.Action == "connect" && input.ID == "" {
			return nil, codeError("EINVAL", "MCP connection id is required", "")
		}
		var notify func(MCPNotification)
		if input.Action == "connect" {
			notify = func(notification MCPNotification) {
				s.send(frameEvent, 0, mcpEvent{Type: "mcpNotification", ID: input.ID, Notification: &notification}, nil)
			}
		}
		session, err := openMCP(ctx, input, notify)
		if err != nil {
			return nil, err
		}
		result, err := session.describe(ctx)
		if err != nil || input.Action == "inspect" {
			session.close()
			return result, err
		}
		s.mu.Lock()
		previous := s.mcpSessions[input.ID]
		s.mcpSessions[input.ID] = session
		s.mu.Unlock()
		if previous != nil {
			previous.close()
		}
		return result, nil
	}
	return nil, codeError("EINVAL", "unknown MCP action "+input.Action, "")
}

func (s *server) closeMCP() {
	s.mu.Lock()
	sessions := s.mcpSessions
	s.mcpSessions = map[string]*mcpSession{}
	s.mu.Unlock()
	for _, session := range sessions {
		session.close()
	}
}

// openMCP connects with the official MCP SDK. HTTP endpoints try streamable
// HTTP first and fall back to the legacy SSE transport.
func openMCP(ctx context.Context, input MCPInput, notify func(MCPNotification)) (*mcpSession, error) {
	config := input.Config
	if config == nil {
		return nil, codeError("EINVAL", "MCP configuration is missing", "")
	}
	forward := func(method string, params any) {
		if notify != nil {
			notify(MCPNotification{Method: method, Params: params})
		}
	}
	session := &mcpSession{}
	client := mcp.NewClient(&mcp.Implementation{Name: "persona", Version: Version}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{},
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			if f, ok := session.progress.Load(fmt.Sprint(r.Params.ProgressToken)); ok {
				f.(func(MCPProgress))(MCPProgress{Progress: r.Params.Progress, Total: r.Params.Total, Message: r.Params.Message})
			}
		},
		ToolListChangedHandler: func(_ context.Context, r *mcp.ToolListChangedRequest) {
			forward("notifications/tools/list_changed", r.Params)
		},
		PromptListChangedHandler: func(_ context.Context, r *mcp.PromptListChangedRequest) {
			forward("notifications/prompts/list_changed", r.Params)
		},
		ResourceListChangedHandler: func(_ context.Context, r *mcp.ResourceListChangedRequest) {
			forward("notifications/resources/list_changed", r.Params)
		},
		ResourceUpdatedHandler: func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) {
			forward("notifications/resources/updated", r.Params)
		},
	})
	switch config.Transport {
	case "stdio":
		env := stdioEnvironment(config.Env)
		path, err := ResolveProgram(ctx, config.Command, env)
		if err != nil {
			return nil, err
		}
		if path == "" {
			return nil, codeError("ENOENT", config.Command+" is not installed for this user", config.Command)
		}
		cmd := exec.Command(path, config.Args...)
		cmd.Env, cmd.Stderr = env, io.Discard
		isolate(cmd)
		cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if cmd.Process != nil {
			session.group = attach(cmd)
		}
		if err != nil {
			session.close()
			return nil, err
		}
		session.session = cs
	case "http":
		httpClient := &http.Client{Transport: headerTransport{headers: config.Headers, base: http.DefaultTransport}}
		session.oauth = newOAuthSource(input.OAuth, input.RedirectURL)
		cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: config.URL, HTTPClient: httpClient, OAuthHandler: session.oauth, MaxRetries: -1}, nil)
		if err != nil && !errors.Is(err, errUnauthorized) && ctx.Err() == nil {
			legacy := &http.Client{Transport: headerTransport{headers: config.Headers, base: http.DefaultTransport, oauth: session.oauth}}
			if sse, sseErr := client.Connect(context.WithoutCancel(ctx), &mcp.SSEClientTransport{Endpoint: config.URL, HTTPClient: legacy}, nil); sseErr == nil {
				cs, err = sse, nil
			}
		}
		if err != nil {
			return nil, err
		}
		session.session = cs
	default:
		return nil, codeError("EINVAL", "unsupported MCP transport "+config.Transport, "")
	}
	return session, nil
}

// describe lists the server's capabilities and every page of its tools.
func (m *mcpSession) describe(ctx context.Context) (*MCPResult, error) {
	capabilities := &mcp.ServerCapabilities{}
	if initialized := m.session.InitializeResult(); initialized != nil && initialized.Capabilities != nil {
		capabilities = initialized.Capabilities
	}
	tools := []*mcp.Tool{}
	if capabilities.Tools != nil {
		for tool, err := range m.session.Tools(ctx, nil) {
			if err != nil {
				return nil, err
			}
			tools = append(tools, tool)
		}
	}
	return &MCPResult{Capabilities: capabilities, Tools: tools, OAuth: m.oauthState()}, nil
}

func (m *mcpSession) request(ctx context.Context, method string, params json.RawMessage, progress func(MCPProgress)) (json.RawMessage, error) {
	token := fmt.Sprintf("persona-%d", progressTokens.Add(1))
	m.progress.Store(token, progress)
	defer m.progress.Delete(token)
	cs := m.session
	var result any
	var err error
	switch method {
	case "tools/list":
		result, err = forward(ctx, params, token, cs.ListTools)
	case "tools/call":
		result, err = forward(ctx, params, token, cs.CallTool)
	case "resources/list":
		result, err = forward(ctx, params, token, cs.ListResources)
	case "resources/templates/list":
		result, err = forward(ctx, params, token, cs.ListResourceTemplates)
	case "resources/read":
		result, err = forward(ctx, params, token, cs.ReadResource)
	case "prompts/list":
		result, err = forward(ctx, params, token, cs.ListPrompts)
	case "prompts/get":
		result, err = forward(ctx, params, token, cs.GetPrompt)
	case "resources/subscribe":
		result, err = forward(ctx, params, token, empty(cs.Subscribe))
	case "resources/unsubscribe":
		result, err = forward(ctx, params, token, empty(cs.Unsubscribe))
	default:
		return nil, codeError("EINVAL", "unsupported MCP method "+method, "")
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func forward[P any, R any](ctx context.Context, raw json.RawMessage, token string, call func(context.Context, *P) (R, error)) (R, error) {
	params := new(P)
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, params); err != nil {
			var zero R
			return zero, codeError("EINVAL", "invalid MCP params: "+err.Error(), "")
		}
	}
	if p, ok := any(params).(interface{ SetProgressToken(any) }); ok {
		p.SetProgressToken(token)
	}
	return call(ctx, params)
}

func empty[P any](call func(context.Context, *P) error) func(context.Context, *P) (struct{}, error) {
	return func(ctx context.Context, params *P) (struct{}, error) { return struct{}{}, call(ctx, params) }
}

func (m *mcpSession) oauthState() *OAuthState {
	if m.oauth == nil {
		return &OAuthState{}
	}
	return m.oauth.snapshot()
}

func (m *mcpSession) close() {
	if m.session != nil {
		_ = m.session.Close()
	}
	if m.group != nil {
		m.group.kill()
	}
}

// kill stops the server's process group before closing the session, so a
// server that is still running a request does not get to finish it.
func (m *mcpSession) kill() {
	if m.group != nil {
		m.group.kill()
	}
	if m.session != nil {
		_ = m.session.Close()
	}
}

// mcpError keeps protocol errors from the server, reports authorization as
// MCP_AUTH, and hides other connection details.
func mcpError(ctx context.Context, err error) *Error {
	var wire *jsonrpc.Error
	var local *Error
	switch {
	case errors.Is(err, errUnauthorized):
		return &Error{Code: "MCP_AUTH", Message: "Sign in to the MCP server."}
	case errors.As(err, &wire):
		return &Error{Code: "MCP_PROTOCOL", Message: wire.Message, MCPCode: wire.Code}
	case errors.As(err, &local):
		return local
	case ctx.Err() != nil:
		return toError(ctx.Err())
	}
	return &Error{Code: "MCP_FAILED", Message: "The MCP connection did not complete. Check the configuration and authorization."}
}

// stdioEnvironment passes the variables the MCP SDKs inherit by default, then
// the connection's own.
func stdioEnvironment(extra map[string]string) []string {
	keys := []string{"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "USER"}
	if runtime.GOOS == "windows" {
		keys = []string{"APPDATA", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "PATH", "PROCESSOR_ARCHITECTURE", "SYSTEMDRIVE", "SYSTEMROOT", "TEMP", "USERNAME", "USERPROFILE", "PROGRAMFILES"}
	}
	var env []string
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok && !strings.HasPrefix(value, "()") {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	return env
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
	oauth   *oauthSource
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for key, value := range t.headers {
		request.Header.Set(key, value)
	}
	if t.oauth != nil {
		if token := t.oauth.accessToken(); token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return t.base.RoundTrip(request)
}
