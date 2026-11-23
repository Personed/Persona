package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// MaxFrameBytes bounds one JSONL record, matching the TypeScript adapter.
const MaxFrameBytes = 16 << 20

// ErrClosed reports that the Pi connection ended.
var ErrClosed = errors.New("pi: connection closed")

// RequestError is a command that Pi answered with success: false.
type RequestError struct {
	Command string
	Message string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("pi: %s rejected: %s", e.Command, e.Message)
}

// Frame is one record from Pi that is not a command response: a session event or
// an extension_ui_request. Raw is the record as received.
type Frame struct {
	Type string
	Raw  json.RawMessage
}

type response struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

type write struct {
	line []byte
	done chan error
}

type pending struct {
	command string
	done    chan response
}

// Client speaks Pi's RPC protocol over a pair of streams. Requests are correlated
// by id, so several may be outstanding. Methods are safe for concurrent use.
type Client struct {
	w      io.Writer
	writes chan write

	mu      sync.Mutex
	seq     uint64
	pending map[string]pending
	err     error
	done    chan struct{}

	queue  chan Frame
	events chan Frame
}

// NewClient reads Pi's stdout from r and writes commands to w. It reads r until EOF.
func NewClient(r io.Reader, w io.Writer) *Client {
	c := &Client{
		w:       w,
		writes:  make(chan write),
		pending: map[string]pending{},
		done:    make(chan struct{}),
		queue:   make(chan Frame),
		events:  make(chan Frame),
	}
	go c.forward()
	go c.write()
	go c.read(r)
	return c
}

// Events delivers frames in arrival order. It closes after the connection ends and
// every received frame was delivered. Frames are buffered, so a slow consumer never
// stalls Pi's stdout or delays command responses.
func (c *Client) Events() <-chan Frame { return c.events }

// Done closes when the connection ends; Err then reports why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the reason the connection ended, or nil while it is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Request sends one command and waits for its response data. Params must encode to
// a JSON object or be nil; its fields join the command record.
func (c *Client) Request(ctx context.Context, command string, params any) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &fields); err != nil {
			return nil, fmt.Errorf("pi: %s params must be an object: %w", command, err)
		}
		if fields == nil {
			fields = map[string]json.RawMessage{}
		}
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}
	c.seq++
	id := strconv.FormatUint(c.seq, 10)
	call := pending{command: command, done: make(chan response, 1)}
	c.pending[id] = call
	c.mu.Unlock()
	defer c.forget(id)

	fields["id"], _ = json.Marshal(id)
	fields["type"], _ = json.Marshal(command)
	var res response
	if err := c.send(ctx, fields); err != nil {
		// The write may have landed and been answered just before the connection ended.
		select {
		case res = <-call.done:
			return res.result(command)
		default:
			return nil, fmt.Errorf("pi: %s: %w", command, err)
		}
	}
	select {
	case res = <-call.done:
	case <-c.done:
		// A response read just before the connection ended still counts.
		select {
		case res = <-call.done:
		default:
			return nil, fmt.Errorf("pi: %s: %w", command, c.Err())
		}
	case <-ctx.Done():
		return nil, fmt.Errorf("pi: %s: %w", command, ctx.Err())
	}
	return res.result(command)
}

func (r response) result(command string) (json.RawMessage, error) {
	if !r.Success {
		return nil, &RequestError{Command: command, Message: r.Error}
	}
	return r.Data, nil
}

// Send writes one record, such as an extension_ui_response, without waiting for a reply.
func (c *Client) Send(ctx context.Context, record any) error { return c.send(ctx, record) }

// send hands the record to the writer goroutine, so a stalled stdin never holds
// a caller past its context or the end of the connection.
func (c *Client) send(ctx context.Context, record any) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	w := write{line: append(line, '\n'), done: make(chan error, 1)}
	select {
	case c.writes <- w:
	case <-c.done:
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-w.done:
		return err
	case <-c.done:
		// The writer accepted this record before the connection ended. Its
		// acknowledgement determines whether the write itself succeeded.
		select {
		case err := <-w.done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) write() {
	for {
		select {
		case w := <-c.writes:
			_, err := c.w.Write(w.line)
			w.done <- err
		case <-c.done:
			return
		}
	}
}

func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) read(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// ScanLines splits on LF only and drops a trailing CR. Unicode line and
	// paragraph separators stay inside records, as the protocol requires.
	scanner.Buffer(make([]byte, 0, 64<<10), MaxFrameBytes)
	var failure error
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := c.dispatch(line); err != nil {
			failure = err
			break
		}
	}
	if failure == nil {
		failure = scanner.Err()
	}
	if errors.Is(failure, bufio.ErrTooLong) {
		failure = fmt.Errorf("pi: record over %d bytes", MaxFrameBytes)
	}
	if failure == nil {
		failure = ErrClosed
	} else {
		failure = fmt.Errorf("%w: %w", ErrClosed, failure)
	}
	c.mu.Lock()
	c.err = failure
	c.pending = map[string]pending{}
	close(c.done)
	c.mu.Unlock()
	close(c.queue)
	// Drain stdout so Pi can finish an orderly shutdown.
	_, _ = io.Copy(io.Discard, r)
}

func (c *Client) dispatch(line []byte) error {
	var head struct {
		ID   *string `json:"id"`
		Type string  `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return fmt.Errorf("pi: invalid record: %w", err)
	}
	if head.Type == "response" && head.ID != nil {
		var res response
		if err := json.Unmarshal(line, &res); err != nil {
			return fmt.Errorf("pi: invalid response: %w", err)
		}
		c.mu.Lock()
		call, ok := c.pending[*head.ID]
		c.mu.Unlock()
		if ok {
			call.done <- res
		}
		return nil
	}
	c.queue <- Frame{Type: head.Type, Raw: append(json.RawMessage(nil), line...)}
	return nil
}

func (c *Client) forward() {
	var backlog []Frame
	queue := c.queue
	for queue != nil || len(backlog) > 0 {
		var out chan Frame
		var next Frame
		if len(backlog) > 0 {
			out, next = c.events, backlog[0]
		}
		select {
		case frame, ok := <-queue:
			if !ok {
				queue = nil
				continue
			}
			backlog = append(backlog, frame)
		case out <- next:
			backlog[0] = Frame{}
			backlog = backlog[1:]
		}
	}
	close(c.events)
}
