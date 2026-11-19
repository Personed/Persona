package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Driver is the work-container lifecycle a Target controls. *Docker implements it.
type Driver interface {
	Config() DockerConfig
	Inspect(ctx context.Context) (State, error)
	CheckNetwork(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Rebuild(ctx context.Context) error
	Daemon() ([]string, error)
	Process(botID int) ([]string, error)
}

// Action is a lifecycle change.
type Action string

// Lifecycle changes.
const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRebuild Action = "rebuild"
)

// Status is the reply of the status and lifecycle endpoints.
type Status struct {
	State   State   `json:"state"`
	Network Network `json:"network"`
}

// Errors returned before a channel opens.
var (
	ErrStopped = errors.New("controller: work container is not running")
	ErrChanged = errors.New("controller: work container changed while connecting")
	ErrClosed  = errors.New("controller: closing")
)

// Target serializes lifecycle changes of one work container and owns the channel
// processes running in it. A change ends every channel first, and a channel opened
// across a change is refused, so no process outlives the container it checked.
type Target struct {
	driver Driver
	// lifecycle is held for each change, status read and channel check, in order.
	lifecycle sync.Mutex

	mu         sync.Mutex
	processes  map[*exec.Cmd]struct{}
	generation uint64
	used       bool
	closed     bool
}

// NewTarget returns a Target for driver.
func NewTarget(driver Driver) *Target {
	return &Target{driver: driver, processes: map[*exec.Cmd]struct{}{}}
}

// Status waits for pending changes, then reports the state. A running container
// whose network check fails is reported as an error, never as running.
func (t *Target) Status(ctx context.Context) (Status, error) {
	t.lifecycle.Lock()
	defer t.lifecycle.Unlock()
	return t.inspect(ctx)
}

func (t *Target) inspect(ctx context.Context) (Status, error) {
	state, err := t.driver.Inspect(ctx)
	if err != nil {
		return Status{}, err
	}
	if state == StateRunning {
		if err := t.driver.CheckNetwork(ctx); err != nil {
			return Status{}, err
		}
	}
	return Status{State: state, Network: t.driver.Config().Network}, nil
}

// Change ends every channel, applies action and reports the resulting state.
func (t *Target) Change(ctx context.Context, action Action) (Status, error) {
	run := map[Action]func(context.Context) error{
		ActionStart: t.driver.Start, ActionStop: t.driver.Stop, ActionRebuild: t.driver.Rebuild,
	}[action]
	if run == nil {
		return Status{}, fmt.Errorf("controller: unknown action %q", action)
	}
	t.mu.Lock()
	if t.closed && action != ActionStop {
		t.mu.Unlock()
		return Status{}, ErrClosed
	}
	t.generation++
	t.used = true
	t.mu.Unlock()

	t.lifecycle.Lock()
	defer t.lifecycle.Unlock()
	t.killAll()
	if err := run(ctx); err != nil {
		return Status{}, err
	}
	return t.inspect(ctx)
}

// Channel is a checked command ready to run in the container.
type Channel struct {
	target     *Target
	args       []string
	generation uint64
}

// Prepare checks that the container is running with its network enforced and
// returns the daemon channel (botID 0, which needs token) or a Bot's process channel.
func (t *Target) Prepare(ctx context.Context, token string, botID int) (*Channel, error) {
	t.lifecycle.Lock()
	defer t.lifecycle.Unlock()
	t.mu.Lock()
	generation, closed := t.generation, t.closed
	t.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	state, err := t.driver.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	if state != StateRunning {
		return nil, ErrStopped
	}
	if err := t.driver.CheckNetwork(ctx); err != nil {
		return nil, err
	}
	var args []string
	if botID > 0 {
		args, err = t.driver.Process(botID)
	} else if token == "" {
		err = errors.New("controller: daemon token required")
	} else {
		args, err = t.driver.Daemon()
		args = append(args, "serve", "--token", token)
	}
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, errors.New("controller: channel command required")
	}
	return &Channel{target: t, args: args, generation: generation}, nil
}

// Process is a running channel.
type Process struct {
	// Stdin feeds the process; closing it ends its input.
	Stdin io.WriteCloser
	cmd   *exec.Cmd
	done  chan struct{}
}

// Kill ends the process.
func (p *Process) Kill() { _ = p.cmd.Process.Kill() }

// Done is closed when the process has exited and its output is flushed.
func (p *Process) Done() <-chan struct{} { return p.done }

// Start runs the channel with its stdout connected to the caller. It fails with
// ErrChanged when a lifecycle change began after Prepare.
func (c *Channel) Start(stdout io.Writer) (*Process, error) {
	t := c.target
	cmd := exec.Command(program(c.args[0]), c.args[1:]...)
	cmd.Stdout = stdout
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, ErrCommand
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, ErrClosed
	}
	if c.generation != t.generation {
		return nil, ErrChanged
	}
	if err := cmd.Start(); err != nil {
		return nil, ErrCommand
	}
	t.used = true
	t.processes[cmd] = struct{}{}
	process := &Process{Stdin: stdin, cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		t.mu.Lock()
		delete(t.processes, cmd)
		t.mu.Unlock()
		close(process.done)
	}()
	return process, nil
}

// Processes reports how many channel processes are running.
func (t *Target) Processes() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.processes)
}

func (t *Target) killAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for cmd := range t.processes {
		_ = cmd.Process.Kill()
	}
}

// Close ends every channel, waits for pending changes and stops the container if
// this controller used it.
func (t *Target) Close(ctx context.Context) error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.killAll()
	t.lifecycle.Lock()
	defer t.lifecycle.Unlock()
	t.mu.Lock()
	used := t.used
	t.mu.Unlock()
	if used {
		return t.driver.Stop(ctx)
	}
	return nil
}
