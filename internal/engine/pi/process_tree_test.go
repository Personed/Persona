//go:build linux || windows

package pi

import (
	"bufio"
	"encoding/json"
	"testing"
	"time"
)

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
