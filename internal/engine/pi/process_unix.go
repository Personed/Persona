//go:build unix

package pi

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// On Unix, Pi's process group is the tree; nothing extra to hold.
type tree struct{}

func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func track(*os.Process) tree { return tree{} }

// The group id equals Pi's pid. It cannot be reused while any member is alive,
// so signalling it never reaches unrelated processes.
func (p *Process) signal(sig syscall.Signal) error {
	err := syscall.Kill(-p.cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func (p *Process) terminate() error { return p.signal(syscall.SIGTERM) }

func (p *Process) kill() error { return p.signal(syscall.SIGKILL) }

// reap ends group members that outlived Pi: SIGTERM, then SIGKILL after grace.
func (p *Process) reap(grace time.Duration) {
	if p.signal(syscall.SIGTERM) != nil {
		return
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if p.signal(0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = p.signal(syscall.SIGKILL)
}
