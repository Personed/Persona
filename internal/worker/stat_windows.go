package worker

import "io/fs"

func fileIdentity(fs.FileInfo) (dev, ino uint64) { return 0, 0 }
