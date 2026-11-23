package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Version is reported by hello; the CLI sets it from the build version.
var Version = "dev"

// Features lists operations added after the first release of protocol 1.
var Features = []string{"listFiles", "process", "launch", "mcp", "gateway", "computer"}

const (
	pingInterval = 5 * time.Second
	idleLimit    = 30 * time.Second
)

// Hello describes the work computer to the service.
type Hello struct {
	Protocol int      `json:"protocol"`
	Version  string   `json:"version"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	User     string   `json:"user"`
	Home     string   `json:"home"`
	Tmpdir   string   `json:"tmpdir"`
	Cwd      string   `json:"cwd"`
	PID      int      `json:"pid"`
	Features []string `json:"features"`
}

type server struct {
	out      io.Writer
	writing  sync.Mutex
	handles  handles
	mu       sync.Mutex
	requests map[uint32]*request
	// mcpSessions holds connected managed MCP servers by connection id.
	mcpSessions   map[string]*mcpSession
	computersMu   sync.Mutex
	computers     map[string]*computerDriver
	computerLanes map[string]*sync.Mutex
	viewsMu       sync.Mutex
	views         map[string]openView
	wg            sync.WaitGroup
	lastRead      atomic.Int64
}

type request struct {
	cancel  context.CancelFunc
	kill    chan struct{}
	once    sync.Once
	input   chan []byte
	closed  bool
	gateway *gatewayStream
}

type operation struct {
	Op string `json:"op"`
}

// Serve answers requests read from in until in ends, ctx ends, or 30 seconds pass
// without input. It first writes the sync line with token. On return every
// program it started has been killed and every handle closed.
func Serve(ctx context.Context, in io.Reader, out io.Writer, token string) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	s := &server{out: out, handles: handles{open: map[uint32]*os.File{}}, requests: map[uint32]*request{}, mcpSessions: map[string]*mcpSession{}, computers: map[string]*computerDriver{}}
	defer s.handles.closeAll()
	defer s.closeMCP()
	defer s.closeComputers()
	defer s.wg.Wait()
	defer s.cancelAll()
	if _, err := io.WriteString(out, syncPrefix+token+"\n"); err != nil {
		return err
	}
	s.lastRead.Store(time.Now().UnixNano())
	frames := make(chan frame)
	failed := make(chan error, 1)
	go func() {
		reader := bufio.NewReaderSize(readerFunc(func(p []byte) (int, error) {
			n, err := in.Read(p)
			if n > 0 {
				s.lastRead.Store(time.Now().UnixNano())
			}
			return n, err
		}), 64<<10)
		for {
			f, err := readFrame(reader)
			if err != nil {
				failed <- err
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-ticker.C:
			if time.Since(time.Unix(0, s.lastRead.Load())) > idleLimit {
				return errors.New("worker: no input for 30 seconds")
			}
			s.send(framePing, 0, nil, nil)
		case f := <-frames:
			s.dispatch(ctx, f)
		}
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func (s *server) send(kind byte, id uint32, value any, payload []byte) {
	data, err := encodeFrame(kind, id, value, payload)
	if err != nil {
		data, _ = encodeFrame(frameError, id, toError(err), nil)
	}
	s.writing.Lock()
	defer s.writing.Unlock()
	_, _ = s.out.Write(data)
}

func (s *server) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		r.cancel()
	}
}

// dispatch registers a request before it runs, so a cancel that follows at once
// is never lost. Writes to a running process are answered in arrival order.
func (s *server) dispatch(ctx context.Context, f frame) {
	switch f.Type {
	case framePing:
		return
	case frameCancel:
		var mode struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(f.JSON, &mode)
		s.mu.Lock()
		r := s.requests[f.ID]
		s.mu.Unlock()
		if r == nil {
			return
		}
		if mode.Mode == "kill" {
			r.once.Do(func() { close(r.kill) })
		} else {
			r.cancel()
		}
		return
	case frameRequest:
	default:
		s.send(frameError, f.ID, codeError("EINVAL", "unexpected frame type", ""), nil)
		return
	}
	op, err := decode[operation](f.JSON)
	if err != nil {
		s.send(frameError, f.ID, toError(err), nil)
		return
	}
	if op.Op == "processWrite" || op.Op == "processCloseStdin" {
		s.writeInput(f, op.Op)
		return
	}
	requestCtx, cancel := context.WithCancel(ctx)
	r := &request{cancel: cancel, kill: make(chan struct{})}
	if op.Op == "process" || op.Op == "launch" {
		r.input = make(chan []byte, 256)
	}
	if op.Op == "gateway" {
		r.gateway = &gatewayStream{pending: map[uint32]gatewayRequest{}}
	}
	s.mu.Lock()
	s.requests[f.ID] = r
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.requests, f.ID)
			s.mu.Unlock()
			cancel()
		}()
		result, payload, err := s.handle(requestCtx, r, f, op.Op)
		if err != nil {
			s.send(frameError, f.ID, toError(err), nil)
			return
		}
		s.send(frameResult, f.ID, result, payload)
	}()
}

func (s *server) writeInput(f frame, op string) {
	target, err := decode[struct {
		Target uint32 `json:"target"`
	}](f.JSON)
	if err != nil {
		s.send(frameError, f.ID, toError(err), nil)
		return
	}
	s.mu.Lock()
	r := s.requests[target.Target]
	ok := r != nil && r.input != nil && !r.closed
	if ok && op == "processCloseStdin" {
		r.closed = true
	}
	s.mu.Unlock()
	if !ok {
		s.send(frameError, f.ID, codeError("EBADF", "process stdin is closed", ""), nil)
		return
	}
	if op == "processCloseStdin" {
		r.input <- nil
	} else {
		r.input <- append([]byte(nil), f.Payload...)
	}
	s.send(frameResult, f.ID, struct{}{}, nil)
}

func (s *server) handle(ctx context.Context, r *request, f frame, op string) (any, []byte, error) {
	empty := struct{}{}
	switch op {
	case "hello":
		return hello(), nil, nil
	case "gateway":
		return empty, nil, s.serveGateway(ctx, f.ID, r.gateway)
	case "gatewayReply":
		return empty, nil, s.replyGateway(f)
	case "lstat", "realpath", "write", "truncate", "rename", "mkdir", "rm", "mkdtemp", "open", "opendir":
		request, err := decode[pathRequest](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		if request.Path == "" {
			return nil, nil, codeError("EINVAL", "path is required", "")
		}
		switch op {
		case "lstat":
			info, err := lstat(request.Path)
			return map[string]FileInfo{"info": info}, nil, err
		case "realpath":
			path, err := filepath.EvalSymlinks(request.Path)
			if err == nil {
				path, err = filepath.Abs(path)
			}
			return map[string]string{"path": path}, nil, err
		case "write":
			result, err := writeFile(&s.handles, request, f.Payload)
			return result, nil, err
		case "truncate":
			return empty, nil, os.Truncate(request.Path, request.Size)
		case "rename":
			return empty, nil, os.Rename(request.Path, request.To)
		case "mkdir":
			return empty, nil, mkdir(request)
		case "rm":
			return empty, nil, remove(request)
		case "mkdtemp":
			path, err := os.MkdirTemp(filepath.Dir(request.Path), filepath.Base(request.Path)+"*")
			return map[string]string{"path": path}, nil, err
		case "open":
			result, err := openFile(&s.handles, request)
			return result, nil, err
		default:
			result, err := openDir(&s.handles, request.Path)
			return result, nil, err
		}
	case "pread", "writeChunk", "fsync", "fstat", "readdir", "close":
		request, err := decode[handleRequest](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		switch op {
		case "pread":
			data, err := pread(&s.handles, request)
			return map[string]int{"bytes": len(data)}, data, err
		case "writeChunk":
			return empty, nil, writeChunk(&s.handles, request, f.Payload)
		case "fsync":
			file, err := s.handles.get(request.Handle)
			if err == nil {
				err = file.Sync()
			}
			return empty, nil, err
		case "fstat":
			file, err := s.handles.get(request.Handle)
			if err != nil {
				return nil, nil, err
			}
			info, err := file.Stat()
			if err != nil {
				return nil, nil, err
			}
			return map[string]FileInfo{"info": describe(file.Name(), info)}, nil, nil
		case "readdir":
			result, err := readDir(&s.handles, request)
			return result, nil, err
		default:
			return empty, nil, s.handles.close(request.Handle)
		}
	case "listFiles":
		request, err := decode[listRequest](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		result, payload, err := listFiles(ctx, request)
		return result, payload, err
	case "exec":
		request, err := decode[execRequest](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		code, err := s.exec(ctx, r, f.ID, request)
		return map[string]int{"exitCode": code}, nil, err
	case "resolve":
		request, err := decode[resolveRequest](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		path, err := ResolveProgram(ctx, request.Command, AccountEnvironment(os.Environ(), request.RuntimeHome, request.Env))
		return map[string]string{"path": path}, nil, err
	case "launch":
		request, err := decode[launchRequest](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		code, err := s.launch(ctx, r, f.ID, request)
		return map[string]int{"exitCode": code}, nil, err
	case "mcp":
		request, err := decode[MCPInput](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		result, err := s.mcp(ctx, f.ID, request)
		return result, nil, err
	case "computer":
		request, err := decode[ComputerInput](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		result, err := s.computer(ctx, request)
		return result, nil, err
	case "process":
		request, err := decode[execRequest](f.JSON)
		if err != nil {
			return nil, nil, err
		}
		request.Command = ""
		if len(request.Argv) == 0 {
			return nil, nil, codeError("EINVAL", "argv is required", "")
		}
		code, err := s.process(ctx, r, f.ID, request)
		return map[string]int{"exitCode": code}, nil, err
	}
	return nil, nil, codeError("ENOSYS", "unknown operation "+op, "")
}

func (s *server) exec(ctx context.Context, r *request, id uint32, request execRequest) (int, error) {
	cmd, err := command(request)
	if err != nil {
		return 0, err
	}
	program, err := start(cmd, nil)
	if err != nil {
		return 0, err
	}
	decoders := map[string]*decoder{"stdout": {}, "stderr": {}}
	emit := func(stream string, text []byte) {
		if len(text) > 0 {
			s.send(frameEvent, id, outputEvent{Kind: "output", Stream: stream}, text)
		}
	}
	code, err := program.run(ctx, r.kill, time.Duration(request.TimeoutMs)*time.Millisecond, func(c chunk) {
		emit(c.stream, decoders[c.stream].decode(c.data))
	})
	for _, stream := range []string{"stdout", "stderr"} {
		emit(stream, decoders[stream].flush())
	}
	return code, err
}

// process runs argv with plain pipes. When the program exits, whatever it
// started in its process group is killed.
func (s *server) process(ctx context.Context, r *request, id uint32, request execRequest) (int, error) {
	cmd, err := command(request)
	if err != nil {
		return 0, err
	}
	return s.pipe(ctx, r, id, cmd)
}

func (s *server) pipe(ctx context.Context, r *request, id uint32, cmd *exec.Cmd) (int, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	program, err := start(cmd, nil)
	if err != nil {
		return 0, err
	}
	s.send(frameEvent, id, outputEvent{Kind: "started", PID: cmd.Process.Pid}, nil)
	go func() {
		for data := range r.input {
			if data == nil {
				_ = stdin.Close()
				return
			}
			if _, err := stdin.Write(data); err != nil {
				return
			}
		}
	}()
	defer func() {
		s.mu.Lock()
		r.closed = true
		s.mu.Unlock()
		_ = stdin.Close()
	}()
	code, err := program.run(ctx, r.kill, 0, func(c chunk) {
		s.send(frameEvent, id, outputEvent{Kind: "output", Stream: c.stream}, c.data)
	})
	program.group.kill()
	return code, err
}

func hello() Hello {
	h := Hello{
		Protocol: Protocol, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Tmpdir: os.TempDir(), PID: os.Getpid(), Features: Features,
	}
	h.Home, _ = os.UserHomeDir()
	h.Cwd, _ = os.Getwd()
	if current, err := user.Current(); err == nil {
		h.User = current.Username
	}
	return h
}
