package cli

import (
	"context"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/mcpserver"
	"github.com/Personed/Persona/internal/routines"
	"github.com/Personed/Persona/internal/skills"
	"github.com/Personed/Persona/internal/store/storetest"
	"github.com/Personed/Persona/internal/takeover"
)

func TestProductToolsServedToPi(t *testing.T) {
	ctx := context.Background()
	records := storetest.New()
	handoff := takeover.Handoff(func(context.Context, takeover.Request) (bool, error) { return false, nil })
	registry, err := productTools(records, routines.NewService(records, nil), skills.NewService(records, fstest.MapFS{}, nil), handoff)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := mcpserver.Open(ctx, mcpserver.Options{Registry: registry, BotID: 1, TaskID: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gateway.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "pi-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gateway.URL, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"computer_request_takeover", "routine_report", "routine_manage", "routine_list", "routine_create", "skill_read", "conversation_read"} {
		if !slices.Contains(names, want) {
			t.Errorf("served tools %q lack %s", names, want)
		}
	}
}
