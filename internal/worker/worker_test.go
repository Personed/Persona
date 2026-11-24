package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"golang.org/x/crypto/ssh"

	"github.com/Personed/Persona/internal/computer"
)

// TestMain lets the test binary act as the persona binary and as a portable cat.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		if os.Args[1] == "mcp" && len(os.Args) > 2 && os.Args[2] == "list" {
			fmt.Fprintf(os.Stdout, `[{"name":"persona"},{"name":"docs","source":%q,"enabled":false,"state":"disabled"},{"name":"local","state":"connected"}]`, mustGetwd())
			os.Exit(0)
		}
		switch os.Args[1] {
		case "worker":
			if err := Main(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case "mcp-stdio":
			if err := testMCPServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case "cat":
			_, _ = io.Copy(os.Stdout, os.Stdin)
			fmt.Fprint(os.Stderr, "done")
			os.Exit(3)
		case "computer-driver":
			if err := testComputerDriver().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case "browser-driver":
			if err := testBrowserDriver().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		case "close":
			_ = os.WriteFile(os.Getenv("TEST_BROWSER_CLOSED"), []byte(os.Getenv("AGENT_BROWSER_SOCKET_DIR")+"\n"+os.Getenv("AGENT_BROWSER_SESSION")), 0o600)
			os.Exit(0)
		case "linger":
			// A browser daemon that ignores `close`.
			time.Sleep(time.Minute)
			os.Exit(0)
		case "stream":
			_ = os.WriteFile(os.Getenv("TEST_BROWSER_SESSION_FILE"), []byte(os.Getenv("AGENT_BROWSER_SESSION")), 0o600)
			fmt.Printf(`{"success":true,"data":{"enabled":true,"port":%s}}`, os.Getenv("TEST_BROWSER_STREAM_PORT"))
			os.Exit(0)
		case "set", "install":
			if log := os.Getenv("TEST_BROWSER_CLI"); log != "" {
				file, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err == nil {
					fmt.Fprintln(file, strings.Join(append(os.Args[1:], os.Getenv("AGENT_BROWSER_HEADED")), " "))
					_ = file.Close()
				}
			}
			if os.Getenv("TEST_BROWSER_FAIL") != "" {
				fmt.Fprintln(os.Stderr, "browser unavailable")
				os.Exit(1)
			}
			os.Exit(0)
		case "pi-gateway":
			if err := testPiGateway(os.Getenv("PERSONA_PI_MCP_URL")); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return cwd
}

func executable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

var _ computer.Target = (*Client)(nil)

// transports dials a worker over local stdio and over SSH to an in-process
// server that runs the worker command it receives.
func transports(t *testing.T) map[string]*Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clients := map[string]*Client{}
	local, err := Dial(ctx, Local(executable(t)), Options{ID: "local", Kind: computer.Local})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	clients["stdio"] = local
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Log("ssh client not found; skipping the SSH transport")
		return clients
	}
	remote, err := Dial(ctx, SSH(sshServer(t)), Options{ID: "remote", Kind: computer.Remote})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	clients["ssh"] = remote
	return clients
}

func sshServer(t *testing.T) SSHTarget {
	t.Helper()
	dir := t.TempDir()
	hostKey, hostPublic := generateKey(t, filepath.Join(dir, "host"))
	_, clientPublic := generateKey(t, filepath.Join(dir, "client"))
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), clientPublic.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("unknown key")
	}}
	config.AddHostKey(hostKey)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSSH(t, conn, config)
		}
	}()
	knownHosts := filepath.Join(dir, "known_hosts")
	line := "persona-test " + string(ssh.MarshalAuthorizedKey(hostPublic))
	if err := os.WriteFile(knownHosts, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return SSHTarget{
		Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, User: "persona",
		IdentityFile: filepath.Join(dir, "client"), KnownHostsFile: knownHosts,
		HostKeyAlias: "persona-test", Program: executable(t),
	}
}

// serveSSH runs each exec request's command as argv, as a POSIX shell would run
// the single-quoted words the SSH transport sends.
func serveSSH(t *testing.T, conn net.Conn, config *ssh.ServerConfig) {
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "")
			continue
		}
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = channel.Close() }()
			for request := range requests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				_ = ssh.Unmarshal(request.Payload, &payload)
				_ = request.Reply(true, nil)
				argv := unquote(payload.Command)
				cmd := exec.Command(argv[0], argv[1:]...)
				cmd.Stdin, cmd.Stdout, cmd.Stderr = channel, channel, channel.Stderr()
				status := 0
				if err := cmd.Run(); err != nil {
					t.Logf("ssh command: %v", err)
					status = 1
				}
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
				return
			}
		}()
	}
}

func unquote(command string) []string {
	var argv []string
	for _, word := range strings.Split(command, "' '") {
		argv = append(argv, strings.ReplaceAll(strings.Trim(word, "'"), `'\''`, "'"))
	}
	return argv
}

func code(err error) string {
	var wire *Error
	if errors.As(err, &wire) {
		return wire.Code
	}
	return fmt.Sprint(err)
}

func TestFiles(t *testing.T) {
	for name, client := range transports(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			hello := client.Hello()
			if hello.Protocol != Protocol || hello.OS != runtime.GOOS || hello.Home == "" {
				t.Fatalf("hello = %+v", hello)
			}
			path := filepath.Join(dir, "a", "b", "file.txt")
			large := bytes.Repeat([]byte("0123456789abcdef"), chunkSize/8)
			if err := client.WriteFile(ctx, path, bytes.NewReader(large)); err != nil {
				t.Fatal(err)
			}
			reader, err := client.ReadFile(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			read, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil || !bytes.Equal(read, large) {
				t.Fatalf("read %d bytes, err %v", len(read), err)
			}
			info, err := client.Stat(ctx, path)
			if err != nil || info.Size != int64(len(large)) || info.Dir || info.ModTime == 0 || info.Symlink || info.Links != 1 {
				t.Fatalf("stat = %+v, %v", info, err)
			}
			hard := filepath.Join(dir, "hard.txt")
			if err := os.Link(path, hard); err != nil {
				t.Fatal(err)
			}
			if info, err := client.Stat(ctx, hard); err != nil || info.Links != 2 || info.Symlink {
				t.Fatalf("hard link stat = %+v, %v", info, err)
			}
			if err := os.Remove(hard); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link.txt")
			if err := os.Symlink(path, link); err == nil {
				if info, err := client.Stat(ctx, link); err != nil || !info.Symlink || info.Links != 1 || info.Size == int64(len(large)) {
					t.Fatalf("symlink stat = %+v, %v", info, err)
				}
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
			} else if runtime.GOOS != "windows" {
				t.Fatal(err)
			}
			if _, err := client.Stat(ctx, filepath.Join(dir, "missing")); code(err) != "ENOENT" {
				t.Fatalf("missing stat: %v", err)
			}
			if err := client.WriteFile(ctx, filepath.Join(path, "child"), strings.NewReader("x")); code(err) != "ENOTDIR" {
				t.Fatalf("write below a file: %v", err)
			}
			if _, err := client.ReadFile(ctx, dir); code(err) != "EISDIR" {
				t.Fatalf("read a directory: %v", err)
			}
			if err := client.Mkdir(ctx, filepath.Join(dir, "a"), true); err != nil {
				t.Fatalf("recursive mkdir of existing directory: %v", err)
			}
			if err := client.Mkdir(ctx, filepath.Join(dir, "a"), false); code(err) != "EEXIST" {
				t.Fatalf("mkdir existing: %v", err)
			}
			if err := client.Mkdir(ctx, path, true); code(err) != "EEXIST" {
				t.Fatalf("recursive mkdir of file: %v", err)
			}
			moved := filepath.Join(dir, "a", "moved.txt")
			if err := client.Rename(ctx, path, moved); err != nil {
				t.Fatal(err)
			}
			resolved, err := client.Realpath(ctx, moved)
			if want, _ := filepath.EvalSymlinks(moved); err != nil || resolved != want {
				t.Fatalf("realpath = %q, %v; want %q", resolved, err, want)
			}
			entries, err := client.ReadDir(ctx, filepath.Join(dir, "a"))
			if err != nil || len(entries) != 2 {
				t.Fatalf("readdir = %+v, %v", entries, err)
			}
			if err := client.Remove(ctx, filepath.Join(dir, "a"), false, false); code(err) != "ERR_FS_EISDIR" {
				t.Fatalf("rm directory: %v", err)
			}
			if err := client.Remove(ctx, filepath.Join(dir, "missing"), false, true); err != nil {
				t.Fatalf("forced rm of missing path: %v", err)
			}
			if err := client.Remove(ctx, filepath.Join(dir, "missing"), false, false); code(err) != "ENOENT" {
				t.Fatalf("rm missing: %v", err)
			}
			if err := client.Remove(ctx, filepath.Join(dir, "a"), true, false); err != nil {
				t.Fatal(err)
			}
			if _, _, err := client.call(ctx, "chmod", nil, nil, nil); code(err) != "ENOSYS" {
				t.Fatalf("unknown op: %v", err)
			}
		})
	}
}

func TestListFiles(t *testing.T) {
	for name, client := range transports(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			for _, file := range []string{
				".git/HEAD", ".gitignore", "b.txt", "a/x.log", "a/keep.log", "a/.hidden", "build/out", "node_modules/m.js", "src/z.go",
			} {
				write(t, filepath.Join(dir, file), "")
			}
			write(t, filepath.Join(dir, ".gitignore"), "*.log\n!keep.log\n/build/\n")
			entries, truncated, err := client.ListFiles(ctx, dir, ListOptions{Exclude: []string{"node_modules"}})
			if err != nil || truncated {
				t.Fatalf("list: %v, truncated %v", err, truncated)
			}
			want := "f.gitignore da fa/.hidden fa/keep.log fb.txt dsrc fsrc/z.go"
			if got := listed(entries); got != want {
				t.Fatalf("list = %q, want %q", got, want)
			}
			entries, _, err = client.ListFiles(ctx, dir, ListOptions{NoHidden: true, NoGitignore: true})
			want = "da fa/keep.log fa/x.log fb.txt dbuild fbuild/out dnode_modules fnode_modules/m.js dsrc fsrc/z.go"
			if got := listed(entries); err != nil || got != want {
				t.Fatalf("list = %q, %v; want %q", got, err, want)
			}
			entries, truncated, err = client.ListFiles(ctx, dir, ListOptions{MaxEntries: 2})
			if err != nil || !truncated || len(entries) != 2 {
				t.Fatalf("bounded list = %v, truncated %v, %v", entries, truncated, err)
			}
			if _, _, err := client.ListFiles(ctx, filepath.Join(dir, "b.txt"), ListOptions{}); code(err) != "ENOTDIR" {
				t.Fatalf("list a file: %v", err)
			}
		})
	}
}

func listed(entries []ListEntry) string {
	var words []string
	for _, entry := range entries {
		words = append(words, string(entry.Kind)+entry.Path)
	}
	return strings.Join(words, " ")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type output struct {
	mu   sync.Mutex
	text map[string]string
}

func (o *output) add(stream string, text []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.text[stream] += string(text)
}

func TestExec(t *testing.T) {
	if findShell() == "" {
		t.Skip("no shell")
	}
	for name, client := range transports(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			out := &output{text: map[string]string{}}
			exit, err := client.Exec(ctx, ExecOptions{
				Command: `printf "%s:%s" "$GREETING" "$(basename "$PWD")"; echo oops >&2; exit 7`,
				Dir:     dir, Env: map[string]string{"GREETING": "héllo"}, Output: out.add,
			})
			if err != nil || exit != 7 || out.text["stdout"] != "héllo:"+filepath.Base(dir) || out.text["stderr"] != "oops\n" {
				t.Fatalf("exec = %d, %v, %q", exit, err, out.text)
			}
			if exit, err := client.Exec(ctx, ExecOptions{Argv: []string{executable(t), "cat"}, Dir: dir}); err != nil || exit != 3 {
				t.Fatalf("argv exec = %d, %v", exit, err)
			}
			if _, err := client.Exec(ctx, ExecOptions{Command: "true", Dir: filepath.Join(dir, "missing")}); code(err) != "spawn_error" {
				t.Fatalf("missing cwd: %v", err)
			}
			if _, err := client.Exec(ctx, ExecOptions{Command: "true", Dir: dir, ShellPath: filepath.Join(dir, "nosh")}); code(err) != "shell_unavailable" {
				t.Fatalf("missing shell: %v", err)
			}
			started := time.Now()
			if _, err := client.Exec(ctx, ExecOptions{Command: "sleep 30 & wait", Dir: dir, Timeout: 200 * time.Millisecond}); code(err) != "timeout" || time.Since(started) > 10*time.Second {
				t.Fatalf("timeout: %v after %v", err, time.Since(started))
			}
			cancelled, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			started = time.Now()
			if _, err := client.Exec(cancelled, ExecOptions{Command: "sleep 30 & wait", Dir: dir}); code(err) != "aborted" || time.Since(started) > 10*time.Second {
				t.Fatalf("cancel: %v after %v", err, time.Since(started))
			}
			if runtime.GOOS != "windows" {
				if exit, err := client.Exec(ctx, ExecOptions{Command: "kill -9 $$", Dir: dir}); err != nil || exit != 137 {
					t.Fatalf("signal exit = %d, %v", exit, err)
				}
			}
		})
	}
}

func TestProcess(t *testing.T) {
	for name, client := range transports(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var stdout, stderr bytes.Buffer
			process, err := client.Start(ctx, computer.Command{
				Path: executable(t), Args: []string{"cat"}, Dir: t.TempDir(),
				Stdin: strings.NewReader(`{"jsonrpc":"2.0"}` + "\n"), Stdout: &stdout, Stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			exit, err := process.Wait()
			if err != nil || exit != 3 || stdout.String() != `{"jsonrpc":"2.0"}`+"\n" || stderr.String() != "done" {
				t.Fatalf("process = %d, %v, %q, %q", exit, err, stdout.String(), stderr.String())
			}
			reader, writer := io.Pipe()
			defer func() { _ = writer.Close() }()
			process, err = client.Start(ctx, computer.Command{Path: executable(t), Args: []string{"cat"}, Dir: t.TempDir(), Stdin: reader})
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Kill(); err != nil {
				t.Fatal(err)
			}
			if exit, err := process.Wait(); err != nil || exit == 0 || exit == 3 {
				t.Fatalf("killed process = %d, %v", exit, err)
			}
			if _, err := client.Start(ctx, computer.Command{Path: "persona-missing-program", Dir: t.TempDir()}); code(err) != "spawn_error" {
				t.Fatalf("missing program: %v", err)
			}
		})
	}
}

func TestLostConnection(t *testing.T) {
	for name, client := range transports(t) {
		t.Run(name, func(t *testing.T) {
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			var wire *Error
			if _, err := client.Stat(context.Background(), t.TempDir()); !errors.As(err, &wire) || !wire.Lost {
				t.Fatalf("stat after close: %v", err)
			}
		})
	}
}

func TestParseIgnoreRule(t *testing.T) {
	cases := []struct {
		pattern, path string
		dir, match    bool
	}{
		{"*.log", "a/b.log", false, true},
		{"/build", "build", true, true},
		{"/build", "a/build", true, false},
		{"out/", "out", false, false},
		{"out/", "x/out", true, true},
		{"a/**/z", "a/z", false, true},
		{"a/**/z", "a/b/c/z", false, true},
		{"**/foo", "x/y/foo", false, true},
		{"doc/*.txt", "doc/a/b.txt", false, false},
		{"[!a]b", "cb", false, true},
		{`\#x`, "#x", false, true},
	}
	for _, c := range cases {
		rule, ok := parseIgnoreRule(c.pattern)
		got := ok && (!rule.dirOnly || c.dir) && rule.pattern.MatchString(c.path)
		if got != c.match {
			t.Errorf("%q against %q (dir %v) = %v, want %v", c.pattern, c.path, c.dir, got, c.match)
		}
	}
}
