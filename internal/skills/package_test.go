package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Personed/Persona/internal/store"
	"github.com/Personed/Persona/internal/store/storetest"
)

func archiveOf(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, content := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

const goodSkill = "---\nname: weekly-report\ndescription: Prepare a weekly report\n---\nRead the guide.\n"

func TestPackageBundleRerootsSingleDirectory(t *testing.T) {
	bundle, err := packageBundle("", archiveOf(t, map[string]string{
		"weekly-report/SKILL.md":            goodSkill,
		"weekly-report/references/guide.md": "Guide",
		"weekly-report/scripts/report.py":   "print(1)",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Name != "weekly-report" || bundle.Description != "Prepare a weekly report" {
		t.Fatalf("bundle = %+v", bundle)
	}
	for _, want := range []string{"SKILL.md", "references/guide.md", "scripts/report.py"} {
		if !slicesContains(bundle.Paths, want) {
			t.Fatalf("paths = %v", bundle.Paths)
		}
	}
	if bundle.Files["SKILL.md"] == nil || bundle.Files["weekly-report/SKILL.md"] != nil {
		t.Fatalf("files = %v", bundle.Files)
	}
}

func TestPackageBundleRejectsMultipleRoots(t *testing.T) {
	_, err := packageBundle("", archiveOf(t, map[string]string{
		"one/SKILL.md": goodSkill,
		"two/evil.py":  "print(2)",
	}))
	want := &Error{Code: BadRequest, Message: "ZIP 只能包含一个技能目录。"}
	var e *Error
	if !errors.As(err, &e) || *e != *want {
		t.Fatalf("err = %v", err)
	}
}

func TestParseSkill(t *testing.T) {
	for name, test := range map[string]struct {
		content string
		want    string
	}{
		"missing front matter": {"Hello", "SKILL.md需要YAML前置信息。"},
		"empty body":           {"---\nname: x\ndescription: y\n---\n", "技能正文不能为空。"},
		"bad name":             {"---\nname: Evil\ndescription: y\n---\nBody", "技能名须为1至64位小写字母、数字或单连字符。"},
		"no description":       {"---\nname: x\n---\nBody", "技能描述须为1至1024个字符。"},
		"yaml sequence":        {"---\n- a\n---\nBody", "技能前置信息必须是对象。"},
		"string y description": {"---\nname: x\ndescription: y\n---\nBody", ""},
	} {
		if _, err := parseSkill(test.content); (err == nil) != (test.want == "") ||
			(err != nil && err.Error() != test.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	parsed, err := parseSkill("\uFEFF---\nname: bom\ndescription:  With space. \n---\r\nBody\r\n")
	if err != nil || parsed.name != "bom" || parsed.description != "With space." {
		t.Fatalf("parsed = %+v, %v", parsed, err)
	}
}

func slicesContains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// TestSavePreviewRestore exercises the manage cycle against a package
// directory on disk, as the TypeScript Skills over SkillPackages.
func TestSavePreviewRestore(t *testing.T) {
	records := storetest.New()
	service := NewService(records, nil, nil)
	service.UsePackageDir(t.TempDir())
	ctx := context.Background()
	v1 := archiveOf(t, map[string]string{
		"weekly-report/SKILL.md":          goodSkill,
		"weekly-report/scripts/report.py": "print('v1')",
	})
	v2 := archiveOf(t, map[string]string{
		"weekly-report/SKILL.md":          goodSkill,
		"weekly-report/scripts/report.py": "print('v2')",
	})
	skill, err := service.Save(ctx, SaveInput{ID: "s1", Archive: v1, RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if skill.Name != "weekly-report" || skill.Revision != 1 || len(skill.Versions) != 1 {
		t.Fatalf("skill = %+v", skill)
	}
	// A retried request returns the same skill without writing again.
	again, err := service.Save(ctx, SaveInput{ID: "s1", Archive: v1, RequestID: "r1"})
	if err != nil || again.Revision != skill.Revision {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	skill, err = service.Save(ctx, SaveInput{ID: "s1", Archive: v2, Revision: 1, RequestID: "r2"})
	if err != nil || len(skill.Versions) != 2 {
		t.Fatalf("v2 = %+v, %v", skill, err)
	}
	// The hash-named package exists under the directory.
	v1Hash := skill.Versions[0].Hash
	if _, err := os.Stat(filepath.Join(service.packageDir, v1Hash+".zip")); err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, "s1", "", v1)
	if err != nil {
		t.Fatal(err)
	}
	out := preview.(map[string]any)
	changes := out["changes"].([]PreviewChange)
	if len(changes) != 1 || changes[0].Path != "scripts/report.py" || changes[0].Status != "changed" {
		t.Fatalf("changes = %+v", changes)
	}
	restored, err := service.Restore(ctx, "s1", v1Hash, 2, "r3")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Hash != v1Hash || restored.Versions[len(restored.Versions)-1].Version != 3 {
		t.Fatalf("restored = %+v", restored)
	}
	// Stale revisions and name conflicts reject as the TypeScript service does.
	if _, err := service.Save(ctx, SaveInput{ID: "s1", Content: goodSkill, Revision: 0, RequestID: "r4"}); err == nil {
		t.Fatal("stale revision saved")
	}
	if _, err := service.Save(ctx, SaveInput{ID: "other", Content: goodSkill, Revision: 0, RequestID: "r5"}); err == nil ||
		!strings.Contains(err.Error(), "技能名已存在。") {
		t.Fatalf("conflict err = %v", err)
	}
}

var _ store.Store = storetest.New() // keep the store import used across files
