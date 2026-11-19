// Package mcpserver serves the product tool registry to a native engine over MCP.
//
// Pi registers the gateway URL with registerMcpServer and direct exposure, so the
// product tools appear as native tools. One gateway serves one Bot task on loopback
// behind an unguessable path, like packages/agent/src/harness/gateway.mjs.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/tools"
)

// MaxRequestBytes bounds one MCP request body.
const MaxRequestBytes = 1 << 20

// ResultHandleMeta is the result _meta key that carries tools.Result.Handle.
const ResultHandleMeta = "persona/resultHandle"

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
	listed     chan struct{}
	listedOnce sync.Once
	served     chan error
}

// Open lists the Bot's tools and starts serving them on a loopback port.
func Open(ctx context.Context, options Options) (*Gateway, error) {
	definitions, err := options.Registry.List(ctx, options.BotID)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	nonce := make([]byte, 8)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	gateway := &Gateway{listed: make(chan struct{}), served: make(chan error, 1)}
	server := mcp.NewServer(&mcp.Implementation{Name: "persona", Version: "1.0.0"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				gateway.listedOnce.Do(func() { close(gateway.listed) })
			}
			return next(ctx, method, request)
		}
	})
	var sequence atomic.Int64
	callPrefix := hex.EncodeToString(nonce) + ":"
	for _, definition := range definitions {
		tool, err := declare(definition)
		if err != nil {
			return nil, err
		}
		server.AddTool(tool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := options.Registry.Call(ctx, tools.Call{
				BotID:  options.BotID,
				TaskID: options.TaskID,
				CallID: callPrefix + strconv.FormatInt(sequence.Add(1), 10),
				Name:   request.Params.Name,
				Input:  request.Params.Arguments,
			}, options.Approve)
			if err != nil {
				return errorResult(err), nil
			}
			return convert(result)
		})
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	path := "/mcp/" + hex.EncodeToString(secret)
	mux := http.NewServeMux()
	mux.Handle(path, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		JSONResponse:        true,
		MaxRequestBodyBytes: MaxRequestBytes,
	}))
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

// Close stops the gateway and its connections.
func (g *Gateway) Close() error {
	err := g.server.Close()
	if served := <-g.served; !errors.Is(served, http.ErrServerClosed) {
		return errors.Join(err, served)
	}
	return err
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
