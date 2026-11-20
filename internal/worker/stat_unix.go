//go:build !windows

package worker

import (
	"io/fs"
	"syscall"
)

func fileIdentity(info fs.FileInfo) (dev, ino uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), stat.Ino //nolint:unconvert // Dev is int32 on darwin.
	}
	return 0, 0
}
