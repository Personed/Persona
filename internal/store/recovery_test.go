package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const crashMarker = "PERSONA_TEST_CRASH_STORE"
const interruptedCommitExitCode = 73

func TestCrashDuringCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.sqlite")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashProcess$")
	child.Env = append(os.Environ(), crashMarker+"="+path)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != interruptedCommitExitCode {
		t.Fatalf("child did not reach crash point: %v\n%s", err, output)
	}

	s := openTestPath(t, path)
	if _, err := s.Conversation(ctx, 1); err != nil {
		t.Fatalf("committed conversation not recovered: %v", err)
	}
	if _, err := s.Conversation(ctx, 50); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial conversation leaked: %v", err)
	}
	if _, _, err := s.Entry(ctx, 51); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial entry leaked: %v", err)
	}
	if id, err := s.MintID(ctx); err != nil || id != 4 {
		t.Fatalf("rolled back batch left its ID watermark: %d, %v", id, err)
	}
	recovery, err := s.RecoveryTasks(ctx, Scan{Limit: 1})
	if err != nil || recovery.Next != "" || len(recovery.Items) != 1 || recovery.Items[0].ID != 2 ||
		recovery.Items[0].State.Status != TaskRunning || string(recovery.Items[0].State.Checkpoint) != `{"step":7}` {
		t.Fatalf("tasks to reconcile = %+v, %v", recovery, err)
	}
	calls, err := s.Calls(ctx, 2)
	if err != nil || len(calls) != 2 || calls[0].State != CallComplete || string(calls[0].Result) != `{"ok":true}` ||
		calls[1].State != CallUncertain || calls[1].Result != nil || string(calls[1].Intent) != `{"tool":"write"}` {
		t.Fatalf("recovered journal = %+v, %v", calls, err)
	}
	if call, dispatch, err := s.BeginCall(ctx, 2, "second", calls[1].Intent); err != nil || dispatch || call.State != CallUncertain {
		t.Fatalf("uncertain call replay: %+v, dispatch=%v, %v", call, dispatch, err)
	}
	if seq := mustCommit(t, s, conversationWrite(4)); seq != 6 {
		t.Fatalf("failed transaction should consume no seq, recovery should consume one: next=%d", seq)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestPath(t, path)
	if seq := mustCommit(t, s, conversationWrite(5)); seq != 7 {
		t.Fatalf("second recovery should be a no-op: next=%d", seq)
	}
}

func TestCrashProcess(t *testing.T) {
	path := os.Getenv(crashMarker)
	if path == "" {
		t.Skip("only run by subprocess crash test")
	}
	ctx := t.Context()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	running := &Task{ID: 2, ConversationID: 1, State: TaskState{Status: TaskRunning, Checkpoint: json.RawMessage(`{"step":7}`)}}
	terminal := &Task{ID: 3, ConversationID: 1, State: TaskState{Status: TaskTerminal}}
	mustCommit(t, s, conversationWrite(1), Write{Type: WriteTask, Task: running}, Write{Type: WriteTask, Task: terminal})
	for _, id := range []string{"first", "second"} {
		if _, dispatch, err := s.BeginCall(ctx, running.ID, id, json.RawMessage(`{"tool":"write"}`)); err != nil || !dispatch {
			t.Fatalf("record call %q: dispatch=%v, %v", id, dispatch, err)
		}
	}
	if err := s.CompleteCall(ctx, running.ID, "first", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA cache_size=8"); err != nil {
		t.Fatal(err)
	}
	wal := path + "-wal"
	before, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry := &Entry{ID: 51, ConversationID: 50, Data: json.RawMessage(`"` + strings.Repeat("x", 512*1024) + `"`)}
	if _, err := applyBatch(ctx, tx, Batch{conversationWrite(50), {Type: WriteEntry, Entry: entry}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(wal)
	if err != nil || after.Size() <= before.Size() {
		t.Fatalf("test must spill uncommitted pages to WAL before crashing: %v, %v", after, err)
	}
	// Abrupt exit: no commit, rollback, connection close, or lease release runs in Go.
	os.Exit(interruptedCommitExitCode)
}

func TestCallJournalAuthorizesDispatchOnce(t *testing.T) {
	s, _ := openTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	mustCommit(t, s,
		Write{Type: WriteTask, Task: &Task{ID: 1, State: TaskState{Status: TaskRunning}}},
		Write{Type: WriteTask, Task: &Task{ID: 2, State: TaskState{Status: TaskRunning}}},
	)
	watch, err := s.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteCall(ctx, 1, "abc", json.RawMessage(`null`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("result written without an intent: %v", err)
	}
	const attempts = 8
	var wg sync.WaitGroup
	allowed := make(chan bool, attempts)
	for range attempts {
		wg.Go(func() {
			r, dispatch, err := s.BeginCall(ctx, 1, "abc", json.RawMessage(`{"name":"tool"}`))
			if err != nil || r.State != CallIntent || r.Result != nil {
				t.Errorf("intent = %+v, %v", r, err)
			}
			allowed <- dispatch
		})
	}
	wg.Wait()
	close(allowed)
	dispatched := 0
	for permit := range allowed {
		if permit {
			dispatched++
		}
	}
	if dispatched != 1 {
		t.Fatalf("journal authorized %d dispatches for one call ID", dispatched)
	}
	for _, body := range []json.RawMessage{json.RawMessage(`{"error":"refused"}`), json.RawMessage(`{ "error": "refused" }`)} {
		if err := s.CompleteCall(ctx, 1, "abc", body); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CompleteCall(ctx, 1, "abc", json.RawMessage(`null`)); err == nil {
		t.Fatal("journal overwrote a result")
	}
	if r, dispatch, err := s.BeginCall(ctx, 1, "abc", json.RawMessage(`{}`)); err != nil || dispatch ||
		r.State != CallComplete || string(r.Result) != `{"error":"refused"}` {
		t.Fatalf("retry = %s, dispatch=%v, %v", r.Result, dispatch, err)
	}
	if _, dispatch, err := s.BeginCall(ctx, 2, "abc", json.RawMessage(`{}`)); err != nil || !dispatch {
		t.Fatalf("same ID on another task = dispatch=%v, %v", dispatch, err)
	}
	// Journal changes take one sequence each; duplicates consume none.
	for _, expected := range []Seq{2, 3, 4} {
		select {
		case seq := <-watch:
			if seq != expected {
				t.Fatalf("watch seq = %d, want %d", seq, expected)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if seq := mustCommit(t, s, conversationWrite(99)); seq != 5 {
		t.Fatalf("duplicate call consumed a sequence: next commit=%d", seq)
	}
}
