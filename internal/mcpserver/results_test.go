package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Personed/Persona/internal/mcpserver"
	"github.com/Personed/Persona/internal/tools"
)

// resultTool returns a fixed result and counts its runs.
type resultTool struct {
	name   string
	result tools.Result
	runs   *int
}

func (r resultTool) Definition() tools.Definition {
	return tools.Definition{Name: r.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (r resultTool) Run(context.Context, tools.Call) (tools.Result, error) {
	*r.runs++
	return r.result, nil
}

func texts(result *mcp.CallToolResult) []string {
	var parts []string
	for _, content := range result.Content {
		if part, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, part.Text)
		}
	}
	return parts
}

func TestGatewayBoundsResultsAndAnswersQueries(t *testing.T) {
	ctx := context.Background()
	type app struct {
		Name string `json:"name"`
		Path string `json:"launch_path"`
		Note string `json:"note"`
	}
	apps := make([]app, 400)
	for index := range apps {
		apps[index] = app{Name: fmt.Sprintf("App %d", index), Path: fmt.Sprintf("/Applications/%d.app", index), Note: "音乐 <b>"}
	}
	apps[321].Name = "Apple Music"
	listing, err := json.Marshal(map[string]any{"apps": apps, "total": len(apps)})
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.Repeat("window > button\n", 40)
	snapshot, err := json.Marshal(map[string]any{"snapshot_id": "s1", "elements": []int{1, 2}, "tree": tree, "pid": 7})
	if err != nil {
		t.Fatal(err)
	}
	prose := strings.Repeat("日志", mcpserver.PreviewCharacters)
	runs := 0
	_, session := open(t, nil,
		resultTool{"list_apps", tools.Result{Content: []tools.Content{{Type: "text", Text: string(listing)}}}, &runs},
		resultTool{"snapshot", tools.Result{Content: []tools.Content{{Type: "text", Text: tree}}, Structured: snapshot}, &runs},
		resultTool{"read_log", tools.Result{Content: []tools.Content{{Type: "text", Text: prose}}}, &runs},
	)
	call := func(name string, arguments map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	query := func(handle, expression string) *mcp.CallToolResult {
		t.Helper()
		return call("query_result", map[string]any{"result": handle, "query": expression})
	}

	// A large JSON result becomes a parseable preview of whole records plus a notice.
	listed := call("list_apps", nil)
	parts := texts(listed)
	if listed.IsError || len(parts) != 2 || utf8.RuneCountInString(parts[0]) > mcpserver.PreviewCharacters {
		t.Fatalf("list_apps = %d parts, error %v", len(parts), listed.IsError)
	}
	var preview struct {
		Apps  []app `json:"apps"`
		Total int   `json:"total"`
	}
	if err := json.Unmarshal([]byte(parts[0]), &preview); err != nil || preview.Total != 400 || len(preview.Apps) == 0 || len(preview.Apps) >= 400 || preview.Apps[0] != apps[0] {
		t.Fatalf("preview = %d apps, total %d, %v", len(preview.Apps), preview.Total, err)
	}
	notice := regexp.MustCompile(`^Partial result\. Full records are held as (r\d+)\. Shape: apps: 400 items; fields: launch_path, name, note\. Example: (.+)\. Use query_result \{"result":"r\d+","query":"<JMESPath>"\}\.`).FindStringSubmatch(parts[1])
	if notice == nil {
		t.Fatalf("notice = %s", parts[1])
	}
	handle := notice[1]
	if listed.Meta[mcpserver.ResultHandleMeta] != handle {
		t.Fatalf("meta = %+v, handle %s", listed.Meta, handle)
	}

	// Queries read the complete value without running the tool again.
	for expression, want := range map[string]string{
		notice[2]: `[{"launch_path":"/Applications/0.app","name":"App 0"},`,
		"apps[?contains(name, 'Music')].{name:name, path:launch_path}": `[{"name":"Apple Music","path":"/Applications/321.app"}]`,
		" apps[399].note ": `"音乐 <b>"`,
		"length(apps)":     `400`,
		"missing":          `null`,
	} {
		if result := query(handle, expression); result.IsError || !strings.HasPrefix(texts(result)[0], want) {
			t.Errorf("query %q = %v, error %v", expression, texts(result), result.IsError)
		}
	}
	if again := query(handle, "apps[].launch_path"); len(texts(again)) != 2 || !strings.Contains(texts(again)[1], "Shape: @: 400 items; fields: . Example: [0:20]. ") {
		t.Errorf("large query answer = %v", texts(again)[1:])
	}
	if runs != 1 {
		t.Fatalf("list_apps ran %d times", runs)
	}
	for _, expression := range []string{"apps[?", "unknown_function(apps)"} {
		invalid := query(handle, expression)
		if !invalid.IsError || !strings.HasPrefix(texts(invalid)[0], "Invalid JMESPath: ") || !strings.HasSuffix(texts(invalid)[0], "Shape: apps: 400 items; fields: launch_path, name, note. Example: "+notice[2]) {
			t.Errorf("query %q = %v", expression, texts(invalid))
		}
	}
	for _, arguments := range []map[string]any{{"result": handle}, {"result": handle, "query": " "}, {"result": handle, "query": "@", "limit": 1}, {"result": 1, "query": "@"}} {
		if invalid := call("query_result", arguments); !invalid.IsError || texts(invalid)[0] != "参数无效：需要 result 和 query。" {
			t.Errorf("query_result %v = %v", arguments, texts(invalid))
		}
	}

	// Structured content stays whole. Its text copy drops what the result text carries.
	shot := call("snapshot", nil)
	structured, _ := shot.StructuredContent.(map[string]any)
	if elements, _ := structured["elements"].([]any); structured["tree"] != tree || len(elements) != 2 {
		t.Fatalf("structured content = %v", structured)
	}
	if parts := texts(shot); len(parts) != 2 || parts[0] != tree || parts[1] != `{"pid":7,"snapshot_id":"s1","tree":"[see result text]"}` || shot.Meta != nil {
		t.Fatalf("snapshot text = %q, meta %v", parts, shot.Meta)
	}

	// Text that is not JSON is cut on a character boundary and stays queryable.
	logged := call("read_log", nil)
	parts = texts(logged)
	if len(parts) != 2 || parts[0] != prose[:len(prose)/2] || !strings.Contains(parts[1], "Shape: type: string; keys: . Example: @. ") {
		t.Fatalf("read_log = %d parts, %d bytes", len(parts), len(parts[0]))
	}
	logHandle, _ := logged.Meta[mcpserver.ResultHandleMeta].(string)
	if whole := query(logHandle, "length(@)"); whole.IsError || texts(whole)[0] != fmt.Sprint(2*mcpserver.PreviewCharacters) {
		t.Fatalf("length of log = %v", texts(whole))
	}

	// Only the latest results stay held.
	for range mcpserver.HeldResults {
		call("snapshot", nil)
	}
	if expired := query(handle, "total"); !expired.IsError || texts(expired)[0] != "结果已失效。请读取原始工作记录。" {
		t.Fatalf("expired handle = %v", texts(expired))
	}
}
