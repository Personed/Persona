//go:build !windows

package worker

import (
	"io/fs"
	"syscall"
)

func fileIdentity(_ string, info fs.FileInfo) (dev, ino, links uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), stat.Ino, uint64(stat.Nlink) //nolint:unconvert // Dev is int32 and Nlink uint16 on darwin.
	}
	return 0, 0, 0
}

// ownedPrivate reports whether uid owns info and only that user can use it.
func ownedPrivate(info fs.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid && info.Mode().Perm() == 0o700
}
