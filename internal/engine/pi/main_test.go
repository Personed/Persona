package pi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake `pi --mode rpc` when fakeMode is set.
const fakeMode = "PERSONA_TEST_FAKE_PI"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeMode); mode != "" {
		runFakePi(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakePiCommand starts the test binary as a fake Pi in the given mode:
// "rpc" answers every command and exits on stdin EOF; "stubborn" ignores EOF and
// SIGTERM; "parent" starts a "sleep" grandchild, prints its pid, then acts as "rpc".
func fakePiCommand(t *testing.T, mode string) Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Command{Path: exe, Env: append(os.Environ(), fakeMode+"="+mode)}
}

func runFakePi(mode string) {
	switch mode {
	case "sleep":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
	case "stubborn":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
	case "parent":
		exe, _ := os.Executable()
		child := exec.Command(exe)
		child.Env = append(os.Environ(), fakeMode+"=sleep")
		if err := child.Start(); err != nil {
			os.Exit(1)
		}
		fmt.Printf("{\"type\":\"child\",\"pid\":%d}\n", child.Process.Pid)
		answer()
	default:
		answer()
	}
}

func answer() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var command struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if json.Unmarshal(scanner.Bytes(), &command) != nil {
			continue
		}
		reply, _ := json.Marshal(map[string]any{"id": command.ID, "type": "response", "command": command.Type, "success": true, "data": map[string]int{"pid": os.Getpid()}})
		fmt.Println(string(reply))
	}
}
