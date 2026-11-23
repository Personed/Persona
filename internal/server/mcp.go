package server

import (
	"context"
	"encoding/json"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/mcp"
	"github.com/Personed/Persona/internal/rpc"
)

// RegisterMCP registers the managed MCP connection procedures that
// RegisterAgent does not, as mcpRouter in apps/api/src/mcp.ts. callback is the
// OAuth redirect URL; empty refuses mcp.login as a deployment without one.
func RegisterMCP(s *rpc.Server, a *agent.Agent, connections *mcp.Service, callback string) {
	r := mcpRoutes{agentRoutes: agentRoutes{a: a}, c: connections, callback: callback}
	s.Handle("mcp.list", r.access(func(ctx context.Context, _ json.RawMessage) (any, error) { return r.c.List(ctx) }))
	s.Handle("mcp.native", r.access(r.native))
	s.Handle("mcp.save", r.access(r.save))
	s.Handle("mcp.grant", r.access(r.grant))
	s.Handle("mcp.remove", r.access(r.remove))
	s.Handle("mcp.check", r.access(r.check))
	s.Handle("mcp.login", r.access(r.login))
	s.Handle("mcp.logout", r.access(r.logout))
}

type mcpRoutes struct {
	agentRoutes
	c        *mcp.Service
	callback string
}

func (r mcpRoutes) access(h rpc.Unary) rpc.Unary {
	return func(ctx context.Context, input json.RawMessage) (any, error) {
		if r.c == nil {
			return nil, rpc.NewError("SERVICE_UNAVAILABLE", "连接服务不可用。")
		}
		out, err := h(ctx, input)
		if err != nil {
			return nil, workError(err)
		}
		return out, nil
	}
}

func (r mcpRoutes) native(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	if r.c.Catalog == nil {
		return nil, rpc.NewError("SERVICE_UNAVAILABLE", "")
	}
	catalog, err := r.c.Catalog(ctx, id)
	if err == nil {
		catalog.Inherited, err = r.c.Native(ctx, id, nil)
	}
	if err != nil {
		return nil, rpc.NewError("BAD_REQUEST", err.Error())
	}
	if in.Enabled != nil {
		if catalog.Inheritance != "selectable" {
			return nil, rpc.NewError("BAD_REQUEST", "此 Harness 使用原生继承范围。请在原生程序管理连接。")
		}
		if catalog.Inherited, err = r.c.Native(ctx, id, in.Enabled); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

func (r mcpRoutes) save(ctx context.Context, input json.RawMessage) (any, error) {
	var in mcp.SaveInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	return r.c.Save(ctx, in)
}

func (r mcpRoutes) grant(ctx context.Context, input json.RawMessage) (any, error) {
	var in struct {
		ID       string      `json:"id"`
		Revision int         `json:"revision"`
		Grants   []mcp.Grant `json:"grants"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	for _, grant := range in.Grants {
		exists, err := r.a.Bots.Exists(ctx, grant.BotID)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, rpc.NewError("NOT_FOUND", "")
		}
	}
	return r.c.Grant(ctx, in.ID, in.Revision, in.Grants)
}

func (r mcpRoutes) remove(ctx context.Context, input json.RawMessage) (any, error) {
	var in struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	return rpc.Void, r.c.Remove(ctx, in.ID, in.Revision)
}

func (r mcpRoutes) check(ctx context.Context, input json.RawMessage) (any, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	return r.c.Check(ctx, in.ID)
}

func (r mcpRoutes) login(ctx context.Context, input json.RawMessage) (any, error) {
	if r.callback == "" {
		return nil, rpc.NewError("SERVICE_UNAVAILABLE", "此部署尚未提供 OAuth 回调地址。")
	}
	var in struct {
		ID     string      `json:"id"`
		Client *mcp.Client `json:"client"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	url, err := r.c.Login(ctx, in.ID, r.callback, in.Client)
	if err != nil {
		return nil, err
	}
	return map[string]*string{"url": url}, nil
}

func (r mcpRoutes) logout(ctx context.Context, input json.RawMessage) (any, error) {
	var in struct {
		ID     string `json:"id"`
		Cancel bool   `json:"cancel"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	return rpc.Void, r.c.Logout(ctx, in.ID, in.Cancel)
}
