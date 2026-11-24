package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// processIdentity reports pid's executable and whether the worker's user
// owns it. found is false once pid has exited.
func processIdentity(ctx context.Context, pid int) (executable string, owned, found bool, err error) {
	script := "$p = Get-CimInstance Win32_Process -Filter 'ProcessId=" + strconv.Itoa(pid) + "'; if ($p) { [pscustomobject]@{ path = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$p.ExecutablePath)); owned = ((Invoke-CimMethod -InputObject $p -MethodName GetOwnerSid).Sid -eq [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value) } | ConvertTo-Json -Compress }"
	query, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// The path travels as base64 UTF-8, which the console code page
	// cannot alter.
	output, err := exec.CommandContext(query, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "", false, false, err
	}
	if strings.TrimSpace(string(output)) == "" {
		return "", false, false, nil
	}
	var identity struct {
		Path  string `json:"path"`
		Owned bool   `json:"owned"`
	}
	if err := json.Unmarshal(output, &identity); err != nil {
		return "", false, false, err
	}
	path, err := base64.StdEncoding.DecodeString(identity.Path)
	if err != nil {
		return "", false, false, err
	}
	return string(path), identity.Owned, true, nil
}

// killTree ends pid's process tree.
func killTree(pid int) error {
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
