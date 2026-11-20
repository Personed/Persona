package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

type execRequest struct {
	Command    string            `json:"command,omitempty"`
	Argv       []string          `json:"argv,omitempty"`
	Cwd        string            `json:"cwd"`
	Env        map[string]string `json:"env,omitempty"`
	InheritEnv *bool             `json:"inheritEnv,omitempty"`
	ShellPath  string            `json:"shellPath,omitempty"`
	TimeoutMs  int               `json:"timeoutMs,omitempty"`
}

type outputEvent struct {
	Kind   string `json:"kind"`
	Stream string `json:"stream,omitempty"`
	PID    int    `json:"pid,omitempty"`
}

// outputSettle is how long output is still collected after the program exits
// while a process it started keeps the pipes open.
const outputSettle = 100 * time.Millisecond

func environment(request execRequest) []string {
	values := map[string]string{}
	if request.InheritEnv == nil || *request.InheritEnv {
		for _, entry := range os.Environ() {
			for i := 1; i < len(entry); i++ {
				if entry[i] == '=' {
					values[entry[:i]] = entry[i+1:]
					break
				}
			}
		}
	}
	for key, value := range request.Env {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

// command builds the program for an exec or process request. Shell strings run
// through shellPath, else bash, else sh.
func command(request execRequest) (*exec.Cmd, error) {
	argv := request.Argv
	if len(argv) == 0 {
		if request.Command == "" {
			return nil, codeError("EINVAL", "command or argv is required", "")
		}
		shell := request.ShellPath
		if shell != "" {
			if _, err := os.Stat(shell); err != nil {
				return nil, codeError("shell_unavailable", "shell not found: "+shell, shell)
			}
		} else if shell = findShell(); shell == "" {
			return nil, codeError("shell_unavailable", "no bash or sh found", "")
		}
		argv = []string{shell, "-c", request.Command}
	}
	if request.Cwd == "" {
		return nil, codeError("EINVAL", "cwd is required", "")
	}
	if info, err := os.Stat(request.Cwd); err != nil || !info.IsDir() {
		return nil, codeError("spawn_error", "working directory does not exist: "+request.Cwd, request.Cwd)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = request.Cwd
	cmd.Env = environment(request)
	if filepath.Base(argv[0]) == argv[0] {
		if path, err := lookPath(argv[0], cmd.Env); err == nil {
			cmd.Path = path
		} else {
			return nil, codeError("spawn_error", err.Error(), argv[0])
		}
	}
	isolate(cmd)
	return cmd, nil
}

func findShell() string {
	candidates := []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"}
	if runtime.GOOS == "windows" {
		// Prefer Git Bash; System32\bash.exe starts WSL instead.
		candidates = []string{filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe")}
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	for _, name := range []string{"bash", "sh"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// lookPath resolves name against the PATH of the program's environment.
func lookPath(name string, env []string) (string, error) {
	for i := len(env) - 1; i >= 0; i-- {
		if len(env[i]) > 5 && (env[i][:5] == "PATH=" || env[i][:5] == "Path=") {
			for _, dir := range filepath.SplitList(env[i][5:]) {
				if path, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
					return path, nil
				}
			}
			return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
		}
	}
	return exec.LookPath(name)
}

// running is a started program whose output is read from its pipes.
type running struct {
	cmd    *exec.Cmd
	group  *group
	chunks chan chunk
	wg     sync.WaitGroup
	reads  []*os.File
}

type chunk struct {
	stream string
	data   []byte
}

func start(cmd *exec.Cmd, stdin io.Reader) (*running, error) {
	r := &running{cmd: cmd, chunks: make(chan chunk, 64)}
	var writes []*os.File
	for _, stream := range []string{"stdout", "stderr"} {
		read, write, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		r.reads, writes = append(r.reads, read), append(writes, write)
		r.wg.Add(1)
		go r.read(stream, read)
	}
	cmd.Stdout, cmd.Stderr = writes[0], writes[1]
	if stdin != nil {
		cmd.Stdin = stdin
	}
	err := cmd.Start()
	for _, write := range writes {
		_ = write.Close()
	}
	if err != nil {
		r.closeReads()
		return nil, codeError("spawn_error", err.Error(), cmd.Path)
	}
	r.group = attach(cmd)
	go func() { r.wg.Wait(); close(r.chunks) }()
	return r, nil
}

func (r *running) read(stream string, file *os.File) {
	defer r.wg.Done()
	buffer := make([]byte, 64<<10)
	for {
		n, err := file.Read(buffer)
		if n > 0 {
			r.chunks <- chunk{stream: stream, data: append([]byte(nil), buffer[:n]...)}
		}
		if err != nil {
			return
		}
	}
}

func (r *running) closeReads() {
	for _, read := range r.reads {
		_ = read.Close()
	}
}

// run waits for the program, forwarding output, and returns its exit code. On
// cancellation it kills the process group and returns aborted; after exit it
// collects output until both pipes close or outputSettle passes without output.
// A signal on kill kills the program without aborting, so run returns the killed
// program's status.
func (r *running) run(ctx context.Context, kill <-chan struct{}, timeout time.Duration, forward func(chunk)) (int, error) {
	exited := make(chan error, 1)
	go func() { exited <- r.cmd.Wait() }()
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	var failure error
	var waitErr error
	chunks := r.chunks
wait:
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			forward(c)
		case waitErr = <-exited:
			break wait
		case <-kill:
			kill = nil
			r.group.kill()
		case <-ctx.Done():
			failure = codeError("aborted", "command aborted", "")
			r.group.kill()
			waitErr = <-exited
			break wait
		case <-timer:
			failure = codeError("timeout", "command timed out", "")
			r.group.kill()
			waitErr = <-exited
			break wait
		}
	}
	settle := time.NewTimer(outputSettle)
	defer settle.Stop()
	for chunks != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			forward(c)
			settle.Reset(outputSettle)
		case <-settle.C:
			r.closeReads()
			for c := range chunks {
				forward(c)
			}
			chunks = nil
		}
	}
	if failure != nil {
		return 0, failure
	}
	return exitCode(r.cmd, waitErr)
}

func exitCode(cmd *exec.Cmd, err error) (int, error) {
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return 0, err
	}
	return statusCode(cmd.ProcessState), nil
}

// decoder turns output bytes into UTF-8 text, holding back an incomplete
// trailing rune until the next chunk.
type decoder struct{ pending []byte }

func (d *decoder) decode(data []byte) []byte {
	data = append(d.pending, data...)
	cut := len(data)
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	d.pending = append([]byte(nil), data[cut:]...)
	return []byte(toValid(data[:cut]))
}

func (d *decoder) flush() []byte {
	rest := d.pending
	d.pending = nil
	return []byte(toValid(rest))
}

func toValid(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	return string([]rune(string(data)))
}
