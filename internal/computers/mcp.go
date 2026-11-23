package computers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/mcp"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/worker"
)

// MCP runs one managed MCP operation in a fresh worker on the computer, as
// catalog.ts mcp and mcpProcess: a dedicated computer runs it for its Bot,
// any other for botID or else the first Bot bound to it.
func (c *Catalog) MCP(ctx context.Context, computerID string, botID int64, input worker.MCPInput) (worker.MCPResult, error) {
	doc, _, err := agent.Snapshot(ctx, c.h, ComputersDoc, 0)
	if err != nil {
		return worker.MCPResult{}, err
	}
	item := requireComputer(doc.Items, computerID)
	if item == nil {
		return worker.MCPResult{}, rpc.NewError("NOT_FOUND", "电脑不存在。")
	}
	if !item.Authorized {
		return worker.MCPResult{}, rpc.NewError("BAD_REQUEST", "这台电脑的授权已撤销。")
	}
	if item.Scope != nil {
		if err := c.ensureReady(ctx, computerID); err != nil {
			return worker.MCPResult{}, err
		}
	}
	dedicated := item.Scope != nil && item.Scope.Kind == "dedicated"
	if dedicated && botID != 0 && item.Scope.BotID != botID {
		return worker.MCPResult{}, rpc.NewError("BAD_REQUEST", "此电脑属于其他 Bot。")
	}
	switch {
	case dedicated:
		botID = item.Scope.BotID
	case botID == 0:
		botID = boundBot(doc, computerID)
	}
	if botID == 0 {
		return worker.MCPResult{}, rpc.NewError("BAD_REQUEST", "请先为此电脑分配 Bot，再测试 MCP。")
	}
	transport, _, err := c.target(*item).transport(botID)
	if err != nil {
		return worker.MCPResult{}, err
	}
	client, err := worker.Dial(ctx, transport, worker.Options{})
	if err != nil {
		return worker.MCPResult{}, err
	}
	defer func() { _ = client.Close() }()
	return client.MCP(ctx, input, nil)
}

// boundBot is the lowest Bot ID bound to the computer, or 0. The TypeScript
// service takes the first binding in insertion order, which numeric keys
// keep ascending.
func boundBot(doc Doc, computerID string) int64 {
	var found int64
	for key, binding := range doc.Bindings {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id <= 0 || binding.ComputerID == nil || *binding.ComputerID != computerID {
			continue
		}
		if found == 0 || id < found {
			found = id
		}
	}
	return found
}

// MCPRunner returns the mcp.Runner that runs managed connection checks and
// sign-ins on their work computers.
func (c *Catalog) MCPRunner() mcp.Runner { return mcpRunner{c} }

type mcpRunner struct{ c *Catalog }

func (r mcpRunner) Run(ctx context.Context, computerID string, op mcp.Operation) (mcp.Outcome, error) {
	raw, err := json.Marshal(op)
	if err != nil {
		return mcp.Outcome{}, err
	}
	var input worker.MCPInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return mcp.Outcome{}, err
	}
	result, err := r.c.MCP(ctx, computerID, 0, input)
	var wire *worker.Error
	if errors.As(err, &wire) && wire.Code == "MCP_AUTH" {
		return mcp.Outcome{}, fmt.Errorf("%w: %s", mcp.ErrAuthorization, wire.Message)
	}
	if err != nil {
		return mcp.Outcome{}, err
	}
	var out mcp.Outcome
	if raw, err = json.Marshal(result); err == nil {
		err = json.Unmarshal(raw, &out)
	}
	out.Tools, out.Capabilities, out.OAuth = present(out.Tools), present(out.Capabilities), present(out.OAuth)
	return out, err
}

// present drops a JSON null, which the TypeScript worker left undefined.
func present(raw json.RawMessage) json.RawMessage {
	if string(raw) == "null" {
		return nil
	}
	return raw
}
