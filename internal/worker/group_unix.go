//go:build !windows

package worker

import (
	"os"
	"os/exec"
	"syscall"
)

// isolate starts the program as the leader of its own process group, so
// cancellation reaches everything it started that stayed in the group.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

type group struct{ pid int }

func attach(cmd *exec.Cmd) *group { return &group{pid: cmd.Process.Pid} }

func (g *group) kill() { _ = syscall.Kill(-g.pid, syscall.SIGKILL) }

func statusCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}
