package worker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// serviceKey matches service variables that never reach a Harness process.
var serviceKey = regexp.MustCompile(`(?i)^(PERSONA_|SERVICE_)|^DATABASE_URL$`)

var windowsProgram = regexp.MustCompile(`(?i)\.(exe|cmd|com|bat)$`)

// AccountEnvironment builds the environment of a native Harness process from the
// worker's own: service variables are removed, HOME is the OS user's, and PATH
// adds the user's npm and local bin directories and the Persona runtime's
// node_modules/.bin under runtimeHome. overlay is applied last.
func AccountEnvironment(source []string, runtimeHome string, overlay map[string]string) []string {
	env := map[string]string{}
	var order []string
	set := func(key, value string) {
		if _, ok := env[key]; !ok {
			order = append(order, key)
		}
		env[key] = value
	}
	for _, entry := range source {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" && !serviceKey.MatchString(key) {
			set(key, value)
		}
	}
	delete(env, "CLAUDECODE")
	delete(env, "CLAUDE_CODE_ENTRYPOINT")
	if env["HOME"] == "" {
		home := env["USERPROFILE"]
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		set("HOME", home)
	}
	if env["USERPROFILE"] == "" {
		set("USERPROFILE", env["HOME"])
	}
	pathKey := "PATH"
	for key := range env {
		if strings.EqualFold(key, "PATH") {
			pathKey = key
		}
	}
	paths := []string{env[pathKey], filepath.Join(env["HOME"], ".local", "bin"), filepath.Join(env["HOME"], ".npm-global", "bin")}
	if env["APPDATA"] != "" {
		paths = append(paths, filepath.Join(env["APPDATA"], "npm"))
	}
	if runtimeHome != "" {
		paths = append(paths, filepath.Join(runtimeHome, "runtime", "node_modules", ".bin"))
	}
	var kept []string
	for _, path := range paths {
		if path != "" {
			kept = append(kept, path)
		}
	}
	set(pathKey, strings.Join(kept, string(os.PathListSeparator)))
	for key, value := range overlay {
		set(key, value)
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		if value, ok := env[key]; ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func lookupEnv(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if key, value, ok := strings.Cut(env[i], "="); ok && strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

// ResolveProgram finds command the way the work computer's OS user would: a path
// (with ~/ expanded) is checked directly, a name is searched on PATH and then
// through the user's login shell (where.exe on Windows). It returns "" when the
// program is not installed.
func ResolveProgram(ctx context.Context, command string, env []string) (string, error) {
	windows := runtime.GOOS == "windows"
	file := func(path string) (string, error) {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || (!windows && info.Mode().Perm()&0o111 == 0) {
			return "", nil
		}
		return path, nil
	}
	if rest, ok := strings.CutPrefix(command, "~/"); ok {
		command = filepath.Join(lookupEnv(env, "HOME"), rest)
	}
	if filepath.IsAbs(command) || strings.ContainsAny(command, `/\`) {
		if windows && filepath.Ext(command) == "" {
			for _, extension := range []string{".exe", ".cmd", ".com", ".bat"} {
				if path, err := file(command + extension); path != "" || err != nil {
					return path, err
				}
			}
		}
		return file(command)
	}
	if !windows {
		for _, dir := range filepath.SplitList(lookupEnv(env, "PATH")) {
			if !filepath.IsAbs(dir) {
				continue
			}
			if path, err := file(filepath.Join(dir, command)); path != "" || err != nil {
				return path, err
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if windows {
		cmd = exec.CommandContext(ctx, "where.exe", command)
	} else {
		shell := lookupEnv(env, "SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-lc", `command -v "$1"`, "persona", command)
	}
	cmd.Env = env
	output, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if (windows && !windowsProgram.MatchString(line)) || !filepath.IsAbs(line) {
			continue
		}
		if path, err := file(line); path != "" || err != nil {
			return path, err
		}
	}
	return "", nil
}

func isNotDir(err error) bool {
	return err != nil && toError(err).Code == "ENOTDIR"
}
