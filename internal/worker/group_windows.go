package worker

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// isolate starts the program in a new process group. Windows has no group
// kill; kill ends the program's own process tree through taskkill.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

type group struct{ process *os.Process }

func attach(cmd *exec.Cmd) *group { return &group{process: cmd.Process} }

func (g *group) kill() {
	tree := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(g.process.Pid))
	if tree.Run() != nil {
		_ = g.process.Kill()
	}
}

func statusCode(state *os.ProcessState) int { return state.ExitCode() }
