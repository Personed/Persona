package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Runner runs one host command (docker, sudo, or ssh wrapping either) and returns
// its trimmed stdout.
type Runner func(ctx context.Context, args []string) (string, error)

// ErrCommand reports a failed control command. Output is not included because it
// can name host paths and keys.
var ErrCommand = errors.New("controller: computer control request did not complete; check Docker, SSH host keys and permissions")

const commandTimeout = 30 * time.Second

// ExecRunner runs commands on this machine with a 30 s timeout and 1 MiB of output.
func ExecRunner(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", ErrCommand
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, program(args[0]), args[1:]...)
	var stdout limitedBuffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil || stdout.overflow {
		return "", ErrCommand
	}
	return strings.TrimSpace(stdout.String()), nil
}

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		b.overflow = true
		return 0, errors.New("output limit")
	}
	return b.Buffer.Write(p)
}

// program finds Docker Desktop's CLI on Windows, where its directory is often not
// on PATH for services.
func program(name string) string {
	if runtime.GOOS != "windows" || name != "docker" {
		return name
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, dir := range []string{
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "DockerDesktop", "resources", "bin"),
		filepath.Join(os.Getenv("ProgramFiles"), "Docker", "Docker", "resources", "bin"),
		`C:\Program Files\Docker\Docker\resources\bin`,
	} {
		if candidate := filepath.Join(dir, "docker.exe"); fileExists(candidate) {
			return candidate
		}
	}
	return name
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// HostCommand wraps args in ssh when the Docker host is remote.
func (c DockerConfig) HostCommand(args ...string) ([]string, error) {
	if c.SSH == nil {
		return args, nil
	}
	ssh, err := sshArguments(*c.SSH)
	if err != nil {
		return nil, err
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = quoteArgument(arg)
	}
	return append(append([]string{"ssh"}, ssh...), strings.Join(quoted, " ")), nil
}

func quoteArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// sshArguments matches pi-env's sshArguments: no prompts, no forwarding, no shared
// connections, and strict host keys from the configured file under a fixed alias.
func sshArguments(target SSH) ([]string, error) {
	for name, value := range map[string]string{"host": target.Host, "host key alias": target.HostKeyAlias, "user": target.User} {
		if name == "user" && value == "" {
			continue
		}
		if value == "" || strings.HasPrefix(value, "-") || strings.ContainsFunc(value, invalidSSHRune) {
			return nil, fmt.Errorf("controller: invalid ssh %s", name)
		}
	}
	if target.Port < 0 || target.Port > 65535 {
		return nil, errors.New("controller: invalid ssh port")
	}
	knownHosts, err := configPath(target.KnownHostsFile)
	if err != nil {
		return nil, err
	}
	var args []string
	if target.ConfigFile != "" {
		args = append(args, "-F", target.ConfigFile)
	}
	args = append(args, "-T", "-a", "-x")
	for _, option := range []string{
		"BatchMode=yes", "ClearAllForwardings=yes", "ForwardAgent=no", "ForwardX11=no",
		"ControlMaster=no", "ControlPath=none", "RemoteCommand=none", "PermitLocalCommand=no",
		"SendEnv=-*", "ConnectTimeout=15", "ServerAliveInterval=15", "StrictHostKeyChecking=yes",
		"UserKnownHostsFile=" + knownHosts, "GlobalKnownHostsFile=none", "HashKnownHosts=no",
		"HostKeyAlias=" + target.HostKeyAlias,
	} {
		args = append(args, "-o", option)
	}
	if target.User != "" {
		args = append(args, "-l", target.User)
	}
	if target.Port != 0 {
		args = append(args, "-p", strconv.Itoa(target.Port))
	}
	if target.IdentityFile != "" {
		identity, err := configPath(target.IdentityFile)
		if err != nil {
			return nil, err
		}
		args = append(args, "-o", "IdentityFile="+identity, "-o", "IdentitiesOnly=yes")
	}
	return append(args, "--", target.Host), nil
}

func invalidSSHRune(r rune) bool { return r <= 0x20 || r == 0x7f }

// configPath quotes a path for an ssh option that expands % tokens.
func configPath(path string) (string, error) {
	if path == "" || strings.ContainsFunc(path, func(r rune) bool { return r == '"' || r < 0x20 || r == 0x7f }) {
		return "", errors.New("controller: invalid ssh path")
	}
	return `"` + strings.ReplaceAll(path, "%", "%%") + `"`, nil
}
