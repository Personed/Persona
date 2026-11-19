//go:build windows

package pi

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	createJobObject          = kernel32.NewProc("CreateJobObjectW")
	assignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	terminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

// tree is a job object holding Pi and every process it starts, since Windows
// keeps no process groups that outlive their parent.
type tree struct{ job syscall.Handle }

func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func track(process *os.Process) tree {
	job, _, _ := createJobObject.Call(0, 0)
	if job == 0 {
		return tree{}
	}
	const access = syscall.PROCESS_TERMINATE | 0x0100 // PROCESS_SET_QUOTA
	handle, err := syscall.OpenProcess(access, false, uint32(process.Pid))
	if err == nil {
		ok, _, _ := assignProcessToJobObject.Call(job, uintptr(handle))
		_ = syscall.CloseHandle(handle)
		if ok != 0 {
			return tree{job: syscall.Handle(job)}
		}
	}
	_ = syscall.CloseHandle(syscall.Handle(job))
	return tree{}
}

// Console programs without a console get no termination signal; closing stdin
// is the orderly request, so terminate and kill both end the tree.
func (p *Process) terminate() error { return p.kill() }

func (p *Process) kill() error {
	if p.tree.job != 0 {
		if ok, _, err := terminateJobObject.Call(uintptr(p.tree.job), 1); ok == 0 {
			return err
		}
		return nil
	}
	return p.cmd.Process.Kill()
}

// reap ends processes that outlived Pi and releases the job.
func (p *Process) reap(time.Duration) {
	if p.tree.job == 0 {
		return
	}
	_, _, _ = terminateJobObject.Call(uintptr(p.tree.job), 1)
	_ = syscall.CloseHandle(p.tree.job)
}
