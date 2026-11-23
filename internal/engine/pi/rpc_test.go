package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakePi is the Pi side of a client connection.
type fakePi struct {
	stdout   *io.PipeWriter
	commands *bufio.Scanner
	client   *Client
}

func newFakePi(t *testing.T) *fakePi {
	t.Helper()
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	t.Cleanup(func() {
		_ = outW.Close()
		_ = inR.Close()
	})
	return &fakePi{stdout: outW, commands: bufio.NewScanner(inR), client: NewClient(outR, inW)}
}

func (f *fakePi) write(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(f.stdout, line); err != nil {
		t.Fatal(err)
	}
}

func (f *fakePi) command(t *testing.T) map[string]any {
	t.Helper()
	if !f.commands.Scan() {
		t.Fatalf("no command: %v", f.commands.Err())
	}
	var command map[string]any
	if err := json.Unmarshal(f.commands.Bytes(), &command); err != nil {
		t.Fatal(err)
	}
	return command
}

func nextFrame(t *testing.T, c *Client) Frame {
	t.Helper()
	select {
	case frame, ok := <-c.Events():
		if !ok {
			t.Fatal("events closed")
		}
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return Frame{}
}

func TestRequestCorrelatesOutOfOrderResponses(t *testing.T) {
	pi := newFakePi(t)
	ctx := context.Background()
	type result struct {
		data string
		err  error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() {
		data, err := pi.client.Request(ctx, "get_state", nil)
		first <- result{string(data), err}
	}()
	a := pi.command(t)
	go func() {
		data, err := pi.client.Request(ctx, "set_model", map[string]string{"provider": "p", "modelId": "m"})
		second <- result{string(data), err}
	}()
	b := pi.command(t)
	if a["type"] != "get_state" || b["type"] != "set_model" || b["modelId"] != "m" || a["id"] == b["id"] {
		t.Fatalf("commands %v %v", a, b)
	}
	pi.write(t, `{"id":"`+b["id"].(string)+`","type":"response","command":"set_model","success":true,"data":{"id":"m"}}`+"\n")
	pi.write(t, `{"id":"`+a["id"].(string)+`","type":"response","command":"get_state","success":true,"data":{"isStreaming":false}}`+"\r\n")
	if got := <-second; got.err != nil || got.data != `{"id":"m"}` {
		t.Fatalf("set_model = %+v", got)
	}
	if got := <-first; got.err != nil || got.data != `{"isStreaming":false}` {
		t.Fatalf("get_state = %+v", got)
	}
}

func TestRequestRejected(t *testing.T) {
	pi := newFakePi(t)
	done := make(chan error, 1)
	go func() {
		_, err := pi.client.Request(context.Background(), "compact", nil)
		done <- err
	}()
	id := pi.command(t)["id"].(string)
	pi.write(t, `{"id":"`+id+`","type":"response","command":"compact","success":false,"error":"nothing to compact"}`+"\n")
	var rejected *RequestError
	if err := <-done; !errors.As(err, &rejected) || rejected.Message != "nothing to compact" {
		t.Fatalf("err = %v", err)
	}
}

func TestRecordsSplitOnlyOnLineFeed(t *testing.T) {
	pi := newFakePi(t)
	text := "a\u2028b\u2029c"
	pi.write(t, `{"type":"message_update","text":"`+text+`"}`+"\r\n"+"\n"+`{"type":"agent_settled"}`+"\n")
	frame := nextFrame(t, pi.client)
	var event struct{ Text string }
	if err := json.Unmarshal(frame.Raw, &event); err != nil || frame.Type != "message_update" || event.Text != text {
		t.Fatalf("frame = %s %q %v", frame.Type, event.Text, err)
	}
	if frame := nextFrame(t, pi.client); frame.Type != "agent_settled" {
		t.Fatalf("frame = %s", frame.Type)
	}
}

func TestSlowConsumerDoesNotBlockResponses(t *testing.T) {
	pi := newFakePi(t)
	go func() {
		for range 1000 {
			if _, err := io.WriteString(pi.stdout, `{"type":"message_update"}`+"\n"); err != nil {
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := pi.client.Request(context.Background(), "abort", nil)
		done <- err
	}()
	id := pi.command(t)["id"].(string)
	pi.write(t, `{"id":"`+id+`","type":"response","command":"abort","success":true}`+"\n")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("response blocked behind unread events")
	}
	for range 1000 {
		nextFrame(t, pi.client)
	}
}

func TestCloseFailsPendingAndDeliversBufferedEvents(t *testing.T) {
	pi := newFakePi(t)
	done := make(chan error, 1)
	go func() {
		_, err := pi.client.Request(context.Background(), "prompt", map[string]string{"message": "hi"})
		done <- err
	}()
	pi.command(t)
	pi.write(t, `{"type":"agent_start"}`+"\n")
	_ = pi.stdout.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v", err)
	}
	if frame := nextFrame(t, pi.client); frame.Type != "agent_start" {
		t.Fatalf("frame = %s", frame.Type)
	}
	if _, ok := <-pi.client.Events(); ok {
		t.Fatal("events open after close")
	}
	if _, err := pi.client.Request(context.Background(), "get_state", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("request after close = %v", err)
	}
}

func TestInvalidOrOversizedRecordEndsConnection(t *testing.T) {
	for name, record := range map[string]string{
		"invalid":   "not json\n",
		"oversized": `{"type":"x","pad":"` + strings.Repeat("a", MaxFrameBytes) + `"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			pi := newFakePi(t)
			go func() { _, _ = io.WriteString(pi.stdout, record) }()
			select {
			case <-pi.client.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("connection still open")
			}
			if !errors.Is(pi.client.Err(), ErrClosed) {
				t.Fatalf("err = %v", pi.client.Err())
			}
		})
	}
}

func TestRequestContextDeadline(t *testing.T) {
	pi := newFakePi(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := pi.client.Request(ctx, "get_state", nil)
		done <- err
	}()
	pi.command(t)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestTypedNilParams(t *testing.T) {
	pi := newFakePi(t)
	done := make(chan error, 1)
	go func() {
		_, err := pi.client.Request(context.Background(), "get_state", (*struct{})(nil))
		done <- err
	}()
	command := pi.command(t)
	pi.write(t, `{"id":"`+command["id"].(string)+`","type":"response","command":"get_state","success":true}`+"\n")
	if err := <-done; err != nil || command["type"] != "get_state" {
		t.Fatalf("command %v err %v", command, err)
	}
}

func TestStalledStdinHonorsContext(t *testing.T) {
	outR, outW := io.Pipe()
	_, inW := io.Pipe() // nobody reads Pi's stdin
	t.Cleanup(func() { _ = outW.Close() })
	client := NewClient(outR, inW)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	for range 2 {
		if _, err := client.Request(ctx, "get_state", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	}
	if err := client.Send(ctx, map[string]string{"type": "extension_ui_response"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send = %v", err)
	}
}

type gatedWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}

func TestSendWaitsForAcceptedWriteAfterEOF(t *testing.T) {
	outR, outW := io.Pipe()
	writer := &gatedWriter{started: make(chan struct{}), release: make(chan struct{})}
	client := NewClient(outR, writer)
	done := make(chan error, 1)
	go func() {
		done <- client.Send(context.Background(), map[string]string{"type": "extension_ui_response"})
	}()

	<-writer.started
	_ = outW.Close()
	<-client.Done()
	select {
	case err := <-done:
		t.Fatalf("Send returned before its accepted write completed: %v", err)
	default:
	}
	close(writer.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return after the write completed")
	}
}

func TestResponseBeforeEOFWins(t *testing.T) {
	for range 50 {
		pi := newFakePi(t)
		done := make(chan error, 1)
		go func() {
			_, err := pi.client.Request(context.Background(), "abort", nil)
			done <- err
		}()
		id := pi.command(t)["id"].(string)
		pi.write(t, `{"id":"`+id+`","type":"response","command":"abort","success":true}`+"\n")
		_ = pi.stdout.Close()
		if err := <-done; err != nil {
			t.Fatalf("answered request failed: %v", err)
		}
	}
}
