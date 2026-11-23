package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Text string `json:"text"`
}

func testMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	var once sync.Once
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo text."}, func(ctx context.Context, request *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
		if token := request.Params.GetProgressToken(); token != nil {
			_ = request.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 1, Total: 2, Message: "half"})
		}
		once.Do(func() {
			server.AddTool(&mcp.Tool{Name: "later", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
		})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}}}, nil, nil
	})
	return server
}

// mcpClients dials both transports with a notification sink.
func mcpClients(t *testing.T) (map[string]*Client, chan string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	notifications := make(chan string, 16)
	options := Options{MCPNotification: func(id string, n MCPNotification) { notifications <- id + " " + n.Method }}
	clients := map[string]*Client{}
	local, err := Dial(ctx, Local(executable(t)), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	clients["stdio"] = local
	if _, err := exec.LookPath("ssh"); err == nil {
		remote, err := Dial(ctx, SSH(sshServer(t)), options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = remote.Close() })
		clients["ssh"] = remote
	}
	return clients, notifications
}

func toolNames(tools []*mcp.Tool) string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return strings.Join(names, ",")
}

func callEcho(t *testing.T, client *Client, id string) {
	t.Helper()
	var mu sync.Mutex
	var progress []MCPProgress
	result, err := client.MCP(context.Background(), MCPInput{
		Action: "request", ID: id, Method: "tools/call",
		Params: json.RawMessage(`{"name":"echo","arguments":{"text":"hi"}}`),
	}, func(p MCPProgress) {
		mu.Lock()
		progress = append(progress, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Result), `"text":"echo: hi"`) {
		t.Fatalf("result = %s", result.Result)
	}
	mu.Lock()
	defer mu.Unlock()
	// The SDK client handles notifications concurrently with responses, so a
	// progress notification can arrive after the result; it must never be wrong.
	if len(progress) > 1 || len(progress) == 1 && (progress[0].Message != "half" || progress[0].Total != 2) {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestMCPStdio(t *testing.T) {
	clients, notifications := mcpClients(t)
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			config := &MCPConfig{Transport: "stdio", Command: executable(t), Args: []string{"mcp-stdio"}}
			inspected, err := client.MCP(ctx, MCPInput{Action: "inspect", Config: config}, nil)
			if err != nil || toolNames(inspected.Tools) != "echo" || inspected.Capabilities == nil || inspected.Capabilities.Tools == nil {
				t.Fatalf("inspect = %+v, %v", inspected, err)
			}
			if _, err := client.MCP(ctx, MCPInput{Action: "connect", ID: "s", Config: config}, nil); err != nil {
				t.Fatal(err)
			}
			callEcho(t, client, "s")
			select {
			case got := <-notifications:
				if got != "s notifications/tools/list_changed" {
					t.Fatalf("notification = %q", got)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no list_changed notification")
			}
			_, err = client.MCP(ctx, MCPInput{Action: "request", ID: "s", Method: "tools/call", Params: json.RawMessage(`{"name":"missing"}`)}, nil)
			if code(err) != "MCP_PROTOCOL" {
				t.Fatalf("missing tool: %v", err)
			}
			if _, err := client.MCP(ctx, MCPInput{Action: "disconnect", ID: "s"}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := client.MCP(ctx, MCPInput{Action: "request", ID: "s", Method: "tools/list"}, nil); code(err) != "EINVAL" {
				t.Fatalf("after disconnect: %v", err)
			}
			if _, err := client.MCP(ctx, MCPInput{Action: "inspect", Config: &MCPConfig{Transport: "stdio", Command: "persona-missing-mcp"}}, nil); code(err) != "ENOENT" {
				t.Fatalf("missing program: %v", err)
			}
		})
	}
}

func TestMCPNativeCatalog(t *testing.T) {
	clients, _ := mcpClients(t)
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			result, err := client.MCP(context.Background(), MCPInput{
				Action: "native",
				Config: &MCPConfig{Provider: "pi", Command: executable(t)},
				Cwd:    cwd,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Items) != 2 || result.Items[0].Name != "docs" || result.Items[0].Source != cwd ||
				result.Items[0].Enabled || result.Items[0].Status != "disabled" ||
				result.Items[1].Name != "local" || result.Items[1].Enabled != true {
				t.Fatalf("native items = %+v", result.Items)
			}
			if result.Inheritance != "native" || !result.Readonly {
				t.Fatalf("native catalog flags = inheritance %q, readonly %t", result.Inheritance, result.Readonly)
			}
		})
	}
}

func TestParseNativeCatalogText(t *testing.T) {
	tests := []struct {
		provider string
		output   string
		want     []MCPNativeItem
	}{
		{
			provider: "claude",
			output:   "docs: connected\npersona: connected\n",
			want: []MCPNativeItem{
				{Name: "docs", Source: "claude 用户/项目配置", Enabled: true, Status: "unknown"},
			},
		},
		{
			provider: "opencode",
			output:   "\x1b[32m● docs connected\x1b[0m\n○ local disabled\npersona connected\n",
			want: []MCPNativeItem{
				{Name: "docs", Source: "opencode 用户/项目配置", Enabled: true, Status: "connected"},
				{Name: "local", Source: "opencode 用户/项目配置", Enabled: false, Status: "disabled"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			items, err := parseNativeCatalog(test.provider, false, test.output)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(items) != fmt.Sprint(test.want) {
				t.Fatalf("items = %+v, want %+v", items, test.want)
			}
		})
	}
}

// oauthServer is an MCP endpoint at /mcp behind OAuth with resource metadata,
// dynamic registration and refresh, plus an unauthenticated legacy SSE endpoint.
func oauthServer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	accepted := &atomic.Value{}
	accepted.Store("access-1")
	server := testMCPServer()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// A failed test can leave a standalone SSE stream open; Close waits for it.
	t.Cleanup(srv.CloseClientConnections)
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL}, "scopes_supported": []string{"read"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token",
			"registration_endpoint": srv.URL + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"client_id": "client-1", "redirect_uris": []string{"http://127.0.0.1/callback"}, "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code") == "good" && r.Form.Get("code_verifier") != "" &&
			r.Form.Get("client_id") == "client-1" && r.Form.Get("resource") == srv.URL+"/mcp":
			writeJSON(w, map[string]any{"access_token": "access-1", "token_type": "Bearer", "refresh_token": "refresh-1", "expires_in": 3600})
		case r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "refresh-1":
			writeJSON(w, map[string]any{"access_token": "access-2", "token_type": "Bearer", "expires_in": 3600})
		default:
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "invalid_grant"})
		}
	})
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+accepted.Load().(string) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		streamable.ServeHTTP(w, r)
	})
	mux.Handle("/sse", mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil))
	return srv, accepted
}

func TestMCPHTTPOAuth(t *testing.T) {
	clients, _ := mcpClients(t)
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			srv, accepted := oauthServer(t)
			config := &MCPConfig{Transport: "http", URL: srv.URL + "/mcp"}
			if _, err := client.MCP(ctx, MCPInput{Action: "inspect", Config: config}, nil); code(err) != "MCP_AUTH" {
				t.Fatalf("unauthorized inspect: %v", err)
			}
			redirect := "http://127.0.0.1/callback"
			started, err := client.MCP(ctx, MCPInput{Action: "authorize", Config: config, OAuth: &OAuthState{}, RedirectURL: redirect, State: "conn.1"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			link, err := url.Parse(started.AuthorizationURL)
			if err != nil || !strings.HasPrefix(started.AuthorizationURL, srv.URL+"/authorize?") {
				t.Fatalf("authorization URL = %q", started.AuthorizationURL)
			}
			query := link.Query()
			if query.Get("state") != "conn.1" || query.Get("client_id") != "client-1" || query.Get("code_challenge_method") != "S256" ||
				query.Get("resource") != srv.URL+"/mcp" || query.Get("scope") != "read" || query.Get("redirect_uri") != redirect {
				t.Fatalf("authorization query = %v", query)
			}
			if started.OAuth.Verifier == "" || started.OAuth.Discovery == nil || len(started.OAuth.Client) == 0 {
				t.Fatalf("oauth = %+v", started.OAuth)
			}
			completed, err := client.MCP(ctx, MCPInput{Action: "complete", Config: config, OAuth: started.OAuth, RedirectURL: redirect, State: "conn.1", AuthorizationCode: "good"}, nil)
			if err != nil || completed.OAuth.Tokens == nil || completed.OAuth.Tokens.AccessToken != "access-1" {
				t.Fatalf("complete = %+v, %v", completed, err)
			}
			inspected, err := client.MCP(ctx, MCPInput{Action: "inspect", Config: config, OAuth: completed.OAuth, RedirectURL: redirect}, nil)
			if err != nil || toolNames(inspected.Tools) != "echo" {
				t.Fatalf("inspect = %+v, %v", inspected, err)
			}
			accepted.Store("access-2")
			connected, err := client.MCP(ctx, MCPInput{Action: "connect", ID: "h", Config: config, OAuth: completed.OAuth, RedirectURL: redirect}, nil)
			if err != nil || connected.OAuth.Tokens.AccessToken != "access-2" || connected.OAuth.Tokens.RefreshToken != "refresh-1" {
				t.Fatalf("connect after refresh = %+v, %v", connected, err)
			}
			callEcho(t, client, "h")
			if _, err := client.MCP(ctx, MCPInput{Action: "disconnect", ID: "h"}, nil); err != nil {
				t.Fatal(err)
			}
			legacy, err := client.MCP(ctx, MCPInput{Action: "inspect", Config: &MCPConfig{Transport: "http", URL: srv.URL + "/sse"}}, nil)
			if err != nil || !strings.HasPrefix(toolNames(legacy.Tools), "echo") {
				t.Fatalf("legacy SSE inspect = %+v, %v", legacy, err)
			}
		})
	}
}
