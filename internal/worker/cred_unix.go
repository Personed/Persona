//go:build !windows

package worker

import (
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// findUser accepts a user name or uid[:gid], as docker --user does.
func findUser(name string) (*user.User, error) {
	uid, gid, numeric := strings.Cut(name, ":")
	if _, err := strconv.ParseUint(uid, 10, 32); err == nil {
		account, err := user.LookupId(uid)
		if err != nil {
			account = &user.User{Uid: uid, Gid: uid, Username: uid}
		}
		if numeric {
			account.Gid = gid
		}
		return account, nil
	}
	return user.Lookup(name)
}

// runAs switches the program to account. Only a worker running as root may
// start programs as another user.
func runAs(cmd *exec.Cmd, account *user.User) error {
	if os.Geteuid() != 0 {
		return codeError("EPERM", "the worker cannot start programs as "+account.Username, "")
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return codeError("EINVAL", "invalid uid "+account.Uid, "")
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return codeError("EINVAL", "invalid gid "+account.Gid, "")
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	return nil
}
