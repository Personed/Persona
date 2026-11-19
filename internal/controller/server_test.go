package controller

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const credential = "0123456789abcdef0123456789abcdef"

// TestHelperProcess is the channel process: it prints its arguments, then echoes stdin.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("PERSONA_CONTROLLER_HELPER") != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	fmt.Printf("args:%s\n", strings.Join(args, " "))
	_, _ = io.Copy(os.Stdout, os.Stdin)
	os.Exit(0)
}

type fakeDriver struct {
	config     DockerConfig
	mu         sync.Mutex
	state      State
	networkErr error
	changeErr  error
	block      chan struct{}
	calls      []string
	active     atomic.Int32
	overlap    atomic.Bool
}

func newFakeDriver(state State) *fakeDriver {
	return &fakeDriver{config: testConfig(NetworkRestricted), state: state}
}

func (f *fakeDriver) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeDriver) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (f *fakeDriver) Config() DockerConfig { return f.config }

func (f *fakeDriver) Inspect(context.Context) (State, error) {
	f.record("inspect")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeDriver) CheckNetwork(context.Context) error {
	f.record("network")
	return f.networkErr
}

func (f *fakeDriver) change(name string, state State) error {
	if f.active.Add(1) > 1 {
		f.overlap.Store(true)
	}
	defer f.active.Add(-1)
	f.record(name)
	if f.block != nil {
		<-f.block
	}
	if f.changeErr != nil {
		return f.changeErr
	}
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
	return nil
}

func (f *fakeDriver) Start(context.Context) error   { return f.change("start", StateRunning) }
func (f *fakeDriver) Stop(context.Context) error    { return f.change("stop", StateStopped) }
func (f *fakeDriver) Rebuild(context.Context) error { return f.change("rebuild", StateRunning) }

func (f *fakeDriver) helper() []string {
	return []string{os.Args[0], "-test.run=^TestHelperProcess$", "--"}
}

func (f *fakeDriver) Daemon() ([]string, error) {
	f.record("daemon")
	return f.helper(), nil
}

func (f *fakeDriver) Process(botID int) ([]string, error) {
	f.record("process")
	return append(f.helper(), "bot", fmt.Sprint(botID)), nil
}

func newTestServer(t *testing.T, driver Driver) (*Server, *httptest.Server) {
	t.Helper()
	t.Setenv("PERSONA_CONTROLLER_HELPER", "1")
	service := NewServer(driver, credential, nil)
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	return service, server
}

func request(t *testing.T, server *httptest.Server, method, path, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func TestServerAuthAndRoutes(t *testing.T) {
	driver := newFakeDriver(StateStopped)
	_, server := newTestServer(t, driver)
	bearer := "Bearer " + credential
	for _, tc := range []struct {
		method, path, auth string
		code               int
	}{
		{"GET", "/health", "", 401},
		{"GET", "/health", "Bearer wrong", 401},
		{"POST", "/work/start", "Bearer " + credential + "x", 401},
		{"GET", "/health", bearer, 200},
		{"GET", "/work/nope", bearer, 404},
		{"GET", "/other/status", bearer, 404},
		{"GET", "/Work/status", bearer, 404},
		{"POST", "/work/status", bearer, 404},
		{"GET", "/work/start", bearer, 404},
	} {
		if code, _ := request(t, server, tc.method, tc.path, tc.auth); code != tc.code {
			t.Errorf("%s %s (%q) = %d, want %d", tc.method, tc.path, tc.auth, code, tc.code)
		}
	}
	if len(driver.calls) != 0 {
		t.Fatalf("unauthorized or readiness requests touched Docker: %q", driver.calls)
	}
}

func TestServerLifecycle(t *testing.T) {
	driver := newFakeDriver(StateMissing)
	_, server := newTestServer(t, driver)
	bearer := "Bearer " + credential
	if code, body := request(t, server, "GET", "/work/status", bearer); code != 200 || body != `{"state":"missing","network":"restricted"}` {
		t.Fatalf("status = %d %s", code, body)
	}
	if code, body := request(t, server, "POST", "/work/start", bearer); code != 200 || body != `{"state":"running","network":"restricted"}` {
		t.Fatalf("start = %d %s", code, body)
	}
	if code, body := request(t, server, "POST", "/work/stop", bearer); code != 200 || !strings.Contains(body, `"stopped"`) {
		t.Fatalf("stop = %d %s", code, body)
	}

	driver.changeErr = errors.New("docker: permission denied on /var/run/docker.sock")
	if code, body := request(t, server, "POST", "/work/rebuild", bearer); code != 503 || body != prepareError {
		t.Fatalf("failed rebuild = %d %s", code, body)
	}
	driver.changeErr = nil
	driver.state = StateRunning
	driver.networkErr = ErrNetwork
	if code, body := request(t, server, "GET", "/work/status", bearer); code != 503 || body != prepareError {
		t.Fatalf("running without firewall reported as %d %s", code, body)
	}
}

func TestChangesAreSerialized(t *testing.T) {
	driver := newFakeDriver(StateStopped)
	driver.block = make(chan struct{})
	target := NewTarget(driver)
	var wg sync.WaitGroup
	for _, action := range []Action{ActionStart, ActionStop, ActionRebuild} {
		wg.Go(func() { _, _ = target.Change(context.Background(), action) })
	}
	statusDone := make(chan struct{})
	go func() {
		_, _ = target.Status(context.Background())
		close(statusDone)
	}()
	for range 3 {
		driver.block <- struct{}{}
	}
	wg.Wait()
	<-statusDone
	if driver.overlap.Load() {
		t.Fatal("lifecycle changes overlapped")
	}
}

func dialConnect(t *testing.T, server *httptest.Server, path string, headers ...string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	head := "CONNECT " + path + " HTTP/1.1\r\nHost: controller\r\n" + strings.Join(headers, "\r\n")
	if len(headers) > 0 {
		head += "\r\n"
	}
	if _, err := io.WriteString(conn, head+"\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, _ := reader.ReadString('\n')
	return conn, reader, strings.TrimSpace(status)
}

const daemonTokenValue = "0123456789abcdef0123456789abcdef"

func TestConnectFailsClosed(t *testing.T) {
	auth := "Authorization: Bearer " + credential
	token := "X-Persona-Daemon-Token: " + daemonTokenValue
	for name, tc := range map[string]struct {
		path    string
		headers []string
		state   State
		network error
		want    string
	}{
		"no credential":    {"/work/daemon", []string{token}, StateRunning, nil, "403"},
		"wrong credential": {"/work/process", []string{"Authorization: Bearer nope", "X-Persona-Bot-Id: 1"}, StateRunning, nil, "403"},
		"bad token":        {"/work/daemon", []string{auth, "X-Persona-Daemon-Token: ZZ"}, StateRunning, nil, "403"},
		"bad bot id":       {"/work/process", []string{auth, "X-Persona-Bot-Id: 0"}, StateRunning, nil, "403"},
		"status route":     {"/work/status", []string{auth, token}, StateRunning, nil, "403"},
		"stopped":          {"/work/daemon", []string{auth, token}, StateStopped, nil, "503"},
		"firewall missing": {"/work/process", []string{auth, "X-Persona-Bot-Id: 3"}, StateRunning, ErrNetwork, "503"},
	} {
		driver := newFakeDriver(tc.state)
		driver.networkErr = tc.network
		_, server := newTestServer(t, driver)
		_, _, status := dialConnect(t, server, tc.path, tc.headers...)
		if !strings.Contains(status, " "+tc.want+" ") {
			t.Errorf("%s: status %q, want %s", name, status, tc.want)
		}
		if driver.has("daemon") || driver.has("process") {
			t.Errorf("%s: channel command built: %q", name, driver.calls)
		}
	}
}

func TestConnectRunsChannelUntilChange(t *testing.T) {
	driver := newFakeDriver(StateRunning)
	service, server := newTestServer(t, driver)
	conn, reader, status := dialConnect(t, server, "/work/daemon",
		"Authorization: Bearer "+credential, "X-Persona-Daemon-Token: "+daemonTokenValue)
	if status != "HTTP/1.1 200 Connection Established" {
		t.Fatalf("status = %q", status)
	}
	_, _ = reader.ReadString('\n')
	if line, _ := reader.ReadString('\n'); line != "args:serve --token "+daemonTokenValue+"\n" {
		t.Fatalf("daemon args = %q", line)
	}
	if _, err := io.WriteString(conn, "ping\x00\xff\n"); err != nil {
		t.Fatal(err)
	}
	if line, _ := reader.ReadString('\n'); line != "ping\x00\xff\n" {
		t.Fatalf("echo = %q", line)
	}
	target, _ := service.target("/work/status")
	if _, err := target.Change(context.Background(), ActionRebuild); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("channel survived a rebuild")
	}
	deadline := time.Now().Add(5 * time.Second)
	for target.Processes() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := target.Processes(); n != 0 {
		t.Fatalf("%d processes left", n)
	}
}

func TestChannelRefusedAfterChange(t *testing.T) {
	t.Setenv("PERSONA_CONTROLLER_HELPER", "1")
	driver := newFakeDriver(StateRunning)
	target := NewTarget(driver)
	channel, err := target.Prepare(context.Background(), "", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Change(context.Background(), ActionStop); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.Start(io.Discard); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale channel: err = %v", err)
	}
	if _, err := target.Prepare(context.Background(), "", 0); err == nil {
		t.Fatal("daemon without token prepared")
	}
}

func TestCloseStopsUsedTargets(t *testing.T) {
	used, unused := newFakeDriver(StateStopped), newFakeDriver(StateRunning)
	unused.config.ID = "idle"
	service := NewServer(used, credential, func(string) (Driver, error) { return unused, nil })
	target, _ := service.target("/work/start")
	if _, err := target.Change(context.Background(), ActionStart); err != nil {
		t.Fatal(err)
	}
	if idle, _ := service.target("/idle/status"); idle == nil {
		t.Fatal("created target missing")
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !used.has("stop") || unused.has("stop") {
		t.Fatalf("used stop = %v, unused stop = %v", used.has("stop"), unused.has("stop"))
	}
	if _, err := target.Change(context.Background(), ActionStart); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after close: %v", err)
	}
	if created, _ := service.target("/late/status"); created != nil {
		t.Fatal("target created while closing")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("credential", credential+"\n")
	write("short", "short")
	computer := `{"kind":"docker","id":"work","image":"img:1","volume":"persona-work","network":"restricted"}`
	config, err := LoadConfig(write("ok.json", `{"port":8787,"credentialFile":"credential","computer":`+computer+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "127.0.0.1" || config.credential != credential || config.docker.Network != NetworkRestricted {
		t.Fatalf("config = %+v", config)
	}
	service, err := config.NewServer(newFakeHost(t).run)
	if err != nil {
		t.Fatal(err)
	}
	if target, _ := service.target("/bot-2/status"); target == nil || target.driver.Config().Volume != "persona-bot-2-work" {
		t.Fatal("created target lacks its own volume")
	}
	for name, data := range map[string]string{
		"short credential": `{"port":1,"credentialFile":"short","computer":` + computer + `}`,
		"missing file":     `{"port":1,"credentialFile":"absent","computer":` + computer + `}`,
		"not docker":       `{"port":1,"credentialFile":"credential","computer":{"kind":"local","id":"work"}}`,
		"unknown field":    `{"port":1,"credentialFile":"credential","computer":` + computer + `,"insecure":true}`,
		"no port":          `{"credentialFile":"credential","computer":` + computer + `}`,
	} {
		if _, err := LoadConfig(write(name+".json", data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
