//go:build windows

package pi

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Windows has no termination signal for console programs without a console;
// closing stdin is the orderly request, so terminate and kill both end the tree.
func terminate(p *os.Process) error { return kill(p) }

func kill(p *os.Process) error {
	err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
	if err != nil {
		return p.Kill()
	}
	return nil
}

func reapGroup(*os.Process, time.Duration) {}
