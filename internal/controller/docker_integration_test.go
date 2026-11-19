package controller

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestDockerLifecycleIntegration runs the lifecycle against a real Docker engine.
// Set PERSONA_DOCKER_TEST to an image that keeps running as 1000:1000, for example
// one that runs "sleep infinity".
func TestDockerLifecycleIntegration(t *testing.T) {
	image := os.Getenv("PERSONA_DOCKER_TEST")
	if image == "" {
		t.Skip("PERSONA_DOCKER_TEST not set")
	}
	ctx := context.Background()
	config := DockerConfig{Kind: "docker", ID: "go-test", Image: image, Volume: "persona-go-test",
		Network: NetworkNone, CPUs: 0.5, MemoryMiB: 256, Pids: 64, TimeoutSeconds: 30}
	d, err := NewDocker(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = ExecRunner(ctx, []string{"docker", "container", "rm", "--force", config.Names().Container})
		_, _ = ExecRunner(ctx, []string{"docker", "volume", "rm", config.Volume})
	})
	expect := func(want State) {
		t.Helper()
		if got, err := d.Inspect(ctx); err != nil || got != want {
			t.Fatalf("Inspect() = %v, %v; want %v", got, err, want)
		}
	}
	expect(StateMissing)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	expect(StateRunning)
	if err := d.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	expect(StateStopped)
	if err := d.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	expect(StateRunning)

	changed := config
	changed.MemoryMiB = 512
	other, err := NewDocker(changed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Inspect(ctx); !errors.Is(err, ErrMismatch) {
		t.Fatalf("changed config: err = %v, want ErrMismatch", err)
	}
}
