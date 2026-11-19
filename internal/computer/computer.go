// Package computer defines the Bot's bound work computer. Commands, files, desktop
// and browser actions all run on the Target, which may be a different machine from
// the client and the Persona service.
package computer

import (
	"context"
	"encoding/json"
	"io"
)

// Kind is how a Target is reached.
type Kind string

// Target kinds.
const (
	Local   Kind = "local"
	Remote  Kind = "remote"  // through the controller
	Sandbox Kind = "sandbox" // a managed remote sandbox
)

// Info describes a Target.
type Info struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     Kind   `json:"kind"`
	OS       string `json:"os"` // GOOS values: linux, darwin, windows
	Arch     string `json:"arch"`
	User     string `json:"user"`
	Home     string `json:"home"`
	Desktop  bool   `json:"desktop"`
	Browser  bool   `json:"browser"`
	Headless bool   `json:"headless"`
}

// Command runs one program on the Target.
type Command struct {
	Path   string
	Args   []string
	Dir    string
	Env    map[string]string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Process is a started command. Wait returns its exit code.
type Process interface {
	Wait() (exitCode int, err error)
	// Kill terminates the process tree.
	Kill() error
}

// FileInfo describes one path on the Target.
type FileInfo struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Dir     bool   `json:"dir"`
	ModTime int64  `json:"modTime"` // Unix ms
}

// Action is one native desktop or browser action. Name and Arguments are forwarded
// unchanged to Cua (desktop) or agent-browser (browser).
type Action struct {
	Service   string          `json:"service"` // "desktop" or "browser"
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Observation is the result of an action or explicit observation. Raw keeps the
// native result; Screenshot keeps the original image with its coordinate space.
type Observation struct {
	Raw        json.RawMessage `json:"raw"`
	Screenshot *Screenshot     `json:"screenshot,omitempty"`
}

// Screenshot is PNG/JPEG data with the scale between image pixels and logical points.
type Screenshot struct {
	MimeType string  `json:"mimeType"`
	Data     []byte  `json:"data"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Scale    float64 `json:"scale"`
}

// Target is one work computer. Execute does not imply the user task completed:
// callers verify with Observe, and never replay an action whose outcome is unknown.
type Target interface {
	Info(ctx context.Context) (Info, error)
	Start(ctx context.Context, command Command) (Process, error)
	Stat(ctx context.Context, path string) (FileInfo, error)
	ReadFile(ctx context.Context, path string) (io.ReadCloser, error)
	WriteFile(ctx context.Context, path string, data io.Reader) error
	Execute(ctx context.Context, action Action) (Observation, error)
	Observe(ctx context.Context, service string) (Observation, error)
	Close() error
}
