//go:build !windows

package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

// processAlive reports whether pid still runs, as signal 0 delivery shows.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// processIdentity reports pid's executable and whether the worker's user
// owns it. found is false once pid has exited.
func processIdentity(ctx context.Context, pid int) (executable string, owned, found bool, err error) {
	if runtime.GOOS == "linux" {
		proc := "/proc/" + strconv.Itoa(pid)
		info, err := os.Stat(proc)
		if errors.Is(err, os.ErrNotExist) {
			return "", false, false, nil
		}
		if err != nil {
			return "", false, false, err
		}
		executable, err := os.Readlink(proc + "/exe")
		if errors.Is(err, os.ErrNotExist) {
			return "", false, false, nil
		}
		if err != nil {
			return "", false, false, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		return executable, ok && int(stat.Uid) == os.Getuid(), true, nil
	}
	if runtime.GOOS != "darwin" {
		return "", false, false, errors.New("process identity is not available on " + runtime.GOOS)
	}
	output, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "uid=").Output()
	uid := strings.TrimSpace(string(output))
	if uid == "" {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", false, false, nil
		}
		if err == nil {
			err = errors.New("ps reported no process owner")
		}
		return "", false, false, err
	}
	// ps reports how the process was invoked; lsof reports the executable
	// it runs, as the first text mapping.
	output, err = exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-Fn").Output()
	for _, line := range strings.Split(string(output), "\n") {
		if name, ok := strings.CutPrefix(line, "n"); ok && filepath.IsAbs(name) {
			return name, uid == strconv.Itoa(os.Getuid()), true, nil
		}
	}
	if err == nil {
		err = errors.New("lsof reported no process executable")
	}
	return "", false, false, err
}

// killTree kills pid's process group, which the daemon leads.
func killTree(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		err = syscall.Kill(pid, syscall.SIGKILL)
	}
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
