package pi

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// StopGrace is how long Stop waits after each shutdown step before escalating.
const StopGrace = 5 * time.Second

const stderrTailBytes = 8 << 10

// Command describes how to start Pi, normally `pi --mode rpc` plus session flags.
type Command struct {
	Path string
	Args []string
	Dir  string
	// Env replaces the environment when non-nil.
	Env []string
}

// Process is one running Pi. Its stdout carries only protocol records; stderr
// diagnostics are kept as a bounded tail.
type Process struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr tail
	tree   tree

	done chan struct{}
	err  error

	stopOnce sync.Once
	stopErr  error
}

// Start runs Pi in its own process group, so Stop reaches the tools it spawned.
func Start(command Command) (*Process, error) {
	cmd := exec.Command(command.Path, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	isolate(cmd)
	// Plain pipes keep Wait independent of stdout readers and of grandchildren
	// that inherit stderr.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW)
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		return nil, err
	}
	closeAll(inR, outW, errW)
	p := &Process{cmd: cmd, stdin: inW, stdout: outR, done: make(chan struct{})}
	p.tree = track(cmd.Process)
	go func() { _, _ = io.Copy(&p.stderr, errR); _ = errR.Close() }()
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// PID returns the operating system process id.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Stdin is where commands are written.
func (p *Process) Stdin() io.Writer { return p.stdin }

// Stdout is where Pi writes protocol records. It ends when Pi and every process
// holding it exit.
func (p *Process) Stdout() io.Reader { return p.stdout }

// Stderr returns the last diagnostics Pi wrote.
func (p *Process) Stderr() string { return p.stderr.String() }

// Done closes when Pi exits.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err reports how Pi exited, once Done is closed.
func (p *Process) Err() error {
	<-p.done
	return p.err
}

// Close stops Pi with StopGrace between steps.
func (p *Process) Close() error { return p.Stop(StopGrace) }

// Stop ends Pi and its process group. Closing stdin asks Pi for an orderly
// shutdown; after grace a termination signal follows, and after another grace
// the group is killed. Remaining group members are then terminated the same way.
func (p *Process) Stop(grace time.Duration) error {
	p.stopOnce.Do(func() { p.stopErr = p.stop(grace) })
	return p.stopErr
}

func (p *Process) stop(grace time.Duration) error {
	_ = p.stdin.Close()
	if !p.wait(grace) {
		_ = p.terminate()
		if !p.wait(grace) {
			if err := p.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
			<-p.done
		}
	}
	p.reap(grace)
	_ = p.stdout.Close()
	return nil
}

func (p *Process) wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

type tail struct {
	mu   sync.Mutex
	data []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, b...)
	if over := len(t.data) - stderrTailBytes; over > 0 {
		t.data = append(t.data[:0], t.data[over:]...)
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}
