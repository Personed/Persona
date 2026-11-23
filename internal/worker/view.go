package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ViewInput is one view request: open a view, or read from, send to or close
// the open view ID.
type ViewInput struct {
	ID      string       `json:"id,omitempty"`
	Action  string       `json:"action"` // open, next, send, close
	Message *ViewMessage `json:"message,omitempty"`
}

// ViewMessage is a viewer message, as in browser-view.mjs and desktop-view.mjs.
type ViewMessage struct {
	Type string `json:"type"` // ack, text, key, pointer, back
	Seq  int64  `json:"seq,omitempty"`
	// MaxFps, with an ack, limits the browser stream's frame rate.
	MaxFps int    `json:"maxFps,omitempty"`
	Text   string `json:"text,omitempty"`
	Key    string `json:"key,omitempty"`
	// Modifiers is a bit set: 1 Alt, 2 Control, 4 Meta, 8 Shift.
	Modifiers int    `json:"modifiers,omitempty"`
	Event     string `json:"event,omitempty"` // down, up, move, scroll, cancel
	// X and Y are screenshot coordinates of the frame being shown.
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	Button string  `json:"button,omitempty"`
	DeltaX float64 `json:"deltaX,omitempty"`
	DeltaY float64 `json:"deltaY,omitempty"`
}

// ViewFrame is one screen image. The viewer acknowledges Seq before the next
// frame is captured.
type ViewFrame struct {
	Seq      int64  `json:"seq"`
	Data     []byte `json:"data"`
	MimeType string `json:"mimeType"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	// Timestamp is the browser stream's frame time, or the desktop capture
	// time in Unix milliseconds.
	Timestamp float64 `json:"timestamp"`
}

type computerView interface {
	next(ctx context.Context) (*ViewFrame, error)
	send(ctx context.Context, message ViewMessage) error
	close()
}

type openView struct {
	key  string
	view computerView
}

var errViewClosed = codeError("ENOENT", "the view connection is closed; reconnect", "")

func (s *server) view(ctx context.Context, input ComputerInput, key, marker string) (*ComputerResult, error) {
	request := input.View
	if request == nil {
		return nil, codeError("EINVAL", "a view request is required", "")
	}
	if request.Action == "open" {
		id := make([]byte, 16)
		_, _ = rand.Read(id)
		viewID := hex.EncodeToString(id)
		var view computerView
		if input.Service == "browser" {
			browser, err := s.openBrowserView(ctx, input, key, marker)
			if err != nil {
				return nil, err
			}
			view = browser
		} else {
			driver, err := s.computerDriver(ctx, key, input)
			if err != nil {
				return nil, mcpError(ctx, err)
			}
			view = &desktopView{driver: driver, id: viewID, closed: make(chan struct{})}
		}
		s.viewsMu.Lock()
		if s.views == nil {
			s.views = map[string]openView{}
		}
		s.views[viewID] = openView{key: key, view: view}
		s.viewsMu.Unlock()
		return &ComputerResult{ViewID: viewID}, nil
	}
	s.viewsMu.Lock()
	open, ok := s.views[request.ID]
	if ok && open.key != key {
		ok = false
	}
	if ok && request.Action == "close" {
		delete(s.views, request.ID)
	}
	s.viewsMu.Unlock()
	if !ok {
		return nil, errViewClosed
	}
	switch request.Action {
	case "next":
		frame, err := open.view.next(ctx)
		if err != nil {
			return nil, err
		}
		return &ComputerResult{Frame: frame}, nil
	case "send":
		if request.Message == nil {
			return nil, codeError("EINVAL", "a view message is required", "")
		}
		return &ComputerResult{}, open.view.send(ctx, *request.Message)
	case "close":
		open.view.close()
		return &ComputerResult{}, nil
	}
	return nil, codeError("EINVAL", "unknown view action "+request.Action, "")
}

func (s *server) closeViews() {
	s.viewsMu.Lock()
	views := s.views
	s.views = nil
	s.viewsMu.Unlock()
	for _, open := range views {
		open.view.close()
	}
}

var errStaleAck = codeError("EINVAL", "the frame acknowledgement is stale", "")

// desktopView captures the primary display through cua-driver, one frame per
// acknowledgement, and sends viewer input to the same Cua session.
type desktopView struct {
	driver  *computerDriver
	id      string
	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	seq     int64
	acked   chan struct{} // closed when the latest frame is acknowledged
	pointer *ViewMessage
}

func (v *desktopView) next(ctx context.Context) (*ViewFrame, error) {
	v.mu.Lock()
	acked := v.acked
	v.mu.Unlock()
	if acked != nil {
		select {
		case <-acked:
		case <-v.closed:
			return nil, errViewClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-v.closed:
			return nil, errViewClosed
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case <-v.closed:
		return nil, errViewClosed
	default:
	}
	result, err := v.driver.session.session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_desktop_state", Arguments: map[string]any{"session": v.id, "max_image_dimension": 1920},
	})
	if err != nil {
		return nil, mcpError(ctx, err)
	}
	var image *mcp.ImageContent
	for _, part := range result.Content {
		if found, ok := part.(*mcp.ImageContent); ok && image == nil {
			image = found
		}
	}
	if result.IsError || image == nil {
		return nil, codeError("EIO", resultMessage(result, "the graphical session cannot provide a screen"), "")
	}
	var state struct {
		Width  int `json:"screenshot_width"`
		Height int `json:"screenshot_height"`
	}
	if structured, err := json.Marshal(result.StructuredContent); err == nil {
		_ = json.Unmarshal(structured, &state)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seq++
	v.acked = make(chan struct{})
	return &ViewFrame{Seq: v.seq, Data: image.Data, MimeType: image.MIMEType, Width: state.Width, Height: state.Height,
		Timestamp: float64(time.Now().UnixMilli())}, nil
}

func (v *desktopView) call(ctx context.Context, name string, args map[string]any) error {
	args["session"] = v.id
	args["target"] = map[string]any{"kind": "desktop", "display_id": "primary"}
	result, err := v.driver.session.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return mcpError(ctx, err)
	}
	if result.IsError {
		return codeError("EIO", resultMessage(result, "the computer did not confirm the input; check the screen"), "")
	}
	return nil
}

func (v *desktopView) send(ctx context.Context, m ViewMessage) error {
	select {
	case <-v.closed:
		return errViewClosed
	default:
	}
	v.mu.Lock()
	if m.Type == "back" || m.Type == "pointer" && m.Event == "cancel" {
		v.pointer = nil
		v.mu.Unlock()
		return nil
	}
	if m.Type == "ack" {
		defer v.mu.Unlock()
		if m.Seq != v.seq || v.acked == nil {
			return errStaleAck
		}
		select {
		case <-v.acked:
		default:
			close(v.acked)
		}
		return nil
	}
	start := v.pointer
	if m.Type == "pointer" && m.Event == "down" {
		message := m
		v.pointer = &message
	}
	if m.Type == "pointer" && m.Event == "up" {
		v.pointer = nil
	}
	held := v.pointer != nil
	v.mu.Unlock()
	switch m.Type {
	case "text":
		return v.call(ctx, "type_text", map[string]any{"text": m.Text})
	case "key":
		var keys []string
		for _, modifier := range []struct {
			bit int
			key string
		}{{1, "alt"}, {2, "ctrl"}, {4, "cmd"}, {8, "shift"}} {
			if m.Modifiers&modifier.bit != 0 {
				keys = append(keys, modifier.key)
			}
		}
		if len(keys) > 0 {
			return v.call(ctx, "hotkey", map[string]any{"keys": append(keys, m.Key)})
		}
		return v.call(ctx, "press_key", map[string]any{"key": m.Key})
	case "pointer":
		switch m.Event {
		case "up":
			if start == nil {
				return nil
			}
			if math.Hypot(start.X-m.X, start.Y-m.Y) > 3 {
				return v.call(ctx, "drag", map[string]any{"from_x": start.X, "from_y": start.Y, "to_x": m.X, "to_y": m.Y, "button": m.Button})
			}
			return v.call(ctx, "click", map[string]any{"x": m.X, "y": m.Y, "button": m.Button})
		case "move":
			if held {
				return nil
			}
			return v.call(ctx, "move_cursor", map[string]any{"x": m.X, "y": m.Y})
		case "scroll":
			direction := "down"
			if m.DeltaY < 0 {
				direction = "up"
			}
			return v.call(ctx, "scroll", map[string]any{"x": m.X, "y": m.Y, "direction": direction,
				"amount": max(1, int(math.Round(math.Abs(m.DeltaY)/40)))})
		}
	}
	return nil
}

func (v *desktopView) close() {
	v.once.Do(func() { close(v.closed) })
}

func resultMessage(result *mcp.CallToolResult, fallback string) string {
	for _, part := range result.Content {
		if text, ok := part.(*mcp.TextContent); ok && text.Text != "" {
			return text.Text
		}
	}
	return fallback
}

// maxBrowserFrame bounds one browser stream message, as browser-view.mjs does.
const maxBrowserFrame = 4 << 20

// browserView reads agent-browser's viewport stream for the Bot session and
// sends viewer input through its command API, as browser-view.mjs does.
type browserView struct {
	server  *server
	key     string
	marker  string
	port    int
	conn    *websocket.Conn
	stop    context.CancelFunc
	frames  chan struct{}
	failed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	latest  *ViewFrame
	sent    int64
	pointer *browserPointer
}

type browserPointer struct {
	x, y   int
	button string
}

var errBrowserViewLost = codeError("ECONNRESET", "the browser view disconnected; reconnect", "")

func (s *server) openBrowserView(ctx context.Context, input ComputerInput, key, marker string) (*browserView, error) {
	config, err := driverConfig(input)
	if err != nil {
		return nil, err
	}
	output, err := browserCLI(ctx, config, "stream", "status", "--json")
	if err != nil {
		return nil, codeError("EIO", "the browser stream is not available: "+err.Error(), "")
	}
	type streamStatus struct {
		Enabled bool            `json:"enabled"`
		Port    json.RawMessage `json:"port"`
	}
	var response struct {
		Data *streamStatus `json:"data"`
		streamStatus
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, codeError("EIO", "the browser stream status is not JSON", "")
	}
	status := response.streamStatus
	if response.Data != nil {
		status = *response.Data
	}
	port, err := strconv.Atoi(string(status.Port))
	if !status.Enabled || err != nil || port < 1 || port > 65535 {
		return nil, codeError("EIO", "the browser stream is not enabled", "")
	}
	identity, err := browserCommand(ctx, port, map[string]any{"action": "session_info"})
	if err != nil {
		return nil, err
	}
	var session struct {
		Session string `json:"session"`
	}
	if json.Unmarshal(identity, &session) != nil || session.Session != config.Env["AGENT_BROWSER_SESSION"] {
		return nil, codeError("EIO", "the browser stream belongs to another browser session", "")
	}
	dial, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dial, fmt.Sprintf("ws://127.0.0.1:%d/?pacing=ack&maxFps=30", port), nil)
	if err != nil {
		return nil, codeError("EIO", "the browser view did not connect: "+err.Error(), "")
	}
	// Larger messages are skipped, as browser-view.mjs does, not fatal.
	conn.SetReadLimit(-1)
	read, stop := context.WithCancel(context.Background())
	view := &browserView{server: s, key: key, marker: marker, port: port, conn: conn, stop: stop,
		frames: make(chan struct{}, 1), failed: make(chan struct{})}
	go view.read(read)
	return view, nil
}

func (v *browserView) fail() {
	v.once.Do(func() {
		close(v.failed)
		v.stop()
		_ = v.conn.Close(websocket.StatusNormalClosure, "")
	})
}

func (v *browserView) read(ctx context.Context) {
	defer v.fail()
	for {
		kind, reader, err := v.conn.Reader(ctx)
		if err != nil {
			return
		}
		data, err := io.ReadAll(io.LimitReader(reader, maxBrowserFrame+1))
		if err != nil {
			return
		}
		if kind != websocket.MessageText || len(data) > maxBrowserFrame {
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return
			}
			continue
		}
		var frame struct {
			Type     string          `json:"type"`
			Data     json.RawMessage `json:"data"`
			Seq      json.RawMessage `json:"seq"`
			Metadata struct {
				Width     int     `json:"deviceWidth"`
				Height    int     `json:"deviceHeight"`
				Timestamp float64 `json:"timestamp"`
			} `json:"metadata"`
		}
		if json.Unmarshal(data, &frame) != nil {
			return
		}
		var encoded string
		seq, seqErr := strconv.ParseInt(string(frame.Seq), 10, 64)
		if frame.Type != "frame" || json.Unmarshal(frame.Data, &encoded) != nil || seqErr != nil || seq > 1<<53-1 || seq < -(1<<53-1) {
			continue
		}
		image, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return
		}
		v.mu.Lock()
		v.latest = &ViewFrame{Seq: seq, Data: image, MimeType: "image/jpeg", Width: frame.Metadata.Width,
			Height: frame.Metadata.Height, Timestamp: frame.Metadata.Timestamp}
		v.mu.Unlock()
		select {
		case v.frames <- struct{}{}:
		default:
		}
	}
}

func (v *browserView) next(ctx context.Context) (*ViewFrame, error) {
	for {
		v.mu.Lock()
		if frame := v.latest; frame != nil {
			v.latest = nil
			v.sent = frame.Seq
			v.mu.Unlock()
			return frame, nil
		}
		v.mu.Unlock()
		select {
		case <-v.failed:
			return nil, errBrowserViewLost
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-v.frames:
		}
	}
}

func (v *browserView) send(ctx context.Context, m ViewMessage) error {
	select {
	case <-v.failed:
		return errBrowserViewLost
	default:
	}
	if m.Type == "ack" {
		v.mu.Lock()
		sent := v.sent
		v.mu.Unlock()
		if m.Seq != sent {
			return errStaleAck
		}
		messages := []map[string]any{{"type": "ack", "seq": m.Seq}}
		if m.MaxFps > 0 {
			messages = append(messages, map[string]any{"type": "config", "maxFps": m.MaxFps})
		}
		for _, message := range messages {
			data, _ := json.Marshal(message)
			if err := v.conn.Write(ctx, websocket.MessageText, data); err != nil {
				v.fail()
				return errBrowserViewLost
			}
		}
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	switch m.Type {
	case "back":
		return v.release(ctx, true)
	case "text":
		return v.input(ctx, map[string]any{"action": "keyboard", "subaction": "insertText", "text": m.Text})
	case "key":
		if m.Modifiers == 4 && strings.ToLower(m.Key) == "a" {
			return v.input(ctx, map[string]any{"action": "selectall", "selector": ":focus"})
		}
		var keys []string
		for _, modifier := range []struct {
			bit int
			key string
		}{{1, "Alt"}, {2, "Control"}, {4, "Meta"}, {8, "Shift"}} {
			if m.Modifiers&modifier.bit != 0 {
				keys = append(keys, modifier.key)
			}
		}
		return v.input(ctx, map[string]any{"action": "press", "key": strings.Join(append(keys, m.Key), "+")})
	case "pointer":
		x, y := int(math.Round(m.X)), int(math.Round(m.Y))
		switch m.Event {
		case "cancel":
			return v.release(ctx, true)
		case "up":
			if v.pointer != nil {
				v.pointer.x, v.pointer.y = x, y
			}
			return v.release(ctx, false)
		}
		if m.Event == "down" {
			v.pointer = &browserPointer{button: m.Button}
		}
		button := "none"
		if v.pointer != nil {
			v.pointer.x, v.pointer.y = x, y
			button = v.pointer.button
		}
		kind := "mouseMoved"
		switch m.Event {
		case "down":
			kind = "mousePressed"
		case "scroll":
			kind = "mouseWheel"
		}
		command := map[string]any{"action": "input_mouse", "type": kind, "x": x, "y": y,
			"modifiers": m.Modifiers, "button": button, "clickCount": 1}
		if m.Event == "scroll" {
			command["deltaX"] = int(math.Round(m.DeltaX))
			command["deltaY"] = int(math.Round(m.DeltaY))
		}
		return v.input(ctx, command)
	}
	return nil
}

// release lifts a held pointer; a cancelled one is released off the page.
func (v *browserView) release(ctx context.Context, cancelled bool) error {
	if v.pointer == nil {
		return nil
	}
	x, y := v.pointer.x, v.pointer.y
	if cancelled {
		x, y = -1, -1
	}
	err := v.input(ctx, map[string]any{"action": "input_mouse", "type": "mouseReleased", "x": x, "y": y,
		"button": v.pointer.button, "clickCount": 1})
	if err == nil {
		v.pointer = nil
	}
	return err
}

// input sends one viewer command after the browser's dispatched actions
// settle. A command whose delivery is unknown makes the browser uncertain,
// like an agent action.
func (v *browserView) input(ctx context.Context, command map[string]any) error {
	lane := v.server.computerLane(v.key)
	lane.Lock()
	defer lane.Unlock()
	if _, err := os.Stat(v.marker); err == nil {
		return errBrowserUncertain
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.WriteFile(v.marker, []byte("view input"), 0o600); err != nil {
		return err
	}
	send, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := browserCommand(send, v.port, command); err != nil {
		return errBrowserUncertain
	}
	return os.Remove(v.marker)
}

func (v *browserView) close() {
	v.mu.Lock()
	if v.pointer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = v.release(ctx, true)
		cancel()
	}
	v.mu.Unlock()
	v.fail()
}

var browserHTTP = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// browserCommand posts one command to agent-browser's loopback command API
// and returns its confirmed data.
func browserCommand(ctx context.Context, port int, command map[string]any) (json.RawMessage, error) {
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	body, _ := json.Marshal(command)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/command", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := browserHTTP.Do(request)
	if err != nil {
		return nil, codeError("EIO", "the browser did not confirm the input; check the page", "")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return nil, codeError("EIO", "the browser input receipt is too large", "")
	}
	var result struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &result) != nil || response.StatusCode != http.StatusOK || !result.Success {
		return nil, codeError("EIO", "the browser did not confirm the input; check the page", "")
	}
	return result.Data, nil
}

// ComputerView is an open screen view of a computer service.
type ComputerView struct {
	client  *Client
	dir     string
	service string
	id      string
}

func (v *ComputerView) request(ctx context.Context, view ViewInput) (ComputerResult, error) {
	view.ID = v.id
	return v.client.computerCall(ctx, ComputerInput{Action: "view", Service: v.service, Dir: v.dir, View: &view})
}

// OpenView connects to service's screen: agent-browser's viewport stream for
// the Bot browser, or cua-driver captures of the primary display.
func (c *Client) OpenView(ctx context.Context, dir, service string) (*ComputerView, error) {
	result, err := c.computerCall(ctx, ComputerInput{Action: "view", Service: service, Dir: dir, View: &ViewInput{Action: "open"}})
	if err != nil {
		return nil, err
	}
	return &ComputerView{client: c, dir: dir, service: service, id: result.ViewID}, nil
}

// Next waits for the newest frame. Acknowledge it with Send before the next.
func (v *ComputerView) Next(ctx context.Context) (ViewFrame, error) {
	result, err := v.request(ctx, ViewInput{Action: "next"})
	if err != nil || result.Frame == nil {
		return ViewFrame{}, err
	}
	return *result.Frame, nil
}

// Send delivers one viewer message: a frame acknowledgement or user input.
func (v *ComputerView) Send(ctx context.Context, message ViewMessage) error {
	_, err := v.request(ctx, ViewInput{Action: "send", Message: &message})
	return err
}

// Close releases a held pointer and disconnects the view.
func (v *ComputerView) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := v.request(ctx, ViewInput{Action: "close"})
	return err
}
