package worker

import (
	"context"
	"os"
	"os/exec"
	"os/user"
)

type resolveRequest struct {
	Command     string            `json:"command"`
	RuntimeHome string            `json:"runtimeHome,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// launchRequest starts a native Harness process, by default `pi --mode rpc`.
type launchRequest struct {
	Program     string            `json:"program,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Cwd         string            `json:"cwd"`
	RuntimeHome string            `json:"runtimeHome,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// User is the OS user the program runs as: a name, or uid[:gid] on Unix.
	// Empty means the worker's own user, which is the work computer's account.
	User string `json:"user,omitempty"`
}

// launch runs a Harness program as the work computer's OS user with that user's
// account environment. The program is resolved like the user's own shell would,
// so inspect, login and execution share one program.
func (s *server) launch(ctx context.Context, r *request, id uint32, request launchRequest) (int, error) {
	program, args := request.Program, request.Args
	if program == "" {
		program, args = "pi", append([]string{"--mode", "rpc"}, args...)
	}
	if request.Cwd == "" {
		return 0, codeError("EINVAL", "cwd is required", "")
	}
	if info, err := os.Stat(request.Cwd); err != nil || !info.IsDir() {
		return 0, codeError("spawn_error", "working directory does not exist: "+request.Cwd, request.Cwd)
	}
	account, err := lookupAccount(request.User)
	if err != nil {
		return 0, err
	}
	source := os.Environ()
	if account != nil {
		source = append(source, "HOME="+account.HomeDir, "USER="+account.Username, "LOGNAME="+account.Username)
	}
	env := AccountEnvironment(source, request.RuntimeHome, request.Env)
	path, err := ResolveProgram(ctx, program, env)
	if err != nil {
		return 0, codeError("spawn_error", err.Error(), program)
	}
	if path == "" {
		return 0, codeError("ENOENT", program+" is not installed for this user", program)
	}
	cmd := exec.Command(path, args...)
	cmd.Dir, cmd.Env = request.Cwd, env
	isolate(cmd)
	if account != nil {
		if err := runAs(cmd, account); err != nil {
			return 0, err
		}
	}
	return s.pipe(ctx, r, id, cmd)
}

// lookupAccount returns the requested OS user, or nil when it is the worker's own.
func lookupAccount(name string) (*user.User, error) {
	if name == "" {
		return nil, nil
	}
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	account, err := findUser(name)
	if err != nil {
		return nil, codeError("EINVAL", "unknown user "+name, "")
	}
	if account.Uid == current.Uid {
		return nil, nil
	}
	return account, nil
}
