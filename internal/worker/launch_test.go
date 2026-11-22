package worker

import (
	"bytes"
	"context"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestAccountEnvironment(t *testing.T) {
	env := AccountEnvironment([]string{
		"PERSONA_SECRET=x", "service_token=y", "DATABASE_URL=z", "CLAUDECODE=1", "HOME=/home/bot", "PATH=/usr/bin", "KEEP=1",
	}, "/srv/persona", map[string]string{"PI_CODING_AGENT_DIR": "/home/bot/.pi"})
	for _, entry := range env {
		if key, _, _ := strings.Cut(entry, "="); slices.Contains([]string{"PERSONA_SECRET", "service_token", "DATABASE_URL", "CLAUDECODE"}, key) {
			t.Errorf("kept service variable %s", entry)
		}
	}
	list := string(os.PathListSeparator)
	want := "/usr/bin" + list + filepath.Join("/home/bot", ".local", "bin") + list + filepath.Join("/home/bot", ".npm-global", "bin") + list + filepath.Join("/srv/persona", "runtime", "node_modules", ".bin")
	if got := lookupEnv(env, "PATH"); got != want {
		t.Errorf("PATH = %q, want %q", got, want)
	}
	if lookupEnv(env, "USERPROFILE") != "/home/bot" || lookupEnv(env, "KEEP") != "1" || lookupEnv(env, "PI_CODING_AGENT_DIR") != "/home/bot/.pi" {
		t.Errorf("env = %q", env)
	}
}

func TestResolveProgram(t *testing.T) {
	home := t.TempDir()
	name := "persona-test-program"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	program := filepath.Join(home, ".local", "bin", name)
	write(t, program, "")
	if err := os.Chmod(program, 0o755); err != nil {
		t.Fatal(err)
	}
	env := AccountEnvironment([]string{"HOME=" + home, "PATH=" + filepath.Join(home, "empty")}, "", nil)
	ctx := context.Background()
	if runtime.GOOS != "windows" {
		if path, err := ResolveProgram(ctx, name, env); err != nil || path != program {
			t.Errorf("resolve on PATH = %q, %v", path, err)
		}
		if path, err := ResolveProgram(ctx, "~/.local/bin/"+name, env); err != nil || path != program {
			t.Errorf("resolve ~/ = %q, %v", path, err)
		}
	}
	if path, err := ResolveProgram(ctx, filepath.Join(home, "missing"), env); err != nil || path != "" {
		t.Errorf("resolve missing = %q, %v", path, err)
	}
}

func TestLaunch(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for name, client := range transports(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if path, err := client.Resolve(ctx, executable(t), ""); err != nil || path != executable(t) {
				t.Fatalf("resolve = %q, %v", path, err)
			}
			var stdout bytes.Buffer
			process, err := client.Launch(ctx, LaunchOptions{
				Program: executable(t), Args: []string{"cat"}, Dir: t.TempDir(), User: current.Username,
				Stdin: strings.NewReader(`{"type":"get_state"}` + "\n"), Stdout: &stdout,
			})
			if err != nil {
				t.Fatal(err)
			}
			if exit, err := process.Wait(); err != nil || exit != 3 || stdout.String() != `{"type":"get_state"}`+"\n" {
				t.Fatalf("launch = %d, %v, %q", exit, err, stdout.String())
			}
			if _, err := client.Launch(ctx, LaunchOptions{Program: "persona-missing-harness", Dir: t.TempDir()}); code(err) != "ENOENT" {
				t.Fatalf("missing harness: %v", err)
			}
			if runtime.GOOS != "windows" && os.Geteuid() != 0 {
				if _, err := client.Launch(ctx, LaunchOptions{Program: executable(t), Dir: t.TempDir(), User: "0"}); code(err) != "EPERM" {
					t.Fatalf("other user: %v", err)
				}
			}
		})
	}
}
