package pi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// alive reports whether pid is a live, non-zombie process.
func alive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func TestProcessStopEndsTree(t *testing.T) {
	p, err := Start(fakePiCommand(t, "parent"))
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p.Stdout()).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var child struct{ PID int }
	if err := json.Unmarshal(line, &child); err != nil || !alive(child.PID) {
		t.Fatalf("grandchild %s: %v", line, err)
	}
	if err := p.Stop(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(child.PID) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived Stop", child.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
