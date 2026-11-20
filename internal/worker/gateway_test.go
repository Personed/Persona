package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/mcpserver"
	"github.com/Personed/Persona/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type gatewayRegistry struct {
	tools.Registry
	calls chan tools.Call
}

func (r gatewayRegistry) List(context.Context, int64) ([]tools.Definition, error) {
	return []tools.Definition{{Name: "test_echo", InputSchema: json.RawMessage(`{"type":"object"}`), Mutates: true}}, nil
}

func (r gatewayRegistry) Call(ctx context.Context, call tools.Call, approve tools.Approver) (tools.Result, error) {
	allowed, err := approve(ctx, call, tools.Definition{Name: call.Name, Mutates: true})
	if err != nil {
		return tools.Result{}, err
	}
	if !allowed {
		return tools.Result{}, errors.New("not approved")
	}
	r.calls <- call
	return tools.Result{
		Content:    []tools.Content{{Type: "text", Text: "service echo"}},
		Structured: call.Input,
	}, nil
}

func testPiGateway(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-pi", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	catalog, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	if catalog == nil || !slices.Contains(strings.Split(toolNames(catalog.Tools), ","), "test_echo") {
		return fmt.Errorf("unexpected gateway catalog: %s", toolNames(catalog.Tools))
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "test_echo", Arguments: map[string]string{"text": "from Pi"}})
	if err != nil {
		return err
	}
	// The product gateway may add its own text after the tool's content.
	if len(result.Content) == 0 {
		return errors.New("empty gateway result")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if result.IsError || !ok || text.Text != "service echo" {
		return fmt.Errorf("unexpected gateway result: %+v", result)
	}
	return nil
}

func TestProductGateway(t *testing.T) {
	for name, worker := range transports(t) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			registry := gatewayRegistry{calls: make(chan tools.Call, 1)}
			service, err := mcpserver.Open(ctx, mcpserver.Options{
				Registry: registry, BotID: 13, TaskID: 51,
				Approve: func(_ context.Context, c tools.Call, d tools.Definition) (bool, error) {
					return c.BotID == 13 && c.TaskID == 51 && d.Mutates, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			mount, err := worker.OpenGateway(ctx, service)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mount.Close() })
			if mount.URL == service.URL {
				t.Fatal("expected a distinct worker loopback endpoint")
			}
			var stderr bytes.Buffer
			proc, err := worker.Launch(ctx, LaunchOptions{
				Program: executable(t), Args: []string{"pi-gateway"}, Dir: t.TempDir(),
				Gateway: mount, Stderr: &stderr,
				Env: map[string]string{"PERSONA_PI_MCP_URL": "http://invalid", "GORACE": os.Getenv("GORACE")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if exit, err := proc.Wait(); exit != 0 || err != nil {
				t.Fatalf("Pi launch: %d, %v, %s", exit, err, stderr.String())
			}
			if err := service.Ready(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case call := <-registry.calls:
				if call.CallID == "" || string(call.Input) != `{"text":"from Pi"}` {
					t.Fatalf("unexpected product call: %+v", call)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := mount.Close(); err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, mount.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				t.Fatal("worker endpoint still open after close")
			}
		})
	}
}
