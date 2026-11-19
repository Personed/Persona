package worker

import (
	"os/exec"
	"os/user"
)

func findUser(name string) (*user.User, error) { return user.Lookup(name) }

// runAs fails: a Windows worker runs Harness programs as its own account, which
// is the one the service signs in with.
func runAs(_ *exec.Cmd, account *user.User) error {
	return codeError("EPERM", "the worker cannot start programs as "+account.Username, "")
}
