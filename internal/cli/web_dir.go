//go:build !embedweb

package cli

import (
	"io/fs"
	"os"
)

func webFS(dir string) (fs.FS, error) { return os.DirFS(dir), nil }
