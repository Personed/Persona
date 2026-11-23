//go:build linux || windows

package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Personed/Persona/internal/engine"
)

// TestRealPiIdleBotsHaveNoProcess runs the installed pi when PERSONA_TEST_PI
// names it: three Bots open sessions, close them, and no pi remains after the
// idle timeout.
func TestRealPiIdleBotsHaveNoProcess(t *testing.T) {
	command := os.Getenv("PERSONA_TEST_PI")
	if command == "" {
		t.Skip("PERSONA_TEST_PI not set")
	}
	e := New(Config{
		Command:     command,
		Extensions:  []string{filepath.Join("..", "..", "..", "packages", "agent", "src", "harness", "pi-tools.mjs")},
		SessionDir:  t.TempDir(),
		StateDir:    t.TempDir(),
		Env:         append(os.Environ(), "PI_CODING_AGENT_DIR="+t.TempDir()),
		IdleTimeout: time.Second,
	})
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var procs []*Process
	for botID := int64(1); botID <= 3; botID++ {
		s, err := e.Start(ctx, engine.SessionOptions{BotID: botID, WorkDir: t.TempDir(), SystemPrompt: "Persona test."})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.State(ctx); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, s.(*session).conn.proc)
		_ = s.Close()
	}
	if live := e.Live(); live != 3 {
		t.Fatalf("live = %d during the idle window", live)
	}
	start := time.Now()
	waitFor(t, "idle pi processes to exit", func() bool {
		for _, p := range procs {
			if alive(p.PID()) {
				return false
			}
		}
		return e.Live() == 0
	})
	t.Logf("3 Bots idle: 0 pi processes %v after the last close", time.Since(start).Round(10*time.Millisecond))
}
