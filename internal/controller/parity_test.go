package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHelperOutput(t *testing.T) {
	if os.Getenv("PERSONA_CONTROLLER_OUTPUT") == "" {
		t.Skip("helper process")
	}
	_, _ = os.Stdout.WriteString(strings.Repeat("x", outputLimit+1))
	os.Exit(0)
}

func TestExecRunnerLimitsOutput(t *testing.T) {
	t.Setenv("PERSONA_CONTROLLER_OUTPUT", "1")
	if _, err := ExecRunner(context.Background(), []string{os.Args[0], "-test.run=^TestHelperOutput$"}); !errors.Is(err, ErrCommand) {
		t.Fatalf("oversized output: err = %v, want ErrCommand", err)
	}
}

func TestParseDockerConfigMatchesZod(t *testing.T) {
	base := `"kind":"docker","id":"work","image":"img:1","volume":"vol"`
	config, err := ParseDockerConfig([]byte(`{` + base + `,"memoryMiB":1024.0,"pids":2.56e2,"timeoutSeconds":120,"ssh":{"host":"box","port":22.0,"knownHostsFile":"/k","hostKeyAlias":"box"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.MemoryMiB != 1024 || config.Pids != 256 || config.SSH == nil || config.SSH.Port != 22 {
		t.Fatalf("integers = %+v", config)
	}
	for name, data := range map[string]string{
		"fraction":       `{` + base + `,"pids":256.5}`,
		"null default":   `{` + base + `,"memoryMiB":null}`,
		"null network":   `{` + base + `,"network":null}`,
		"null optional":  `{` + base + `,"proxyImage":null}`,
		"null nested":    `{` + base + `,"ssh":{"host":"box","user":null,"knownHostsFile":"/k","hostKeyAlias":"box"}}`,
		"second value":   `{` + base + `} {"network":"web"}`,
		"trailing text":  `{` + base + `} x`,
		"not an object":  `null`,
		"huge integer":   `{` + base + `,"pids":1e300}`,
		"string integer": `{` + base + `,"pids":"256"}`,
	} {
		if _, err := ParseDockerConfig([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseDockerConfig([]byte("{" + base + "}\n\t ")); err != nil {
		t.Errorf("trailing whitespace rejected: %v", err)
	}
}

func TestChannelCommandsResolveDocker(t *testing.T) {
	want := "docker"
	if runtime.GOOS == "windows" {
		dir := t.TempDir()
		t.Setenv("LOCALAPPDATA", dir)
		t.Setenv("PATH", dir)
		t.Setenv("NoDefaultCurrentDirectoryInExePath", "1")
		want = filepath.Join(dir, "Programs", "DockerDesktop", "resources", "bin", "docker.exe")
		if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(want, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	docker, err := NewDocker(testConfig(NetworkNone), newFakeHost(t).run)
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := docker.Daemon()
	if err != nil {
		t.Fatal(err)
	}
	process, err := docker.Process(1)
	if err != nil {
		t.Fatal(err)
	}
	if daemon[0] != want || process[0] != want {
		t.Fatalf("programs = %q, %q, want %q", daemon[0], process[0], want)
	}
}
