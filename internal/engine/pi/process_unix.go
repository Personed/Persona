//go:build unix

package pi

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// The group id equals Pi's pid. It cannot be reused while any member is alive,
// so signalling it never reaches unrelated processes.
func signalGroup(p *os.Process, sig syscall.Signal) error {
	err := syscall.Kill(-p.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func terminate(p *os.Process) error { return signalGroup(p, syscall.SIGTERM) }

func kill(p *os.Process) error { return signalGroup(p, syscall.SIGKILL) }

func reapGroup(p *os.Process, grace time.Duration) {
	if signalGroup(p, syscall.SIGTERM) != nil {
		return
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if signalGroup(p, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = signalGroup(p, syscall.SIGKILL)
}
