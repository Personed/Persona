package pi

import "syscall"

// alive reports whether pid is a running process.
func alive(pid int) bool {
	const queryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(queryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if syscall.GetExitCodeProcess(handle, &code) != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
