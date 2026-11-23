package pi

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestProcessSpeaksRPCAndStopsOnStdinClose(t *testing.T) {
	p, err := Start(fakePiCommand(t, "rpc"))
	if err != nil {
		t.Fatal(err)
	}
	if reply := roundTrip(t, p); !strings.Contains(reply, `"success":true`) {
		t.Fatalf("get_state = %s", reply)
	}
	start := time.Now()
	if err := p.Stop(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("orderly stop took %v", elapsed)
	}
	if err := p.Err(); err != nil {
		t.Fatalf("exit = %v", err)
	}
}

// roundTrip sends one command to a fake Pi and returns its response record.
func roundTrip(t *testing.T, p *Process) string {
	t.Helper()
	if _, err := io.WriteString(p.Stdin(), `{"id":"1","type":"get_state"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	var line []byte
	buf := make([]byte, 1)
	for {
		if _, err := p.Stdout().Read(buf); err != nil {
			t.Fatal(err)
		}
		if buf[0] == '\n' {
			return string(line)
		}
		line = append(line, buf[0])
	}
}

func TestProcessStopEscalates(t *testing.T) {
	p, err := Start(fakePiCommand(t, "stubborn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(100 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("process still running after Stop")
	}
	if err := p.Stop(time.Millisecond); err != nil {
		t.Fatalf("second Stop = %v", err)
	}
}

func TestStartMissingBinary(t *testing.T) {
	if _, err := Start(Command{Path: "persona-no-such-pi"}); err == nil {
		t.Fatal("Start succeeded")
	}
}
