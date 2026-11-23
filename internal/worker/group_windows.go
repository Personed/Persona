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

// processAlive reports whether pid still runs: it has no exit code yet.
func processAlive(pid int) bool {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	var code uint32
	const stillActive = 259
	return syscall.GetExitCodeProcess(handle, &code) == nil && code == stillActive
}

func killProcess(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}
