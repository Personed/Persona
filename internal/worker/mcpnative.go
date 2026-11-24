package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// nativeProgram is the default program each native Harness provides.
var nativeProgram = map[string]string{"claude": "claude", "codex": "codex", "pi": "pi", "opencode": "opencode"}

// nativeOutputLimit bounds the catalog output, as the TypeScript worker does.
const nativeOutputLimit = 1 << 20

var errNativeOutputLarge = errors.New("native MCP catalog is too large")

// nativeMCP lists the connections the Bot's native Harness configures on this
// work computer by running its "mcp list" command under the account
// environment, as packages/agent/src/harness/native-mcp.mjs does.
func nativeMCP(ctx context.Context, engine *MCPConfig, cwd string) (*MCPResult, error) {
	if engine == nil {
		return nil, codeError("EINVAL", "native MCP engine is missing", "")
	}
	program, known := nativeProgram[engine.Provider]
	if !known {
		return nil, codeError("EINVAL", "此 ACP 程序未提供原生 MCP 目录接口。", "")
	}
	command := engine.Command
	if command == "" {
		command = program
	}
	env := AccountEnvironment(os.Environ(), "", nil)
	path, err := ResolveProgram(ctx, command, env)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, codeError("ENOENT", "工作电脑未安装所选 Harness。", command)
	}
	args := []string{"mcp", "list"}
	jsonOutput := engine.Provider == "codex" || engine.Provider == "pi"
	if jsonOutput {
		args = append(args, "--json")
	}
	run, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(run, path, args...)
	cmd.Env, cmd.Stderr = env, io.Discard
	cmd.Dir = cwd
	cmd.Stdout = &nativeOutputWriter{buf: &out, limit: nativeOutputLimit}
	err = cmd.Run()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, errNativeOutputLarge):
		return nil, codeError("EFBIG", "原生 MCP 目录过大。", "")
	case run.Err() != nil:
		return nil, codeError("timeout", "原生 MCP 目录读取超时。", "")
	case err != nil && out.Len() == 0:
		return nil, codeError("MCP_FAILED", "原生 MCP 目录不可用。", "")
	}
	items, err := parseNativeCatalog(engine.Provider, jsonOutput, out.String())
	if err != nil {
		return nil, err
	}
	inheritance := "native"
	if engine.Provider == "claude" {
		inheritance = "selectable"
	}
	return &MCPResult{Items: items, Inheritance: inheritance, Readonly: true}, nil
}

type nativeOutputWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *nativeOutputWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		return 0, errNativeOutputLarge
	}
	return w.buf.Write(p)
}

var (
	vtControl    = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\)|[@-Z\\-_])`)
	claudeLine   = regexp.MustCompile(`^([\w.-]+):\s`)
	opencodeLine = regexp.MustCompile(`[●○]\s+([\w.-]+)\s+(connected|disabled|failed|needs_auth)`)
)

// parseNativeCatalog reads the "mcp list" output into catalog items and drops
// the Persona entry.
func parseNativeCatalog(provider string, jsonOutput bool, output string) ([]MCPNativeItem, error) {
	source := provider + " 用户/项目配置"
	var items []MCPNativeItem
	if jsonOutput {
		var raw any
		if err := json.Unmarshal([]byte(output), &raw); err != nil {
			return nil, codeError("EINVAL", "原生 MCP 目录不是有效的 JSON。", "")
		}
		var entries []map[string]any
		switch value := raw.(type) {
		case []any:
			entries = toMaps(value)
		case map[string]any:
			if servers, ok := value["servers"].([]any); ok {
				entries = toMaps(servers)
			}
		}
		for _, entry := range entries {
			name, _ := entry["name"].(string)
			item := MCPNativeItem{Name: name, Source: source, Enabled: true, Status: "unknown"}
			if s, ok := entry["source"].(string); ok {
				item.Source = s
			}
			if enabled, ok := entry["enabled"].(bool); ok {
				item.Enabled = enabled
			}
			if status, ok := entry["state"].(string); ok {
				item.Status = status
			}
			items = append(items, item)
		}
	} else {
		text := vtControl.ReplaceAllString(output, "")
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimRight(line, "\r")
			if provider == "claude" {
				if m := claudeLine.FindStringSubmatch(line); m != nil {
					items = append(items, MCPNativeItem{Name: m[1], Source: source, Enabled: true, Status: "unknown"})
				}
				continue
			}
			if m := opencodeLine.FindStringSubmatch(line); m != nil {
				items = append(items, MCPNativeItem{Name: m[1], Source: source, Enabled: m[2] != "disabled", Status: m[2]})
			}
		}
	}
	kept := items[:0]
	for _, item := range items {
		if item.Name != "persona" {
			kept = append(kept, item)
		}
	}
	return kept, nil
}

func toMaps(values []any) []map[string]any {
	var out []map[string]any
	for _, value := range values {
		if m, ok := value.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
