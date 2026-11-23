package agent

// EngineCapabilities describes what a native Harness supports.
type EngineCapabilities struct {
	Approvals bool `json:"approvals"`
	Models    bool `json:"models"`
	Recovery  bool `json:"recovery"`
}

// Engine is one item of engines.catalog.
type Engine struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Capabilities EngineCapabilities `json:"capabilities"`
}

// EngineCatalog is engineCatalog in packages/agent/src/runtime, in provider order.
func EngineCatalog() []Engine {
	full := EngineCapabilities{Approvals: true, Models: true, Recovery: true}
	return []Engine{
		{ID: "pi", Name: "Pi", Capabilities: full},
		{ID: "codex", Name: "Codex", Capabilities: full},
		{ID: "claude", Name: "Claude Code", Capabilities: full},
		{ID: "opencode", Name: "OpenCode", Capabilities: full},
		{ID: "acp", Name: "自定义 ACP", Capabilities: EngineCapabilities{Approvals: true, Models: true}},
	}
}

// MCPApp is one managed MCP application of the catalog.
type MCPApp struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Icon          string    `json:"icon"`
	Category      string    `json:"category"`
	Description   string    `json:"description"`
	Preparation   string    `json:"preparation"`
	Documentation string    `json:"documentation"`
	Config        MCPConfig `json:"config"`
}

// MCPConfig is the HTTP transport of an MCP application.
type MCPConfig struct {
	Transport string            `json:"transport"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
}

// MCPApps is mcpApps in packages/agent/src/mcp/catalog.ts.
func MCPApps() []MCPApp {
	return []MCPApp{
		{
			ID: "microsoft-learn", Name: "Microsoft Learn", Icon: "BookOpen", Category: "资料",
			Description:   "搜索 Microsoft 官方文档和代码示例。",
			Preparation:   "无需登录。需要网络连接。",
			Documentation: "https://learn.microsoft.com/training/support/mcp",
			Config:        MCPConfig{Transport: "http", URL: "https://learn.microsoft.com/api/mcp", Headers: map[string]string{}},
		},
		{
			ID: "context7", Name: "Context7", Icon: "Code", Category: "开发",
			Description:   "查询软件库的最新文档。",
			Preparation:   "可直接测试。服务有使用额度；可选登录或填写自己的 Key。",
			Documentation: "https://context7.com/docs/resources/all-clients",
			Config:        MCPConfig{Transport: "http", URL: "https://mcp.context7.com/mcp", Headers: map[string]string{}},
		},
	}
}

// Work is the work.get answer for a Bot. Memory, skills and routines are
// served by their own packages once the runtime composes them; until then a
// Bot reads as having none, with memory review on.
func Work() map[string]any {
	return map[string]any{
		"memory":              []any{},
		"memoryUsage":         map[string]int{"profile": 0, "work": 0},
		"memoryReviewEnabled": true,
		"skills":              []any{},
		"routines":            map[string]any{"items": []any{}, "runs": []any{}},
	}
}
