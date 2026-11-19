// Package mcpserver serves the product tool registry to a native engine over MCP.
//
// Pi registers the gateway URL with registerMcpServer and direct exposure, so the
// product tools appear as native tools. One gateway serves one Bot task on loopback
// behind an unguessable path, like packages/agent/src/harness/gateway.mjs.
package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/tools"
)

// MaxRequestBytes bounds one MCP request body.
const MaxRequestBytes = 1 << 20

// ResultHandleMeta is the result _meta key that carries tools.Result.Handle.
const ResultHandleMeta = "persona/resultHandle"

// requestIDHeader carries the JSON-RPC ID of a tools/call into its handler; the SDK
// does not expose it. Incoming values are discarded.
const requestIDHeader = "X-Persona-Request-Id"

// Options select the tools and identity of one gateway.
type Options struct {
	Registry tools.Registry
	BotID    int64
	TaskID   int64
	// Approve decides mutating calls. Nil denies them.
	Approve tools.Approver
}

// Gateway is a running MCP endpoint for one Bot task.
type Gateway struct {
	// URL is the endpoint the engine connects to.
	URL string

	server     *http.Server
	sessions   *mcp.Server
	listed     chan struct{}
	listedOnce sync.Once
	served     chan error
	closing    context.Context
	stop       context.CancelFunc

	mu       sync.Mutex
	calls    map[string]*outcome
	closed   bool
	requests sync.WaitGroup
}

// outcome is the recorded result of one call, returned again when the client
// redelivers the same request instead of running the tool twice.
type outcome struct {
	name   string
	input  string
	done   chan struct{}
	result *mcp.CallToolResult
}

// Open lists the Bot's tools and starts serving them on a loopback port.
func Open(ctx context.Context, options Options) (*Gateway, error) {
	definitions, err := options.Registry.List(ctx, options.BotID)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	gateway := &Gateway{listed: make(chan struct{}), served: make(chan error, 1), calls: map[string]*outcome{}}
	gateway.closing, gateway.stop = context.WithCancel(context.Background())
	server := mcp.NewServer(&mcp.Implementation{Name: "persona", Version: "1.0.0"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				gateway.listedOnce.Do(func() { close(gateway.listed) })
			}
			return next(ctx, method, request)
		}
	})
	for _, definition := range definitions {
		tool, err := declare(definition)
		if err != nil {
			return nil, err
		}
		server.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return gateway.call(ctx, options, request)
		})
	}

	gateway.sessions = server

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		gateway.stop()
		return nil, err
	}
	path := "/mcp/" + hex.EncodeToString(secret)
	mux := http.NewServeMux()
	mux.Handle(path, gateway.track(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		JSONResponse:        true,
		MaxRequestBodyBytes: MaxRequestBytes,
	})))
	gateway.server = &http.Server{Handler: mux}
	gateway.URL = "http://" + listener.Addr().String() + path
	go func() { gateway.served <- gateway.server.Serve(listener) }()
	return gateway, nil
}

// Ready waits until the engine lists the tools, which confirms product-tool discovery
// before task dispatch. The caller bounds the wait with ctx.
func (g *Gateway) Ready(ctx context.Context) error {
	select {
	case <-g.listed:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mcpserver: engine has not listed the Persona tools: %w", context.Cause(ctx))
	}
}

// Close stops the gateway, cancels its running calls, and ends its MCP sessions.
//
// SDK sessions outlive their HTTP connections, so they are closed here once no request
// can open another. A session waits for its running calls, which is why tools must
// return when their context is cancelled.
func (g *Gateway) Close() error {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.stop()
	err := g.server.Close()
	if served := <-g.served; !errors.Is(served, http.ErrServerClosed) {
		err = errors.Join(err, served)
	}
	g.requests.Wait()
	for session := range g.sessions.Sessions() {
		err = errors.Join(err, session.Close())
	}
	return err
}

// call runs one tools/call. Its CallID is the MCP session ID plus the JSON-RPC request
// ID, so a redelivered request keeps its identity and receives the recorded outcome.
func (g *Gateway) call(ctx context.Context, options Options, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var requestID string
	if request.Extra != nil {
		requestID = request.Extra.Header.Get(requestIDHeader)
	}
	if requestID == "" {
		return errorResult(errors.New("mcpserver: tools/call has no request ID")), nil
	}
	var input bytes.Buffer
	if len(request.Params.Arguments) > 0 {
		if err := json.Compact(&input, request.Params.Arguments); err != nil {
			return errorResult(err), nil
		}
	}
	callID := request.Session.ID() + ":" + requestID
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(g.closing, cancel)()
	g.mu.Lock()
	recorded, found := g.calls[callID]
	if !found {
		recorded = &outcome{name: request.Params.Name, input: input.String(), done: make(chan struct{})}
		g.calls[callID] = recorded
	}
	g.mu.Unlock()
	if found {
		if recorded.name != request.Params.Name || recorded.input != input.String() {
			return errorResult(fmt.Errorf("mcpserver: request ID %s was reused for a different call", requestID)), nil
		}
		select {
		case <-recorded.done:
			return recorded.result, nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	defer close(recorded.done)
	result, err := options.Registry.Call(ctx, tools.Call{
		BotID:  options.BotID,
		TaskID: options.TaskID,
		CallID: callID,
		Name:   request.Params.Name,
		Input:  request.Params.Arguments,
	}, options.Approve)
	if err != nil {
		recorded.result = errorResult(err)
	} else if recorded.result, err = convert(result); err != nil {
		recorded.result = errorResult(err)
	}
	return recorded.result, nil
}

// track refuses requests once the gateway is closing, lets Close wait for the ones it
// admitted, and passes the JSON-RPC ID of a tools/call to its handler.
func (g *Gateway) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			http.Error(w, "gateway closed", http.StatusServiceUnavailable)
			return
		}
		g.requests.Add(1)
		g.mu.Unlock()
		defer g.requests.Done()
		r.Header.Del(requestIDHeader)
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
			if err != nil {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(body, &message) == nil && message.Method == "tools/call" && len(message.ID) > 0 {
				r.Header.Set(requestIDHeader, string(message.ID))
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		next.ServeHTTP(w, r)
	})
}

func declare(definition tools.Definition) (*mcp.Tool, error) {
	tool := &mcp.Tool{Name: definition.Name, Description: definition.Description, InputSchema: definition.InputSchema}
	if len(definition.OutputSchema) > 0 {
		tool.OutputSchema = definition.OutputSchema
	}
	if len(definition.Annotations) > 0 {
		encoded, err := json.Marshal(definition.Annotations)
		if err != nil {
			return nil, err
		}
		tool.Annotations = &mcp.ToolAnnotations{}
		if err := json.Unmarshal(encoded, tool.Annotations); err != nil {
			return nil, fmt.Errorf("mcpserver: tool %q annotations: %w", definition.Name, err)
		}
	}
	return tool, nil
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

func convert(result tools.Result) (*mcp.CallToolResult, error) {
	converted := &mcp.CallToolResult{IsError: result.IsError, Content: []mcp.Content{}}
	for _, content := range result.Content {
		switch content.Type {
		case "text":
			converted.Content = append(converted.Content, &mcp.TextContent{Text: content.Text})
		case "image":
			data, err := base64.StdEncoding.DecodeString(content.Data)
			if err != nil {
				return nil, fmt.Errorf("mcpserver: image content: %w", err)
			}
			converted.Content = append(converted.Content, &mcp.ImageContent{Data: data, MIMEType: content.MimeType})
		case "resource":
			converted.Content = append(converted.Content, &mcp.ResourceLink{URI: content.URI, Name: content.URI, MIMEType: content.MimeType})
		default:
			return nil, fmt.Errorf("mcpserver: unknown content type %q", content.Type)
		}
	}
	if len(result.Structured) > 0 {
		converted.StructuredContent = result.Structured
	}
	if result.Handle != "" {
		converted.Meta = mcp.Meta{ResultHandleMeta: result.Handle}
	}
	return converted, nil
}
