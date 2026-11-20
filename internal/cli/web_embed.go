//go:build embedweb

package cli

import (
	"io/fs"

	"github.com/Personed/Persona/apps/web"
)

// webFS serves the embedded build; the --static directory is ignored.
func webFS(string) (fs.FS, error) { return fs.Sub(web.Dist, "dist") }
