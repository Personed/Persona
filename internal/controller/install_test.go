package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withInstallRunner runs body with the install commands answered by runner.
func withInstallRunner(runner func(ctx context.Context, args []string) (string, error), body func()) {
	saved := runInstallCommand
	runInstallCommand = func(ctx context.Context, args []string, _ time.Duration) (string, error) {
		return runner(ctx, args)
	}
	defer func() { runInstallCommand = saved }()
	body()
}

// writeRelease publishes the deploy/release.json the install reads under
// <root>/deploy.
func writeRelease(t *testing.T, root string, release map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "release.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func testRelease() map[string]any {
	return map[string]any{
		"version":         "0.1.0",
		"serviceImage":    "persona-service:0.1.0",
		"workImage":       "persona-workspace:3",
		"proxyImage":      "persona-egress:1",
		"proxyBuild":      map[string]any{"context": ".", "dockerfile": "deploy/proxy.Dockerfile"},
		"controllerEntry": "apps/controller/src/main.ts",
		"serverEntry":     "apps/api/src/main.ts",
		"bridgeEntry":     "packages/agent/src/computer/bridge.ts",
		"workerEntry":     "packages/agent/src/harness/worker.mjs",
		"staticDir":       "apps/web/dist",
		"workBuild":       map[string]any{"context": ".", "dockerfile": "deploy/workspace.Dockerfile"},
	}
}

func TestInstallWritesContainerDeployment(t *testing.T) {
	dir := t.TempDir()
	source := t.TempDir()
	writeRelease(t, source, testRelease())
	var builds []string
	runner := func(_ context.Context, args []string) (string, error) {
		switch args[1] {
		case "info":
			return `"linux"`, nil
		case "image":
			return "", ErrCommand
		case "build":
			builds = append(builds, strings.Join(args[3:], " "))
			return "", nil
		default:
			return "", ErrCommand
		}
	}
	withInstallRunner(runner, func() {
		if err := Install(context.Background(), []string{dir, source}, io.Discard); err != nil {
			t.Fatal(err)
		}
	})
	if len(builds) != 2 {
		t.Fatalf("expected work and proxy image builds, got %v", builds)
	}
	// The app config points at the controller over the internal network.
	data, err := os.ReadFile(filepath.Join(dir, "persona.config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["host"] != "0.0.0.0" || config["port"] != float64(4310) || config["dataDir"] != "/data" || config["deployment"] != "container" {
		t.Fatalf("unexpected persona.config.json: %s", data)
	}
	computer, _ := config["computer"].(map[string]any)
	if computer["kind"] != "controller" || computer["endpoint"] != "http://controller:4311" || computer["network"] != "web" {
		t.Fatalf("unexpected computer: %v", computer)
	}
	work, _ := config["workComputer"].(map[string]any)
	if work["endpoint"] != "http://controller:4311" || work["kind"] != "controller" {
		t.Fatalf("unexpected workComputer: %v", work)
	}
	// The controller config loads and its docker computer is the work image.
	loaded, err := LoadConfig(filepath.Join(dir, "controller.json"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Host != "0.0.0.0" || loaded.Port != 4311 || loaded.docker.Image != "persona-workspace:3" || loaded.docker.Network != NetworkWeb || loaded.docker.ProxyImage != "persona-egress:1" {
		t.Fatalf("unexpected controller config: %+v", loaded)
	}
	if computer["id"] != loaded.docker.ID {
		t.Fatalf("app computer id %v does not match the controller computer id %v", computer["id"], loaded.docker.ID)
	}
	secret, err := os.ReadFile(filepath.Join(dir, "controller-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base64.RawURLEncoding.DecodeString(string(secret)); err != nil || len(secret) < 32 {
		t.Fatalf("unexpected credential: %q", secret)
	}
}

func TestInstallResolvesRelativeDirectories(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	source := filepath.Join(root, "source")
	writeRelease(t, source, testRelease())
	withInstallRunner(func(_ context.Context, args []string) (string, error) {
		if args[1] == "info" {
			return `"linux"`, nil
		}
		return "", nil
	}, func() {
		if err := Install(context.Background(), []string{"control", source}, io.Discard); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := LoadConfig(filepath.Join(root, "control", "controller.json")); err != nil {
		t.Fatalf("the installed controller.json must load from a relative install: %v", err)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	source := t.TempDir()
	writeRelease(t, source, testRelease())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "persona.config.json"), []byte(`{"host":"0.0.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	withInstallRunner(func(_ context.Context, _ []string) (string, error) {
		called = true
		return "", nil
	}, func() {
		if err := Install(context.Background(), []string{dir, source}, io.Discard); err != nil {
			t.Fatal(err)
		}
	})
	if called {
		t.Fatal("an existing deployment must not touch Docker")
	}
	if _, err := os.Stat(filepath.Join(dir, "controller.json")); !os.IsNotExist(err) {
		t.Fatal("an existing deployment must keep its configuration")
	}
}
