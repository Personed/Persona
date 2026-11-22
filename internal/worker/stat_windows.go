package worker

import (
	"io/fs"
	"syscall"
)

// fileIdentity reads the link count from the path itself; the reparse point
// flag keeps a symbolic link from being followed.
func fileIdentity(path string, _ fs.FileInfo) (dev, ino, links uint64) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0
	}
	handle, err := syscall.CreateFile(name, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, 0, 0
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck // Read-only handle.
	var data syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(handle, &data) != nil {
		return 0, 0, 0
	}
	return 0, 0, uint64(data.NumberOfLinks)
}
