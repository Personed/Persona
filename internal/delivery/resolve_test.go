package delivery

import (
	"context"
	"slices"
	"testing"

	"github.com/Personed/Persona/internal/computer"
)

// visited records the paths Stat is asked about.
type visited struct {
	computer.Target
	paths []string
}

func (v *visited) Stat(_ context.Context, path string) (computer.FileInfo, error) {
	v.paths = append(v.paths, path)
	return computer.FileInfo{Path: path, Links: 1}, nil
}

func TestStatChecksEveryPathFromTheWorkspace(t *testing.T) {
	for _, test := range []struct {
		goos, dir, input string
		want             []string
	}{
		{"linux", "/work/bot", "out/sub/a.txt", []string{"/work/bot", "/work/bot/out", "/work/bot/out/sub", "/work/bot/out/sub/a.txt"}},
		{"linux", "/", "etc/hosts", []string{"/", "/etc", "/etc/hosts"}},
		{"windows", `C:\Users\bot\work`, `out\a.txt`, []string{"C:/Users/bot/work", "C:/Users/bot/work/out", "C:/Users/bot/work/out/a.txt"}},
		{"windows", `C:\`, `a.txt`, []string{"C:/", "C:/a.txt"}},
	} {
		absolute, relative, err := resolve(test.goos, test.dir, test.input)
		if err != nil {
			t.Fatal(err)
		}
		target := &visited{}
		if _, err := stat(t.Context(), target, absolute, relative); err != nil || !slices.Equal(target.paths, test.want) {
			t.Errorf("stat(%q, %q) visited %q, %v", absolute, relative, target.paths, err)
		}
	}
}

func TestResolve(t *testing.T) {
	for _, test := range []struct {
		goos, dir, input, absolute, relative string
	}{
		{"linux", "/work/bot", "out/a.txt", "/work/bot/out/a.txt", "out/a.txt"},
		{"linux", "/work/bot/", "./out/../a.txt", "/work/bot/a.txt", "a.txt"},
		{"linux", "/work/bot", "/work/bot/a.txt", "/work/bot/a.txt", "a.txt"},
		{"linux", "/work/bot", "/work/botany/a.txt", "", ""},
		{"linux", "/work/bot", "/work/Bot/a.txt", "", ""},
		{"linux", "/", "etc/hosts", "/etc/hosts", "etc/hosts"},
		{"darwin", "/Users/bot/work", `a\b.txt`, `/Users/bot/work/a\b.txt`, `a\b.txt`},
		{"windows", `C:\Users\bot\work`, `out\a.txt`, "C:/Users/bot/work/out/a.txt", "out/a.txt"},
		{"windows", `C:\Users\bot\work`, `c:\users\BOT\work\a.txt`, "c:/users/BOT/work/a.txt", "a.txt"},
		{"windows", `C:\Users\bot\work`, `D:\a.txt`, "", ""},
		{"windows", `C:\Users\bot\work`, `..\peer\a.txt`, "", ""},
		{"windows", `C:\Users\bot\work`, `a.txt::$DATA`, "", ""},
		{"windows", `C:\Users\bot\work`, `secret `, "", ""},
	} {
		absolute, relative, err := resolve(test.goos, test.dir, test.input)
		if absolute != test.absolute || relative != test.relative || (err == nil) != (test.absolute != "") {
			t.Errorf("resolve(%q, %q, %q) = %q, %q, %v", test.goos, test.dir, test.input, absolute, relative, err)
		}
	}
}
