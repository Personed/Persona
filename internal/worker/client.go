package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Personed/Persona/internal/computer"
)

// Client is one connection to a worker. It implements computer.Target. Requests
// of a lost connection fail with an Error whose Lost field is set; handles belong
// to the connection, so callers dial again instead of reusing them.
type Client struct {
	info    computer.Info
	hello   Hello
	reader  *bufio.Reader
	writer  io.WriteCloser
	writing sync.Mutex
	mu      sync.Mutex
	next    uint32
	calls   map[uint32]*pending
	done    chan struct{}
	err     error
	cmd     *exec.Cmd
	closed  atomic.Bool
	options Options
}

type pending struct {
	event  func(json.RawMessage, []byte)
	result chan frame
}

// Options names the target a client connects to.
type Options struct {
	ID   string
	Name string
	Kind computer.Kind
	// MCPNotification receives notifications from connected MCP servers.
	MCPNotification func(id string, notification MCPNotification)
	// WorkDir is the Bot work directory that Execute and Observe use.
	WorkDir string
	// ComputerDrivers replaces the desktop or browser driver, by service.
	ComputerDrivers map[string]*MCPConfig
}

// Transport builds the command that starts a worker printing token on its sync line.
type Transport func(token string) []string

// Local starts the worker binary as a child process of the service.
func Local(binary string) Transport {
	return func(token string) []string { return []string{binary, "worker", "serve", "--token", token} }
}

// Dial starts a worker through transport and returns a connected client.
// Closing the client ends the worker and every program it started.
func Dial(ctx context.Context, transport Transport, options Options) (*Client, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	argv := transport(token)
	cmd := exec.Command(argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{buffer: &stderr, limit: 64 << 10}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("worker: start %s: %w", argv[0], err)
	}
	client, err := Connect(ctx, stdout, stdin, token, options)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			err = fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	client.cmd = cmd
	return client, nil
}

// Connect speaks the protocol over an established stream: it waits for the
// sync line carrying token, then sends hello.
func Connect(ctx context.Context, in io.Reader, out io.WriteCloser, token string, options Options) (*Client, error) {
	c := &Client{options: options,
		reader: bufio.NewReaderSize(in, 64<<10), writer: out,
		calls: map[uint32]*pending{}, done: make(chan struct{}),
	}
	synced := make(chan error, 1)
	go func() { synced <- c.sync(token) }()
	select {
	case err := <-synced:
		if err != nil {
			_ = out.Close()
			return nil, err
		}
	case <-ctx.Done():
		_ = out.Close()
		return nil, ctx.Err()
	}
	go c.readLoop()
	go c.pingLoop()
	raw, _, err := c.call(ctx, "hello", map[string]int{"protocol": Protocol}, nil, nil)
	if err == nil {
		err = json.Unmarshal(raw, &c.hello)
	}
	if err == nil && c.hello.Protocol != Protocol {
		err = fmt.Errorf("worker: protocol %d, want %d", c.hello.Protocol, Protocol)
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	c.info = computer.Info{
		ID: options.ID, Name: options.Name, Kind: options.Kind,
		OS: c.hello.OS, Arch: c.hello.Arch, User: c.hello.User, Home: c.hello.Home,
	}
	return c, nil
}

// sync discards what a login shell prints before the worker's sync line.
func (c *Client) sync(token string) error {
	want := syncPrefix + token
	discarded := 0
	for {
		line, err := c.reader.ReadString('\n')
		if strings.TrimRight(line, "\r\n") == want {
			return nil
		}
		if err != nil {
			return fmt.Errorf("worker: no sync line: %w", err)
		}
		if discarded += len(line); discarded > 1<<20 {
			return errors.New("worker: no sync line in the first MiB")
		}
	}
}

func newToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func (c *Client) readLoop() {
	var err error
	for {
		var f frame
		if f, err = readFrame(c.reader); err != nil {
			break
		}
		c.mu.Lock()
		call := c.calls[f.ID]
		if f.Type == frameResult || f.Type == frameError {
			delete(c.calls, f.ID)
		}
		c.mu.Unlock()
		switch {
		case call == nil && f.Type == frameEvent && f.ID == 0:
			c.workerEvent(f.JSON)
		case call == nil:
		case f.Type == frameEvent && call.event != nil:
			call.event(f.JSON, f.Payload)
		case f.Type == frameResult || f.Type == frameError:
			call.result <- f
		}
	}
	c.fail(err)
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = errors.New("worker connection closed")
	}
	c.err = &Error{Code: "unknown", Message: err.Error(), Lost: true}
	close(c.done)
}

func (c *Client) pingLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			_ = c.send(framePing, 0, nil, nil)
		}
	}
}

func (c *Client) send(kind byte, id uint32, value any, payload []byte) error {
	data, err := encodeFrame(kind, id, value, payload)
	if err != nil {
		return err
	}
	c.writing.Lock()
	defer c.writing.Unlock()
	_, err = c.writer.Write(data)
	return err
}

// call sends op with args and waits for its result. Events of the request go
// to event. Cancelling ctx sends a cancel frame and waits for the worker's answer.
func (c *Client) call(ctx context.Context, op string, args any, payload []byte, event func(json.RawMessage, []byte)) (json.RawMessage, []byte, error) {
	id, call, err := c.begin(op, args, payload, event)
	if err != nil {
		return nil, nil, err
	}
	return c.await(ctx, id, call)
}

func (c *Client) begin(op string, args any, payload []byte, event func(json.RawMessage, []byte)) (uint32, *pending, error) {
	body, err := withOp(op, args)
	if err != nil {
		return 0, nil, err
	}
	call := &pending{event: event, result: make(chan frame, 1)}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return 0, nil, c.err
	}
	c.next++
	id := c.next
	c.calls[id] = call
	c.mu.Unlock()
	if err := c.send(frameRequest, id, body, payload); err != nil {
		c.forget(id)
		return 0, nil, err
	}
	return id, call, nil
}

func (c *Client) await(ctx context.Context, id uint32, call *pending) (json.RawMessage, []byte, error) {
	cancelled := ctx.Done()
	for {
		select {
		case f := <-call.result:
			if f.Type == frameError {
				wire := &Error{}
				if err := json.Unmarshal(f.JSON, wire); err != nil || wire.Code == "" {
					wire = &Error{Code: "unknown", Message: string(f.JSON)}
				}
				return nil, nil, wire
			}
			return f.JSON, f.Payload, nil
		case <-cancelled:
			cancelled = nil
			_ = c.send(frameCancel, id, nil, nil)
		case <-c.done:
			c.forget(id)
			return nil, nil, c.err
		}
	}
}

func (c *Client) forget(id uint32) {
	c.mu.Lock()
	delete(c.calls, id)
	c.mu.Unlock()
}

func withOp(op string, args any) (map[string]any, error) {
	body := map[string]any{}
	if args != nil {
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, err
		}
	}
	body["op"] = op
	return body, nil
}

// Hello returns what the worker reported when the connection opened.
func (c *Client) Hello() Hello { return c.hello }

// Info implements computer.Target.
func (c *Client) Info(context.Context) (computer.Info, error) { return c.info, nil }

// Lstat returns path's metadata without following a final symbolic link.
func (c *Client) Lstat(ctx context.Context, path string) (FileInfo, error) {
	raw, _, err := c.call(ctx, "lstat", map[string]string{"path": path}, nil, nil)
	var result struct {
		Info FileInfo `json:"info"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result.Info, err
}

// Stat implements computer.Target with lstat semantics.
func (c *Client) Stat(ctx context.Context, path string) (computer.FileInfo, error) {
	info, err := c.Lstat(ctx, path)
	if err != nil {
		return computer.FileInfo{}, err
	}
	return computer.FileInfo{
		Path: path, Size: info.Size, Dir: info.Kind == "directory",
		ModTime: info.MtimeSec*1000 + info.MtimeNsec/1_000_000,
		Symlink: info.Kind == "symlink", Links: int(info.Links), //nolint:gosec // Link counts fit in int.
	}, nil
}

// Realpath resolves every symbolic link in path.
func (c *Client) Realpath(ctx context.Context, path string) (string, error) {
	raw, _, err := c.call(ctx, "realpath", map[string]string{"path": path}, nil, nil)
	var result struct {
		Path string `json:"path"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result.Path, err
}

// Mkdir creates path; recursive creates missing parents and accepts an existing directory.
func (c *Client) Mkdir(ctx context.Context, path string, recursive bool) error {
	_, _, err := c.call(ctx, "mkdir", pathRequest{Path: path, Recursive: recursive}, nil, nil)
	return err
}

// Remove deletes path. A directory needs recursive; force accepts a missing path.
func (c *Client) Remove(ctx context.Context, path string, recursive, force bool) error {
	_, _, err := c.call(ctx, "rm", pathRequest{Path: path, Recursive: recursive, Force: force}, nil, nil)
	return err
}

// Rename moves from to to.
func (c *Client) Rename(ctx context.Context, from, to string) error {
	_, _, err := c.call(ctx, "rename", pathRequest{Path: from, To: to}, nil, nil)
	return err
}

// DirEntry is one readdir entry with its lstat result or error.
type DirEntry struct {
	Name  string    `json:"name"`
	Info  *FileInfo `json:"info,omitempty"`
	Error *Error    `json:"error,omitempty"`
}

// ReadDir lists path's entries in directory order.
func (c *Client) ReadDir(ctx context.Context, path string) ([]DirEntry, error) {
	raw, _, err := c.call(ctx, "opendir", map[string]string{"path": path}, nil, nil)
	if err != nil {
		return nil, err
	}
	var handle struct {
		Handle uint32 `json:"handle"`
	}
	if err := json.Unmarshal(raw, &handle); err != nil {
		return nil, err
	}
	defer c.closeHandle(handle.Handle)
	var entries []DirEntry
	for {
		raw, _, err := c.call(ctx, "readdir", handleRequest{Handle: handle.Handle, Max: 1024}, nil, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Entries []DirEntry `json:"entries"`
			Done    bool       `json:"done"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		entries = append(entries, page.Entries...)
		if page.Done {
			return entries, nil
		}
	}
}

func (c *Client) closeHandle(handle uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, _ = c.call(ctx, "close", handleRequest{Handle: handle}, nil, nil)
}

// ReadFile implements computer.Target, streaming the file in chunks.
func (c *Client) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	raw, _, err := c.call(ctx, "open", pathRequest{Path: path}, nil, nil)
	if err != nil {
		return nil, err
	}
	var opened struct {
		Handle uint32 `json:"handle"`
	}
	if err := json.Unmarshal(raw, &opened); err != nil {
		return nil, err
	}
	return &fileReader{ctx: ctx, client: c, handle: opened.Handle}, nil
}

const chunkSize = 512 << 10

type fileReader struct {
	ctx    context.Context
	client *Client
	handle uint32
	offset int64
	buffer []byte
	eof    bool
	once   sync.Once
}

func (r *fileReader) Read(p []byte) (int, error) {
	for len(r.buffer) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		_, data, err := r.client.call(r.ctx, "pread", handleRequest{Handle: r.handle, Offset: r.offset, Length: chunkSize}, nil, nil)
		if err != nil {
			return 0, err
		}
		r.offset += int64(len(data))
		r.buffer, r.eof = data, len(data) < chunkSize
	}
	n := copy(p, r.buffer)
	r.buffer = r.buffer[n:]
	return n, nil
}

func (r *fileReader) Close() error {
	r.once.Do(func() { r.client.closeHandle(r.handle) })
	return nil
}

// WriteFile implements computer.Target, creating missing parent directories.
func (c *Client) WriteFile(ctx context.Context, path string, data io.Reader) error {
	buffer := make([]byte, chunkSize)
	n, err := io.ReadFull(data, buffer)
	last := err != nil
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	raw, _, err := c.call(ctx, "write", pathRequest{Path: path, Keep: !last}, buffer[:n], nil)
	if err != nil || last {
		return err
	}
	var opened struct {
		Handle uint32 `json:"handle"`
	}
	if err := json.Unmarshal(raw, &opened); err != nil {
		return err
	}
	defer c.closeHandle(opened.Handle)
	for {
		n, err := io.ReadFull(data, buffer)
		if n > 0 {
			if _, _, err := c.call(ctx, "writeChunk", handleRequest{Handle: opened.Handle}, buffer[:n], nil); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ListOptions bounds ListFiles. Zero values use the worker's defaults.
type ListOptions struct {
	NoHidden    bool
	NoGitignore bool
	Exclude     []string
	MaxEntries  int
	MaxBytes    int
	Timeout     time.Duration
}

// ListEntry is one ListFiles entry. Kind is 'f', 'd' or 'l'.
type ListEntry struct {
	Kind byte
	Path string
}

// ListFiles lists the tree below dir depth first, siblings in byte order.
func (c *Client) ListFiles(ctx context.Context, dir string, options ListOptions) ([]ListEntry, bool, error) {
	hidden, gitignore := !options.NoHidden, !options.NoGitignore
	raw, payload, err := c.call(ctx, "listFiles", listRequest{
		Path: dir, Hidden: &hidden, Gitignore: &gitignore, Exclude: options.Exclude,
		MaxEntries: options.MaxEntries, MaxBytes: options.MaxBytes, TimeoutMs: int(options.Timeout / time.Millisecond),
	}, nil, nil)
	if err != nil {
		return nil, false, err
	}
	var result listResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, false, err
	}
	entries := make([]ListEntry, 0, result.Count)
	for _, record := range bytes.Split(payload, []byte{0}) {
		if len(record) > 1 {
			entries = append(entries, ListEntry{Kind: record[0], Path: string(record[1:])})
		}
	}
	return entries, result.Truncated, nil
}

// ExecOptions configures Exec. Command runs through a shell; Argv runs directly.
type ExecOptions struct {
	Command   string
	Argv      []string
	Dir       string
	Env       map[string]string
	NoInherit bool
	ShellPath string
	Timeout   time.Duration
	// Output receives decoded text of stdout or stderr as it arrives.
	Output func(stream string, text []byte)
}

// Exec runs a command to completion and returns its exit code; a signal
// returns 128 plus the signal number. Cancelling ctx kills the process group
// and fails with code aborted; the timeout fails with code timeout.
func (c *Client) Exec(ctx context.Context, options ExecOptions) (int, error) {
	inherit := !options.NoInherit
	raw, _, err := c.call(ctx, "exec", execRequest{
		Command: options.Command, Argv: options.Argv, Cwd: options.Dir, Env: options.Env,
		InheritEnv: &inherit, ShellPath: options.ShellPath, TimeoutMs: int(options.Timeout / time.Millisecond),
	}, nil, func(raw json.RawMessage, payload []byte) {
		var event outputEvent
		if options.Output != nil && json.Unmarshal(raw, &event) == nil && event.Kind == "output" {
			options.Output(event.Stream, payload)
		}
	})
	return exitResult(raw, err)
}

func exitResult(raw json.RawMessage, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	var result struct {
		ExitCode int `json:"exitCode"`
	}
	err = json.Unmarshal(raw, &result)
	return result.ExitCode, err
}

// Start implements computer.Target: it runs Path with Args directly, with plain
// pipes, and kills what the program left in its process group when it exits.
func (c *Client) Start(ctx context.Context, command computer.Command) (computer.Process, error) {
	inherit := true
	return c.start(ctx, "process", execRequest{
		Argv: append([]string{command.Path}, command.Args...), Cwd: command.Dir, Env: command.Env, InheritEnv: &inherit,
	}, command)
}

// LaunchOptions starts a native Harness process. Program defaults to
// `pi --mode rpc`. User names the OS user when the worker runs as root.
type LaunchOptions struct {
	Program     string
	Args        []string
	Dir         string
	RuntimeHome string
	Env         map[string]string
	User        string
	// Gateway is the product tool endpoint mounted on this worker.
	Gateway *Gateway
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// Launch runs a Harness program as the work computer's OS user, with its account
// environment and the program its login shell resolves. Kill stops the whole tree.
func (c *Client) Launch(ctx context.Context, options LaunchOptions) (computer.Process, error) {
	env := options.Env
	if options.Gateway != nil {
		if options.Gateway.owner != c {
			return nil, errors.New("worker: gateway belongs to another connection")
		}
		select {
		case <-options.Gateway.done:
			return nil, errors.New("worker: gateway is closed")
		default:
		}
		env = make(map[string]string, len(options.Env)+1)
		for key, value := range options.Env {
			env[key] = value
		}
		env["PERSONA_PI_MCP_URL"] = options.Gateway.URL
	}
	return c.start(ctx, "launch", launchRequest{
		Program: options.Program, Args: options.Args, Cwd: options.Dir,
		RuntimeHome: options.RuntimeHome, Env: env, User: options.User,
	}, computer.Command{Stdin: options.Stdin, Stdout: options.Stdout, Stderr: options.Stderr})
}

// Resolve returns the path of command for the work computer's OS user, or ""
// when it is not installed.
func (c *Client) Resolve(ctx context.Context, command, runtimeHome string) (string, error) {
	raw, _, err := c.call(ctx, "resolve", resolveRequest{Command: command, RuntimeHome: runtimeHome}, nil, nil)
	var result struct {
		Path string `json:"path"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result.Path, err
}

func (c *Client) start(ctx context.Context, op string, args any, command computer.Command) (computer.Process, error) {
	p := &process{client: c, started: make(chan struct{}), done: make(chan struct{})}
	id, call, err := c.begin(op, args, nil, func(raw json.RawMessage, payload []byte) {
		var event outputEvent
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		switch {
		case event.Kind == "started":
			p.once.Do(func() { close(p.started) })
		case event.Stream == "stdout" && command.Stdout != nil:
			_, _ = command.Stdout.Write(payload)
		case event.Stream == "stderr" && command.Stderr != nil:
			_, _ = command.Stderr.Write(payload)
		}
	})
	if err != nil {
		return nil, err
	}
	p.id = id
	go func() {
		defer close(p.done)
		raw, _, err := c.await(context.Background(), id, call)
		p.code, p.err = exitResult(raw, err)
	}()
	select {
	case <-p.started:
	case <-p.done:
		return nil, p.err
	case <-ctx.Done():
		_ = p.Kill()
		<-p.done
		return nil, ctx.Err()
	}
	go p.feed(command.Stdin)
	return p, nil
}

type process struct {
	client  *Client
	id      uint32
	started chan struct{}
	once    sync.Once
	done    chan struct{}
	code    int
	err     error
}

func (p *process) feed(stdin io.Reader) {
	ctx := context.Background()
	if stdin == nil {
		_, _, _ = p.client.call(ctx, "processCloseStdin", map[string]uint32{"target": p.id}, nil, nil)
		return
	}
	buffer := make([]byte, 64<<10)
	for {
		n, err := stdin.Read(buffer)
		if n > 0 {
			if _, _, werr := p.client.call(ctx, "processWrite", map[string]uint32{"target": p.id}, buffer[:n], nil); werr != nil {
				return
			}
		}
		if err != nil {
			_, _, _ = p.client.call(ctx, "processCloseStdin", map[string]uint32{"target": p.id}, nil, nil)
			return
		}
	}
}

func (p *process) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

// Kill kills the program's process group; Wait then returns its status.
func (p *process) Kill() error {
	return p.client.send(frameCancel, p.id, map[string]string{"mode": "kill"}, nil)
}

// Execute implements computer.Target for Options.WorkDir. It journals the action
// under a fresh call ID; task callers use Act with the engine's call ID.
func (c *Client) Execute(ctx context.Context, action computer.Action) (computer.Observation, error) {
	callID, err := newToken()
	if err != nil {
		return computer.Observation{}, err
	}
	return c.Act(ctx, ComputerCall{Dir: c.options.WorkDir, CallID: callID, Action: action})
}

// Observe implements computer.Target with the driver's native observation:
// get_desktop_state for the desktop, agent_browser_snapshot for the browser.
func (c *Client) Observe(ctx context.Context, service string) (computer.Observation, error) {
	name := "get_desktop_state"
	if service == "browser" {
		name = "agent_browser_snapshot"
	}
	return c.Act(ctx, ComputerCall{Dir: c.options.WorkDir, Action: computer.Action{Service: service, Name: name}})
}

// Close ends the connection. The worker then kills every program it started.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	err := c.writer.Close()
	if c.cmd != nil {
		exited := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = c.cmd.Process.Kill()
			<-exited
		}
	}
	c.fail(nil)
	return err
}

type limitedWriter struct {
	buffer *bytes.Buffer
	limit  int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buffer.Len(); room > 0 {
		w.buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *Client) workerEvent(raw json.RawMessage) {
	var event mcpEvent
	if json.Unmarshal(raw, &event) == nil && event.Type == "mcpNotification" && event.Notification != nil && c.options.MCPNotification != nil {
		c.options.MCPNotification(event.ID, *event.Notification)
	}
}

// MCP runs one managed MCP operation on the work computer. progress receives
// the server's progress notifications for a forwarded request.
func (c *Client) MCP(ctx context.Context, input MCPInput, progress func(MCPProgress)) (MCPResult, error) {
	var result MCPResult
	raw, _, err := c.call(ctx, "mcp", input, nil, func(raw json.RawMessage, _ []byte) {
		var event mcpEvent
		if progress != nil && json.Unmarshal(raw, &event) == nil && event.Type == "progress" && event.Progress != nil {
			progress(*event.Progress)
		}
	})
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}
