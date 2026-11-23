package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/hosting"
	"github.com/Personed/Persona/internal/mcp"
	"github.com/Personed/Persona/internal/memory"
	"github.com/Personed/Persona/internal/routines"
	"github.com/Personed/Persona/internal/skills"
	"github.com/Personed/Persona/internal/store"
)

// memoryCredentials is an in-memory hosting.Credentials.
type memoryCredentials map[hosting.Ref]hosting.Credential

func (m memoryCredentials) Get(_ context.Context, ref hosting.Ref) (hosting.Credential, error) {
	c, ok := m[ref]
	if !ok {
		return hosting.Credential{}, hosting.ErrNotFound
	}
	return c, nil
}
func (m memoryCredentials) Put(_ context.Context, c hosting.Credential) error {
	m[c.Ref] = c
	return nil
}
func (m memoryCredentials) Delete(_ context.Context, ref hosting.Ref) error {
	delete(m, ref)
	return nil
}
func (m memoryCredentials) List(context.Context, string, string) ([]hosting.Credential, error) {
	return nil, nil
}

// fakeRunner answers every inspection with one tool.
type fakeRunner struct{ ops []mcp.Operation }

func (r *fakeRunner) Run(_ context.Context, _ string, op mcp.Operation) (mcp.Outcome, error) {
	r.ops = append(r.ops, op)
	return mcp.Outcome{Tools: json.RawMessage(`[{"name":"search"}]`), Capabilities: json.RawMessage(`{}`)}, nil
}

type workFixture struct {
	h           http.Handler
	records     store.Store
	credentials memoryCredentials
	runner      *fakeRunner
}

func workService(t *testing.T) workFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	records, err := store.Open(ctx, filepath.Join(dir, "persona-go.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = records.Close() })
	a, err := agent.Open(ctx, records, agent.Options{Models: fauxModels{}, FilesRoot: filepath.Join(dir, "files")})
	if err != nil {
		t.Fatal(err)
	}
	f := workFixture{records: records, credentials: memoryCredentials{}, runner: &fakeRunner{}}
	procedures := newRPC(t)
	RegisterAgent(procedures, a, nil)
	packages := skills.NewService(records, os.DirFS(filepath.Join(dir, "files", "skills")), nil)
	packages.UsePackageDir(filepath.Join(dir, "files", "skills"))
	RegisterWork(procedures, a, nil, Work{Memory: memory.NewService(records, nil), Routines: routines.NewService(records, nil),
		Skills: packages})
	connections := mcp.NewService(records, f.credentials, nil)
	connections.Runner = f.runner
	RegisterMCP(procedures, a, connections, "")
	if f.h, err = New(Config{RPC: procedures}); err != nil {
		t.Fatal(err)
	}
	return f
}

type exchange struct {
	path, input string
	status      int
	body        string
}

func (f workFixture) run(t *testing.T, cases []exchange) {
	t.Helper()
	for _, c := range cases {
		status, body := call(t, f.h, c.path, c.input)
		if status != c.status || !strings.Contains(body, c.body) {
			t.Errorf("%s %s = %d %s, want %d %s", c.path, c.input, status, body, c.status, c.body)
		}
	}
}

func uuidN(n string) string { return `"00000000-0000-4000-8000-00000000000` + n + `"` }

func TestWorkProcedures(t *testing.T) {
	f := workService(t)
	bot := rootBot(t, f.h)
	b := `{"botId":` + bot
	skill := `{"items":[{"id":"skill-1","name":"weekly-report","description":"Weekly.","content":"---\nname: weekly-report\ndescription: Weekly.\n---\nBody\n","revision":1,"enabledBots":[],"source":"user","requestId":"r"}]}`
	id, err := f.records.MintID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	meta := store.Document{ID: store.DocumentID(id), Kind: skills.DocKind, Scope: store.Scope{Kind: "session"}}
	if _, _, err := f.records.Commit(context.Background(), []store.Batch{{{Type: store.WriteDocumentCreate, Document: &meta, Value: json.RawMessage(skill)}}}); err != nil {
		t.Fatal(err)
	}
	routine := b + `,"id":"routine-1","revision":0,"title":" Daily ","instructions":"Summarize.","schedule":{"type":"cron","expression":"0 9 * * *","timeZone":"UTC"},"skillIds":[],"requestId":` + uuidN("4") + `}`
	f.run(t, []exchange{
		{"work.get", `{"botId":999}`, 404, `"NOT_FOUND"`},
		{"work.prompt", b + `}`, 501, `"NOT_IMPLEMENTED"`},
		{"work.memory.save", b + `,"id":"m1","revision":0,"text":"Likes tea","category":"profile","shared":false,"requestId":` + uuidN("1") + `}`, 200, `"revision":1`},
		{"work.memory.save", b + `,"id":"m1","revision":0,"text":"Likes tea","shared":false,"requestId":` + uuidN("1") + `}`, 200, `"text":"Likes tea"`},
		{"work.memory.save", b + `,"id":"m1","revision":0,"text":"Likes coffee","shared":false,"requestId":` + uuidN("2") + `}`, 409, `"CONFLICT"`},
		{"work.get", b + `}`, 200, `"memoryUsage":{"profile":`},
		{"work.memory.delete", b + `,"id":"m1","revision":1}`, 200, `{"json":{"ok":true}}`},
		{"work.memory.review.setEnabled", b + `,"enabled":false}`, 200, `{"json":{"enabled":false}}`},
		{"work.get", b + `}`, 200, `"memoryReviewEnabled":false`},
		{"work.routines.save", routine, 200, `"title":"Daily"`},
		{"work.routines.save", routine, 200, `"enabled":false`},
		{"work.routines.enable", b + `,"id":"routine-1","revision":1,"enabled":true,"requestId":` + uuidN("5") + `}`, 200, `{"json":{"ok":true}}`},
		{"work.get", b + `}`, 200, `"enabled":true`},
		{"work.routines.enable", b + `,"id":"missing","revision":1,"enabled":true,"requestId":` + uuidN("6") + `}`, 404, `定时任务不存在。`},
		{"work.routines.test", b + `,"id":"routine-1","requestId":` + uuidN("7") + `}`, 501, `"NOT_IMPLEMENTED"`},
		{"work.routines.delete", b + `,"id":"routine-1","revision":1}`, 409, `"CONFLICT"`},
		{"work.routines.delete", b + `,"id":"routine-1","revision":2}`, 200, `{"json":{"ok":true}}`},
		{"work.routines.delete", b + `,"id":"routine-1","revision":2}`, 200, `{"json":{"ok":true}}`},
		{"work.skills.enable", b + `,"id":"skill-1","revision":1,"enabled":true,"requestId":` + uuidN("8") + `}`, 200, `{"json":{"ok":true}}`},
		{"work.get", b + `}`, 200, `"enabledBots":[` + bot + `]`},
		{"work.skills.enable", b + `,"id":"missing","revision":1,"enabled":true,"requestId":` + uuidN("9") + `}`, 404, `技能不存在。`},
		{"work.skills.export", b + `,"id":"skill-1","hash":"` + strings.Repeat("a", 64) + `"}`, 404, `技能版本不存在。`},
		{"work.skills.save", b + `,"id":"s","content":"x","revision":0,"requestId":` + uuidN("3") + `}`, 400, `SKILL.md需要YAML前置信息。`},
		{"work.skills.save", b + `,"id":"s","content":"---\nname: notes\ndescription: Notes.\n---\nBody\n","revision":0,"requestId":` + uuidN("3") + `}`, 200, `"name":"notes"`},
		{"work.skills.save", b + `,"id":"s","content":"---\nname: notes\ndescription: Notes.\n---\nBody\n","revision":0,"requestId":` + uuidN("3") + `}`, 200, `"revision":1`},
		{"work.skills.preview", b + `,"id":"s","content":"---\nname: notes\ndescription: Notes.\n---\nMore\n"}`, 200, `"status":"changed"`},
		{"work.skills.save", b + `,"id":"s","content":"---\nname: notes\ndescription: Notes.\n---\nBody\n","revision":0,"requestId":` + uuidN("7") + `}`, 409, `"CONFLICT"`},
		{"work.skills.delete", b + `,"id":"skill-1","revision":1}`, 409, `"CONFLICT"`},
	})
	status, body := call(t, f.h, "work.skills.export", b+`,"id":"skill-1"}`)
	var export struct {
		JSON skills.Export `json:"json"`
	}
	if err := json.Unmarshal([]byte(body), &export); status != 200 || err != nil || export.JSON.Name != "weekly-report" {
		t.Fatalf("work.skills.export = %d %s", status, body)
	}
	if _, err := base64.StdEncoding.DecodeString(export.JSON.Archive); err != nil {
		t.Errorf("archive: %v", err)
	}
	f.run(t, []exchange{{"work.skills.delete", b + `,"id":"skill-1","revision":2}`, 200, `{"json":{"ok":true}}`}})
}

func TestMCPProcedures(t *testing.T) {
	f := workService(t)
	bot := rootBot(t, f.h)
	const id = `"11111111-1111-4111-8111-111111111111"`
	f.run(t, []exchange{
		{"mcp.list", `{}`, 200, `{"json":[]}`},
		{"mcp.save", `{"id":` + id + `,"revision":0,"name":"Persona","computerId":"local","enabled":true,"config":{"transport":"http","url":"https://example.com/mcp"}}`, 400, `此名称已保留。`},
		{"mcp.save", `{"id":` + id + `,"revision":0,"name":"Docs","computerId":"local","enabled":true}`, 400, `请填写连接配置。`},
		{"mcp.save", `{"id":` + id + `,"revision":0,"name":"Docs","computerId":"local","enabled":true,"config":{"transport":"http","url":"https://user:pw@example.com/mcp"}}`, 400, `MCP 地址无效。`},
		{"mcp.save", `{"id":` + id + `,"revision":0,"name":"Docs","computerId":"local","enabled":true,"config":{"transport":"http","url":"https://example.com/mcp"}}`, 200, `"status":"configured"`},
		{"mcp.grant", `{"id":` + id + `,"revision":1,"grants":[{"botId":999,"tools":null}]}`, 404, `"NOT_FOUND"`},
		{"mcp.grant", `{"id":` + id + `,"revision":1,"grants":[{"botId":` + bot + `,"tools":null},{"botId":` + bot + `,"tools":[]}]}`, 400, `Bot 权限重复。`},
		{"mcp.grant", `{"id":` + id + `,"revision":1,"grants":[{"botId":` + bot + `,"tools":["a","a"]}]}`, 200, `"tools":["a"]`},
		{"mcp.check", `{"id":` + id + `}`, 200, `"status":"ready"`},
		{"mcp.login", `{"id":` + id + `}`, 503, `此部署尚未提供 OAuth 回调地址。`},
		{"mcp.native", `{"botId":` + bot + `}`, 503, `"SERVICE_UNAVAILABLE"`},
		{"mcp.logout", `{"id":` + id + `}`, 200, ``},
		{"mcp.list", `{}`, 200, `"revision":3`},
		{"mcp.remove", `{"id":` + id + `,"revision":2}`, 409, `"CONFLICT"`},
		{"mcp.remove", `{"id":` + id + `,"revision":3}`, 200, ``},
		{"mcp.check", `{"id":` + id + `}`, 404, `连接不存在。`},
	})
	if _, body := call(t, f.h, "mcp.list", `{}`); strings.Contains(body, "credential") {
		t.Errorf("mcp.list exposes the credential reference: %s", body)
	}
	if len(f.credentials) != 0 {
		t.Errorf("credentials left after remove: %v", f.credentials)
	}
	if len(f.runner.ops) != 1 || f.runner.ops[0].Action != "inspect" || !strings.Contains(string(f.runner.ops[0].Config), "example.com") {
		t.Errorf("runner operations = %+v", f.runner.ops)
	}
}
