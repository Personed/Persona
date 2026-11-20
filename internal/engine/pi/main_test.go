package pi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake `pi` when fakeMode is set.
const fakeMode = "PERSONA_TEST_FAKE_PI"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeMode); mode != "" {
		runFakePi(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakePiCommand starts the test binary as a fake Pi in the given mode:
// "rpc" answers commands like Pi and exits on stdin EOF; "stubborn" ignores EOF
// and SIGTERM; "parent" starts a "sleep" grandchild, prints its pid, then acts as "rpc".
func fakePiCommand(t *testing.T, mode string) Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Command{Path: exe, Env: append(os.Environ(), fakeMode+"="+mode)}
}

func runFakePi(mode string) {
	if slices.Contains(os.Args[1:], "--version") {
		fmt.Println("1.5.2-fake")
		return
	}
	switch mode {
	case "sleep", "stubborn":
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

// answer emulates the parts of Pi's RPC mode the tests use.
func answer() {
	if os.Getenv("PERSONA_PI_LOGIN") == "1" {
		fakeLogin()
		return
	}
	out := json.NewEncoder(os.Stdout)
	emit := func(record map[string]any) { _ = out.Encode(record) }
	file := fmt.Sprintf("/sessions/%d-0.jsonl", os.Getpid())
	if slices.Contains(os.Args[1:], "--no-session") {
		file = ""
	}
	sessions := 0
	var model map[string]any
	level := "medium"
	cwd, _ := os.Getwd()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var command map[string]any
		if json.Unmarshal(scanner.Bytes(), &command) != nil {
			continue
		}
		id, _ := command["id"].(string)
		kind, _ := command["type"].(string)
		data := map[string]any{"pid": os.Getpid()}
		reply := func() {
			emit(map[string]any{"id": id, "type": "response", "command": kind, "success": true, "data": data})
		}
		switch kind {
		case "get_state":
			data["sessionFile"] = file
			data["thinkingLevel"] = level
			data["isStreaming"] = false
			data["pendingMessageCount"] = 0
			data["argv"] = os.Args[1:]
			data["cwd"] = cwd
			data["env"] = os.Getenv("PERSONA_TEST_ENV")
			if model != nil {
				data["model"] = model
			}
		case "switch_session":
			file = command["sessionPath"].(string)
			data["cancelled"] = false
		case "new_session":
			sessions++
			file = fmt.Sprintf("/sessions/%d-%d.jsonl", os.Getpid(), sessions)
		case "set_model":
			model = map[string]any{"provider": command["provider"], "id": command["modelId"]}
		case "set_thinking_level":
			level = command["level"].(string)
		case "get_messages":
			data["messages"] = []map[string]string{{"role": "user"}, {"role": "assistant"}, {"role": "user"}}
		case "get_session_stats":
			data["tokens"] = map[string]int{"input": 10, "output": 5, "cacheRead": 2, "cacheWrite": 1, "total": 18}
			data["cost"] = 0.25
		case "get_available_models":
			data["models"] = []map[string]any{{"provider": "anthropic", "id": "claude", "name": "Claude", "input": []string{"text", "image"}, "reasoning": true}}
		case "extension_ui_response":
			emit(map[string]any{"type": "answered", "id": id, "value": command["value"]})
			emit(map[string]any{"type": "agent_settled"})
			continue
		case "reject":
			emit(map[string]any{"id": id, "type": "response", "command": kind, "success": false, "error": "rejected"})
			continue
		case "prompt":
			reply()
			emit(map[string]any{"type": "agent_start"})
			emit(map[string]any{"type": "message_update", "text": command["message"], "behavior": command["streamingBehavior"]})
			if command["message"] == "ask" {
				emit(map[string]any{"type": "extension_ui_request", "id": "ui-1", "method": "select", "title": "Pick", "options": []string{"a", "b"}})
				continue
			}
			emit(map[string]any{"type": "agent_end"})
			emit(map[string]any{"type": "agent_settled"})
			continue
		}
		reply()
	}
}

// fakeLogin emulates pi-auth.mjs: pick a provider, open a link, paste a code.
func fakeLogin() {
	out := json.NewEncoder(os.Stdout)
	emit := func(record map[string]any) { _ = out.Encode(record) }
	notify := func(value map[string]any) {
		encoded, _ := json.Marshal(value)
		emit(map[string]any{"type": "extension_ui_request", "id": "n", "method": "notify", "message": "persona-auth:" + string(encoded)})
	}
	extension := slices.Contains(os.Args[1:], "--extension") && os.Getenv("PERSONA_TEST_NO_AUTH") == ""
	scanner := bufio.NewScanner(os.Stdin)
	prompt := ""
	for scanner.Scan() {
		var command map[string]any
		if json.Unmarshal(scanner.Bytes(), &command) != nil {
			continue
		}
		id, _ := command["id"].(string)
		switch command["type"] {
		case "get_commands":
			commands := []map[string]string{{"name": "skill:x"}}
			if extension {
				commands = append(commands, map[string]string{"name": "persona-auth"})
			}
			emit(map[string]any{"id": id, "type": "response", "command": "get_commands", "success": true, "data": map[string]any{"commands": commands}})
		case "prompt":
			prompt = id
			emit(map[string]any{"type": "extension_ui_request", "id": "p1", "method": "select", "title": "模型提供方", "options": []string{"anthropic"}})
		case "extension_ui_response":
			switch command["id"] {
			case "p1":
				notify(map[string]any{"type": "auth_url", "url": "https://auth.example/login?x=1", "instructions": "Open the link"})
				notify(map[string]any{"type": "auth_url", "url": "http://evil.example/"})
				notify(map[string]any{"secret": true})
				emit(map[string]any{"type": "extension_ui_request", "id": "p2", "method": "input", "title": "Code"})
			case "p2":
				state := "failed"
				if command["value"] == "good" {
					state = "done"
				}
				notify(map[string]any{"state": state})
				emit(map[string]any{"id": prompt, "type": "response", "command": "prompt", "success": true})
			}
		}
	}
}
