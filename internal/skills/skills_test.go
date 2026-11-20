package skills_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Personed/Persona/internal/computer"
	"github.com/Personed/Persona/internal/skills"
	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/store/storetest"
	"github.com/Personed/Persona/internal/tools"
)

const (
	skillContent = "---\nname: weekly-report\ndescription: Write the weekly report.\n---\nRun scripts/report.py.\n"
	hash         = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// target records writes to one work computer.
type target struct {
	computer.Target
	os    string
	files map[string]string
}

func (t *target) Info(context.Context) (computer.Info, error) {
	return computer.Info{OS: t.os}, nil
}

func (t *target) WriteFile(_ context.Context, path string, data io.Reader) error {
	content, err := io.ReadAll(data)
	t.files[path] = string(content)
	return err
}

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func seed(t *testing.T, items ...skills.Skill) *storetest.Store {
	t.Helper()
	ctx := context.Background()
	records := storetest.New()
	id, err := records.MintID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	meta := store.Document{ID: store.DocumentID(id), Kind: skills.DocKind, Scope: store.Scope{Kind: "session"}}
	if _, _, err := records.Commit(ctx, []store.Batch{{{Type: store.WriteDocumentCreate, Document: &meta, Value: value}}}); err != nil {
		t.Fatal(err)
	}
	return records
}

func call(t *testing.T, service *skills.Service, botID int64, input string) (tools.Result, string) {
	t.Helper()
	registry := tools.NewRegistry()
	if err := skills.Register(registry, service); err != nil {
		t.Fatal(err)
	}
	// No approver: a read-only tool must not ask for approval.
	result, err := registry.Call(context.Background(), tools.Call{BotID: botID, Name: "skill_read", Input: json.RawMessage(input)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("content = %+v", result.Content)
	}
	return result, result.Content[0].Text
}

func TestSkillReadDefinition(t *testing.T) {
	registry := tools.NewRegistry()
	if err := skills.Register(registry, skills.NewService(storetest.New(), nil, nil)); err != nil {
		t.Fatal(err)
	}
	definitions, err := registry.List(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].Name != "skill_read" || definitions[0].Mutates {
		t.Fatalf("definitions = %+v", definitions)
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(definitions[0].InputSchema, &schema); err != nil || len(schema.Required) != 1 || schema.Required[0] != "id" {
		t.Fatalf("schema = %s, %v", definitions[0].InputSchema, err)
	}
}

func TestSkillReadEnabledOnly(t *testing.T) {
	skill := skills.Skill{ID: "s1", Name: "weekly-report", Description: "Write the weekly report.", Content: skillContent, Revision: 2, EnabledBots: []int64{1}, Source: "user", RequestID: "r1"}
	service := skills.NewService(seed(t, skill), nil, nil)

	result, text := call(t, service, 1, `{"id":"s1"}`)
	var loaded skills.Loaded
	if err := json.Unmarshal([]byte(text), &loaded); err != nil {
		t.Fatal(err)
	}
	if result.IsError || loaded.Content != skillContent || loaded.Revision != 2 || loaded.Path != "" || strings.Contains(text, `"path"`) || strings.Contains(text, `"hash"`) {
		t.Fatalf("result = %+v", result)
	}

	for name, test := range map[string]struct {
		botID int64
		input string
		want  string
	}{
		"other Bot":  {2, `{"id":"s1"}`, "技能已删除或未为此Bot启用。"},
		"missing":    {1, `{"id":"s2"}`, "技能已删除或未为此Bot启用。"},
		"no id":      {1, `{}`, "参数无效：需要 id。"},
		"wrong type": {1, `{"id":1}`, "参数无效：需要 id。"},
	} {
		if result, text := call(t, service, test.botID, test.input); !result.IsError || text != test.want {
			t.Errorf("%s: result = %+v", name, result)
		}
	}

	if result, _ := call(t, skills.NewService(storetest.New(), nil, nil), 1, `{"id":"s1"}`); !result.IsError {
		t.Fatalf("empty catalog: result = %+v", result)
	}
}

func TestSkillReadMaterializesPackage(t *testing.T) {
	skill := skills.Skill{ID: "s1", Name: "weekly-report", Content: skillContent, EnabledBots: []int64{1}, Hash: hash}
	packages := fstest.MapFS{hash + ".zip": {Data: archive(t, map[string]string{
		"SKILL.md":          skillContent,
		"scripts/":          "",
		"scripts/report.py": "print('ok')\n",
	})}}
	for goos, want := range map[string][2]string{
		"linux":   {"/work/bot/.persona/skills/" + hash + "/weekly-report/SKILL.md", "/work/bot/.persona/skills/" + hash + "/weekly-report/scripts/report.py"},
		"windows": {`C:\work\bot\.persona\skills\` + hash + `\weekly-report\SKILL.md`, `C:\work\bot\.persona\skills\` + hash + `\weekly-report\scripts\report.py`},
	} {
		workDir := "/work/bot/"
		if goos == "windows" {
			workDir = `C:\work\bot`
		}
		work := &target{os: goos, files: map[string]string{}}
		service := skills.NewService(seed(t, skill), packages, func(_ context.Context, botID int64) (computer.Target, string, error) {
			if botID != 1 {
				t.Errorf("workspace Bot = %d", botID)
			}
			return work, workDir, nil
		})
		result, text := call(t, service, 1, `{"id":"s1"}`)
		var loaded skills.Loaded
		if err := json.Unmarshal([]byte(text), &loaded); err != nil {
			t.Fatalf("%s: %s: %v", goos, text, err)
		}
		if result.IsError || loaded.Path != want[0] || loaded.Hash != hash {
			t.Fatalf("%s: loaded = %+v", goos, loaded)
		}
		if len(work.files) != 2 || work.files[want[0]] != skillContent || work.files[want[1]] != "print('ok')\n" {
			t.Fatalf("%s: files = %+v", goos, work.files)
		}
	}
}

func TestSkillReadRejectsUnsafePackage(t *testing.T) {
	enabled := func(name, hash string) skills.Skill {
		return skills.Skill{ID: "s1", Name: name, Content: skillContent, EnabledBots: []int64{1}, Hash: hash}
	}
	for name, test := range map[string]struct {
		skill skills.Skill
		files map[string]string
		want  string
	}{
		"escaping path":  {enabled("weekly-report", hash), map[string]string{"SKILL.md": skillContent, "../escape": "x"}, "技能路径无效。"},
		"absolute path":  {enabled("weekly-report", hash), map[string]string{"SKILL.md": skillContent, "/etc/passwd": "x"}, "技能路径无效。"},
		"drive path":     {enabled("weekly-report", hash), map[string]string{"SKILL.md": skillContent, "C:evil": "x"}, "技能路径无效。"},
		"no SKILL.md":    {enabled("weekly-report", hash), map[string]string{"notes.md": "x"}, "缺少 SKILL.md。"},
		"escaping name":  {enabled("../../outside", hash), map[string]string{"SKILL.md": skillContent}, "技能名须为1至64位小写字母、数字或单连字符。"},
		"malformed hash": {enabled("weekly-report", "../"+hash[3:]), map[string]string{"SKILL.md": skillContent}, "版本哈希无效。"},
		"oversized file": {enabled("weekly-report", hash), map[string]string{"SKILL.md": skillContent, "big.bin": strings.Repeat("x", skills.PackageBytes)}, "技能包超过 8 MiB。"},
	} {
		work := &target{os: "linux", files: map[string]string{}}
		packages := fstest.MapFS{hash + ".zip": {Data: archive(t, test.files)}}
		service := skills.NewService(seed(t, test.skill), packages, func(context.Context, int64) (computer.Target, string, error) {
			return work, "/work/bot", nil
		})
		if result, text := call(t, service, 1, `{"id":"s1"}`); !result.IsError || text != test.want {
			t.Errorf("%s: text = %q", name, text)
		}
		if len(work.files) != 0 {
			t.Errorf("%s: wrote %d files", name, len(work.files))
		}
	}
}
