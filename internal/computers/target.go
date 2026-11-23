package computers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"sync"

	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/worker"
)

// Status is ComputerStatus of index.ts: the wire shape of a target's state.
type Status struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Deployment string  `json:"deployment"`
	State      string  `json:"state"` // running | stopped | missing | unavailable
	OS         string  `json:"os"`
	Arch       string  `json:"arch"`
	Directory  string  `json:"directory"`
	Boundary   string  `json:"boundary"`
	Network    string  `json:"network"`
	Error      *string `json:"error"`
}

// driver manages a non-process target's lifecycle, ComputerDriver of driver.ts.
type driver interface {
	inspect(ctx context.Context) (string, error)
	checkNetwork(ctx context.Context) error
	start(ctx context.Context) error
	stop(ctx context.Context) error
	rebuild(ctx context.Context) error
	binding() json.RawMessage
	directory() string
	network() string
}

// Target is the Computer class of index.ts: one catalog item's live target.
type Target struct {
	cfg    ServiceConfig
	record Record
	drv    driver

	mu       sync.Mutex
	changing bool
	remote   *remoteInfo
}

type remoteInfo struct {
	Platform string
	Arch     string
}

func newTarget(cfg ServiceConfig, record Record) *Target {
	t := &Target{cfg: cfg, record: record}
	switch record.Config.Kind {
	case "docker":
		t.drv = newDocker(record.Config)
	case "controller":
		d, err := newController(record.Config)
		if err == nil {
			t.drv = d
		}
	}
	return t
}

// bind writes computer.json pinning the execution target, Computer.bind.
func (t *Target) bind() error {
	filename := filepath.Join(t.cfg.DataDir, "computer.json")
	var binding json.RawMessage
	if t.record.Config.Kind == "process" {
		binding, _ = json.Marshal(map[string]string{
			"kind": "process", "deployment": t.cfg.Deployment,
		})
	} else {
		binding = t.drv.binding()
	}
	previous, err := os.ReadFile(filename)
	switch {
	case err == nil:
		var a, b any
		if json.Unmarshal(previous, &a) != nil || json.Unmarshal(binding, &b) != nil || !reflect.DeepEqual(a, b) {
			return errors.New("执行目标已改变。请停止 Persona，备份工作文件并确认迁移，再移除 computer.json 后启动。")
		}
	case errors.Is(err, os.ErrNotExist):
		file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, werr := file.Write(binding)
		if err := file.Close(); err != nil {
			return err
		}
		if werr != nil {
			return werr
		}
	default:
		return err
	}
	return nil
}

// status reports the target's state, Computer.status.
func (t *Target) status(ctx context.Context) (Status, error) {
	if t.drv == nil {
		osName, arch := runtime.GOOS, runtime.GOARCH
		boundary := "host"
		if t.cfg.Deployment == "container" {
			boundary = "application"
		}
		return Status{
			Deployment: t.cfg.Deployment,
			ID:         "process",
			Kind:       "process",
			State:      "running",
			OS:         fmt.Sprintf("%s %s", osName, arch),
			Arch:       arch,
			Directory:  filepath.Join(t.cfg.DataDir, "agent.sqlite.files"),
			Boundary:   boundary,
			Network:    "host",
			Error:      nil,
		}, nil
	}
	state, err := t.drv.inspect(ctx)
	var message *string
	if err == nil && state == "running" {
		err = t.drv.checkNetwork(ctx)
	}
	if err != nil {
		state = "unavailable"
		text := err.Error()
		message = &text
	}
	osName, arch := "", ""
	if t.remote != nil {
		osName, arch = t.remote.Platform, t.remote.Arch
	}
	return Status{
		Deployment: t.cfg.Deployment,
		ID:         t.record.Config.ID,
		Kind:       t.record.Config.Kind,
		State:      state,
		OS:         empty(osName, nil),
		Arch:       empty(arch, nil),
		Directory:  t.drv.directory(),
		Boundary:   "sandbox",
		Network:    t.drv.network(),
		Error:      message,
	}, nil
}

func empty(value string, _ any) string { return value }

// manage runs a lifecycle action on the target, Computer.manage. The pending
// channel serializes actions; requireIdle runs while the target is held.
func (t *Target) manage(ctx context.Context, action string, requireIdle func(context.Context) error) error {
	if t.drv == nil {
		return errors.New("当前环境由 Persona 进程管理。")
	}
	t.mu.Lock()
	for t.changing {
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		t.mu.Lock()
	}
	t.changing = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.changing = false
		t.mu.Unlock()
	}()
	if err := requireIdle(ctx); err != nil {
		return err
	}
	if err := t.close(ctx); err != nil {
		return err
	}
	switch action {
	case "start":
		return t.drv.start(ctx)
	case "stop":
		return t.drv.stop(ctx)
	case "rebuild":
		return t.drv.rebuild(ctx)
	}
	return rpc.NewError("BAD_REQUEST", "操作无效。")
}

// close releases the target's environments and services.
func (t *Target) close(context.Context) error {
	// Environments and service workers land with the views/tools slices.
	return nil
}

// workDir is the Bot workspace on the computer (SandboxWorkspace in index.ts).
func (t *Target) workDir(botID int64) string {
	if t.drv == nil {
		return filepath.Join(t.cfg.DataDir, "agent.sqlite.files", strconv.FormatInt(botID, 10), "workspace")
	}
	return t.drv.directory() + "/bots/" + strconv.FormatInt(botID, 10)
}

// transport returns the worker.Transport reaching this computer's worker and
// the workspace the first call should use. A process target launches
// `persona worker serve` locally; docker execs into its container; controller
// opens the CONNECT bridge (not yet wired).
func (t *Target) transport(botID int64) (worker.Transport, string, error) {
	dir := t.workDir(botID)
	if t.drv == nil {
		binary, err := os.Executable()
		if err != nil {
			return nil, "", err
		}
		return worker.Local(binary), dir, nil
	}
	switch drv := t.drv.(type) {
	case *dockerDriver:
		args, err := drv.daemon(botID)
		if err != nil {
			return nil, "", err
		}
		return func(token string) []string {
			return append(args, "--token", token)
		}, dir, nil
	}
	return nil, "", errors.New("工作电脑控制端通道尚未接入。")
}
