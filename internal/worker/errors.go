package worker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
)

var errnoCodes = map[syscall.Errno]string{
	syscall.ENOENT:       "ENOENT",
	syscall.EEXIST:       "EEXIST",
	syscall.ENOTDIR:      "ENOTDIR",
	syscall.EISDIR:       "EISDIR",
	syscall.ENOTEMPTY:    "ENOTEMPTY",
	syscall.EACCES:       "EACCES",
	syscall.EPERM:        "EPERM",
	syscall.EBADF:        "EBADF",
	syscall.EMFILE:       "EMFILE",
	syscall.ELOOP:        "ELOOP",
	syscall.EINVAL:       "EINVAL",
	syscall.EBUSY:        "EBUSY",
	syscall.EXDEV:        "EXDEV",
	syscall.ENOSPC:       "ENOSPC",
	syscall.EROFS:        "EROFS",
	syscall.ENAMETOOLONG: "ENAMETOOLONG",
}

// toError maps a Go error to a wire error with a Node-style code.
func toError(err error) *Error {
	var wire *Error
	if errors.As(err, &wire) {
		return wire
	}
	out := &Error{Code: "unknown", Message: err.Error()}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		out.Syscall, out.Path = pathErr.Op, pathErr.Path
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		out.Syscall, out.Path = linkErr.Op, linkErr.Old
	}
	var errno syscall.Errno
	switch {
	case errors.As(err, &errno) && errnoCodes[errno] != "":
		out.Code = errnoCodes[errno]
	case errors.Is(err, fs.ErrNotExist):
		out.Code = "ENOENT"
	case errors.Is(err, fs.ErrExist):
		out.Code = "EEXIST"
	case errors.Is(err, fs.ErrPermission):
		out.Code = "EACCES"
	case errors.Is(err, context.Canceled):
		out.Code = "aborted"
	case errors.Is(err, context.DeadlineExceeded):
		out.Code = "timeout"
	}
	return out
}

func codeError(code, message, path string) *Error {
	return &Error{Code: code, Message: message, Path: path}
}
