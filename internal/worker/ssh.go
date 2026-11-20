package worker

import (
	"strconv"
	"strings"
)

// SSHTarget is a work computer reached over OpenSSH. The host key must already
// be in KnownHostsFile; the connection never prompts, forwards, or reads user
// ssh configuration.
type SSHTarget struct {
	Host           string
	Port           int
	User           string
	IdentityFile   string
	KnownHostsFile string
	// HostKeyAlias names the host's entry in KnownHostsFile, for hosts reached
	// through changing addresses.
	HostKeyAlias string
	// Program is the persona binary on the work computer; empty means persona on PATH.
	Program string
	// SSH is the local ssh client; empty means ssh on PATH.
	SSH string
}

// SSH starts the worker on target through the system ssh client.
func SSH(target SSHTarget) Transport {
	return func(token string) []string {
		program := target.Program
		if program == "" {
			program = "persona"
		}
		return append(SSHArguments(target), shellQuote(program, "worker", "serve", "--token", token))
	}
}

// SSHArguments returns the ssh command line up to and including the host.
func SSHArguments(target SSHTarget) []string {
	ssh := target.SSH
	if ssh == "" {
		ssh = "ssh"
	}
	args := []string{
		ssh, "-T", "-F", "none",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "PermitLocalCommand=no",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=6",
		"-o", "ConnectTimeout=15",
	}
	if target.KnownHostsFile != "" {
		args = append(args, "-o", "UserKnownHostsFile="+target.KnownHostsFile, "-o", "GlobalKnownHostsFile=none")
	}
	if target.HostKeyAlias != "" {
		args = append(args, "-o", "HostKeyAlias="+target.HostKeyAlias)
	}
	if target.IdentityFile != "" {
		args = append(args, "-i", target.IdentityFile, "-o", "IdentitiesOnly=yes")
	}
	if target.Port != 0 {
		args = append(args, "-p", strconv.Itoa(target.Port))
	}
	if target.User != "" {
		args = append(args, "-l", target.User)
	}
	return append(args, "--", target.Host)
}

// shellQuote joins argv for the remote POSIX shell that runs the command.
func shellQuote(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
