package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeHost answers docker commands from a table keyed by command prefix and records
// every call. Unknown commands fail, so an unexpected mutation fails the test.
type fakeHost struct {
	t       *testing.T
	answers map[string]func() (string, error)
	calls   []string
}

func newFakeHost(t *testing.T) *fakeHost {
	return &fakeHost{t: t, answers: map[string]func() (string, error){}}
}

func (f *fakeHost) on(prefix, out string) {
	f.answers[prefix] = func() (string, error) { return out, nil }
}

func (f *fakeHost) fail(prefix string) {
	f.answers[prefix] = func() (string, error) { return "", ErrCommand }
}

func (f *fakeHost) run(_ context.Context, args []string) (string, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	best := ""
	for prefix := range f.answers {
		if strings.HasPrefix(line, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		f.t.Logf("unexpected command: %s", line)
		return "", ErrCommand
	}
	return f.answers[best]()
}

func (f *fakeHost) called(prefix string) bool {
	return slices.ContainsFunc(f.calls, func(call string) bool { return strings.HasPrefix(call, prefix) })
}

func testConfig(network Network) DockerConfig {
	config := DockerConfig{Kind: "docker", ID: "work", Image: "persona-work:1", Volume: "persona-work",
		Network: network, CPUs: 1.5, MemoryMiB: 2048, Pids: 256, TimeoutSeconds: 120}
	if network == NetworkWeb {
		config.ProxyImage = "persona-proxy:1"
	}
	return config
}

// containerJSON is what docker inspect reports for a container created by
// CreateArguments; change mutates it before encoding.
func containerJSON(t *testing.T, config DockerConfig, running bool, change func(map[string]any)) string {
	network := string(config.Network)
	if config.Network != NetworkNone {
		network = config.Names().Network
	}
	info := map[string]any{
		"State":  map[string]any{"Running": running},
		"Config": map[string]any{"User": SandboxUser, "Image": config.Image, "Labels": map[string]any{specLabel: config.Spec()}},
		"HostConfig": map[string]any{
			"Privileged": false, "ReadonlyRootfs": true, "Init": true, "CapDrop": []string{"ALL"}, "CapAdd": nil,
			"NanoCpus": int64(config.CPUs * 1e9), "Memory": int64(config.MemoryMiB) << 20, "PidsLimit": config.Pids,
			"PidMode": "", "SecurityOpt": []string{"no-new-privileges:true"}, "NetworkMode": network,
		},
		"NetworkSettings": map[string]any{"Networks": map[string]any{network: map[string]any{}}},
		"Mounts": []map[string]any{
			{"Type": "volume", "Name": config.Volume, "Destination": SandboxHome},
			{"Type": "tmpfs", "Destination": "/tmp"},
		},
	}
	if change != nil {
		change(info)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func host(info map[string]any) map[string]any { return info["HostConfig"].(map[string]any) }

func newTestDocker(t *testing.T, config DockerConfig) (*Docker, *fakeHost) {
	t.Helper()
	fake := newFakeHost(t)
	d, err := NewDocker(config, fake.run)
	if err != nil {
		t.Fatal(err)
	}
	return d, fake
}

const (
	lsContainer = "docker container ls -aq --filter name=^/persona-work$"
	inspectWork = "docker container inspect --format {{json .}} persona-work"
	firewall    = "sudo -n " + FirewallCommand + " --check"
)

func TestSpecAndNamesMatchTypeScript(t *testing.T) {
	// Values computed by sandboxSpec and sandboxNames in packages/agent.
	config := testConfig(NetworkWeb)
	config.SSH = &SSH{Host: "box", KnownHostsFile: "/k", HostKeyAlias: "box"}
	if got := config.Spec(); got != "01bcb2af5a609e00931a843d5ed53a28fc9cd4ff713ab346655bccbbb67e56b4" {
		t.Fatalf("Spec() = %s", got)
	}
	names := config.Names()
	if names.Container != "persona-work" || names.Network != "persona-work" || names.Bridge != "persona-c00e13e" || names.Proxy != "persona-work-egress" {
		t.Fatalf("Names() = %+v", names)
	}
}

func TestParseDockerConfig(t *testing.T) {
	config, err := ParseDockerConfig([]byte(`{"kind":"docker","id":"work","image":"img:1","volume":"vol"}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.Network != NetworkNone || config.CPUs != 2 || config.MemoryMiB != 1024 || config.Pids != 256 || config.TimeoutSeconds != 120 {
		t.Fatalf("defaults = %+v", config)
	}
	for name, data := range map[string]string{
		"unknown field":    `{"kind":"docker","id":"work","image":"img","volume":"vol","privileged":true}`,
		"bad id":           `{"kind":"docker","id":"Work/../x","image":"img","volume":"vol"}`,
		"option image":     `{"kind":"docker","id":"work","image":"--privileged","volume":"vol"}`,
		"web needs proxy":  `{"kind":"docker","id":"work","image":"img","volume":"vol","network":"web"}`,
		"unknown network":  `{"kind":"docker","id":"work","image":"img","volume":"vol","network":"host"}`,
		"too little ram":   `{"kind":"docker","id":"work","image":"img","volume":"vol","memoryMiB":64}`,
		"ssh option host":  `{"kind":"docker","id":"work","image":"img","volume":"vol","ssh":{"host":"-oProxyCommand=x","knownHostsFile":"/k","hostKeyAlias":"a"}}`,
		"ssh no hostsfile": `{"kind":"docker","id":"work","image":"img","volume":"vol","ssh":{"host":"box","hostKeyAlias":"a"}}`,
	} {
		if _, err := ParseDockerConfig([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCreateArgumentsIsolate(t *testing.T) {
	args := strings.Join(testConfig(NetworkWeb).CreateArguments(), " ")
	for _, want := range []string{
		"--pull=never", "--user 1000:1000", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true",
		"--init", "--cpus 1.5", "--memory 2048m --memory-swap 2048m", "--pids-limit 256", "--network persona-work",
		"--mount type=volume,source=persona-work,target=/home/persona", "--env HTTPS_PROXY=" + ProxyURL,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("create arguments lack %q: %s", want, args)
		}
	}
	if args := strings.Join(testConfig(NetworkNone).CreateArguments(), " "); !strings.Contains(args, "--network none") || strings.Contains(args, "PROXY") {
		t.Errorf("none network arguments: %s", args)
	}
}

func TestInspect(t *testing.T) {
	config := testConfig(NetworkRestricted)
	d, fake := newTestDocker(t, config)
	fake.on(lsContainer, "")
	if state, err := d.Inspect(context.Background()); err != nil || state != StateMissing {
		t.Fatalf("missing: %v %v", state, err)
	}
	fake.on(lsContainer, "abc")
	fake.on(inspectWork, containerJSON(t, config, true, nil))
	if state, err := d.Inspect(context.Background()); err != nil || state != StateRunning {
		t.Fatalf("running: %v %v", state, err)
	}
	fake.on(inspectWork, containerJSON(t, config, false, nil))
	if state, err := d.Inspect(context.Background()); err != nil || state != StateStopped {
		t.Fatalf("stopped: %v %v", state, err)
	}
}

func TestInspectRejectsMismatchedContainer(t *testing.T) {
	config := testConfig(NetworkRestricted)
	for name, change := range map[string]func(map[string]any){
		"old spec":      func(i map[string]any) { i["Config"].(map[string]any)["Labels"] = map[string]any{specLabel: "old"} },
		"root user":     func(i map[string]any) { i["Config"].(map[string]any)["User"] = "0:0" },
		"privileged":    func(i map[string]any) { host(i)["Privileged"] = true },
		"writable root": func(i map[string]any) { host(i)["ReadonlyRootfs"] = false },
		"caps kept":     func(i map[string]any) { host(i)["CapDrop"] = []string{"NET_RAW"} },
		"caps added":    func(i map[string]any) { host(i)["CapAdd"] = []string{"SYS_ADMIN"} },
		"no nnp":        func(i map[string]any) { host(i)["SecurityOpt"] = []string{} },
		"extra security": func(i map[string]any) {
			host(i)["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=unconfined"}
		},
		"cpus":          func(i map[string]any) { host(i)["NanoCpus"] = 4e9 },
		"memory":        func(i map[string]any) { host(i)["Memory"] = 0 },
		"no pids limit": func(i map[string]any) { host(i)["PidsLimit"] = nil },
		"host pid":      func(i map[string]any) { host(i)["PidMode"] = "host" },
		"host network":  func(i map[string]any) { host(i)["NetworkMode"] = "host" },
		"two networks": func(i map[string]any) {
			i["NetworkSettings"] = map[string]any{"Networks": map[string]any{"persona-work": map[string]any{}, "bridge": map[string]any{}}}
		},
		"bind mount": func(i map[string]any) {
			i["Mounts"] = []map[string]any{{"Type": "bind", "Name": "", "Destination": SandboxHome}}
		},
		"extra mount": func(i map[string]any) {
			i["Mounts"] = append(i["Mounts"].([]map[string]any), map[string]any{"Type": "bind", "Destination": "/var/run/docker.sock"})
		},
		"other volume": func(i map[string]any) {
			i["Mounts"] = []map[string]any{{"Type": "volume", "Name": "other", "Destination": SandboxHome}}
		},
	} {
		d, fake := newTestDocker(t, config)
		fake.on(lsContainer, "abc")
		fake.on(inspectWork, containerJSON(t, config, true, change))
		if _, err := d.Inspect(context.Background()); !errors.Is(err, ErrMismatch) {
			t.Errorf("%s: err = %v, want ErrMismatch", name, err)
		}
		if err := d.Start(context.Background()); !errors.Is(err, ErrMismatch) || fake.called("docker container start") {
			t.Errorf("%s: Start err = %v, started = %v", name, err, fake.called("docker container start"))
		}
	}
}

func TestStartProvisionsRestrictedContainer(t *testing.T) {
	d, fake := newTestDocker(t, testConfig(NetworkRestricted))
	fake.on(lsContainer, "")
	fake.on(firewall, "")
	fake.on("docker network ls -q --filter name=^persona-work$", "")
	fake.on("docker network create --driver bridge", "net")
	fake.on("docker network inspect --format", "persona-c00e13e")
	fake.on("docker container create", "id")
	fake.on("docker container start persona-work", "")
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{lsContainer, firewall, "docker network ls", "docker network create", "docker network inspect",
		"docker container create", firewall, "docker container start persona-work"}
	if len(fake.calls) != len(want) {
		t.Fatalf("calls = %q", fake.calls)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(fake.calls[i], prefix) {
			t.Fatalf("call %d = %q, want %q", i, fake.calls[i], prefix)
		}
	}
	if !strings.Contains(fake.calls[3], "com.docker.network.bridge.name=persona-c00e13e") {
		t.Errorf("network create = %q", fake.calls[3])
	}
}

func TestStartFailsClosed(t *testing.T) {
	config := testConfig(NetworkRestricted)
	t.Run("missing firewall rules", func(t *testing.T) {
		d, fake := newTestDocker(t, config)
		fake.on(lsContainer, "")
		fake.fail(firewall)
		if err := d.Start(context.Background()); !errors.Is(err, ErrNetwork) {
			t.Fatalf("err = %v", err)
		}
		if fake.called("docker container create") || fake.called("docker container start") {
			t.Fatalf("created or started without firewall: %q", fake.calls)
		}
	})
	t.Run("firewall removed after create", func(t *testing.T) {
		d, fake := newTestDocker(t, config)
		fake.on(lsContainer, "abc")
		fake.on(inspectWork, containerJSON(t, config, false, nil))
		fake.fail(firewall)
		if err := d.Start(context.Background()); !errors.Is(err, ErrNetwork) || fake.called("docker container start") {
			t.Fatalf("err = %v, calls = %q", err, fake.calls)
		}
	})
	t.Run("bridge not firewalled", func(t *testing.T) {
		d, fake := newTestDocker(t, config)
		fake.on(lsContainer, "")
		fake.on(firewall, "")
		fake.on("docker network ls", "net")
		fake.on("docker network inspect --format", "br-1234")
		if err := d.Start(context.Background()); !errors.Is(err, ErrNetwork) || fake.called("docker container create") {
			t.Fatalf("err = %v, calls = %q", err, fake.calls)
		}
	})
	t.Run("docker unavailable", func(t *testing.T) {
		d, fake := newTestDocker(t, config)
		fake.fail(lsContainer)
		if err := d.Start(context.Background()); !errors.Is(err, ErrCommand) || len(fake.calls) != 1 {
			t.Fatalf("err = %v, calls = %q", err, fake.calls)
		}
	})
	t.Run("unreadable inspection", func(t *testing.T) {
		d, fake := newTestDocker(t, config)
		fake.on(lsContainer, "abc")
		fake.on(inspectWork, "not json")
		if err := d.Start(context.Background()); err == nil || fake.called("docker container start") {
			t.Fatalf("err = %v, calls = %q", err, fake.calls)
		}
	})
}

func TestStopAndRebuild(t *testing.T) {
	config := testConfig(NetworkNone)
	d, fake := newTestDocker(t, config)
	fake.on(lsContainer, "abc")
	fake.on(inspectWork, containerJSON(t, config, true, nil))
	fake.on("docker container stop --time 10 persona-work", "")
	if err := d.Stop(context.Background()); err != nil || !fake.called("docker container stop --time 10 persona-work") {
		t.Fatalf("Stop: %v %q", err, fake.calls)
	}

	d, fake = newTestDocker(t, config)
	fake.on("docker container ls -aq --filter label=persona.computer=work --filter name=^/persona-work$", "abc")
	fake.on("docker container rm --force persona-work", "")
	fake.on("docker container create", "id")
	fake.on(lsContainer, "abc")
	fake.on(inspectWork, containerJSON(t, config, false, nil))
	fake.on("docker container start persona-work", "")
	if err := d.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"docker container rm --force persona-work", "docker container create", "docker container start persona-work"} {
		if !fake.called(prefix) {
			t.Errorf("Rebuild did not run %q: %q", prefix, fake.calls)
		}
	}
}

func proxyJSON(t *testing.T, change func(map[string]any)) string {
	config := testConfig(NetworkWeb)
	return containerJSON(t, config, true, func(i map[string]any) {
		i["State"] = map[string]any{"Running": true, "Health": map[string]any{"Status": "healthy"}}
		i["Config"] = map[string]any{"User": SandboxUser, "Image": config.ProxyImage}
		i["NetworkSettings"] = map[string]any{"Networks": map[string]any{"persona-work": map[string]any{}, "bridge": map[string]any{}}}
		i["Mounts"] = []map[string]any{{"Type": "tmpfs", "Destination": "/tmp"}}
		if change != nil {
			change(i)
		}
	})
}

const (
	inspectNet   = "docker network inspect --format {{json .}} persona-work"
	inspectProxy = "docker container inspect --format {{json .}} persona-work-egress"
	squid        = "docker exec persona-work-egress squid -k check"
	isolatedNet  = `{"Internal":true,"EnableIPv6":false,"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"isolated"}}`
)

func TestWebNetworkCheck(t *testing.T) {
	d, fake := newTestDocker(t, testConfig(NetworkWeb))
	fake.on(inspectNet, isolatedNet)
	fake.on(inspectProxy, proxyJSON(t, nil))
	fake.on(squid, "")
	if err := d.CheckNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(*fakeHost){
		"external network": func(f *fakeHost) {
			f.on(inspectNet, `{"Internal":false,"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"isolated"}}`)
		},
		"ipv6": func(f *fakeHost) {
			f.on(inspectNet, strings.Replace(isolatedNet, `"EnableIPv6":false`, `"EnableIPv6":true`, 1))
		},
		"routed gateway":  func(f *fakeHost) { f.on(inspectNet, `{"Internal":true,"Options":{}}`) },
		"missing network": func(f *fakeHost) { f.fail(inspectNet) },
		"missing proxy":   func(f *fakeHost) { f.fail(inspectProxy) },
		"squid broken":    func(f *fakeHost) { f.fail(squid) },
		"unhealthy proxy": func(f *fakeHost) {
			f.on(inspectProxy, proxyJSON(t, func(i map[string]any) { i["State"] = map[string]any{"Running": true} }))
		},
		"other proxy image": func(f *fakeHost) {
			f.on(inspectProxy, proxyJSON(t, func(i map[string]any) { i["Config"].(map[string]any)["Image"] = "evil" }))
		},
		"proxy no init": func(f *fakeHost) {
			f.on(inspectProxy, proxyJSON(t, func(i map[string]any) { host(i)["Init"] = false }))
		},
		"proxy caps": func(f *fakeHost) {
			f.on(inspectProxy, proxyJSON(t, func(i map[string]any) { host(i)["CapAdd"] = []string{"NET_ADMIN"} }))
		},
		"proxy volume": func(f *fakeHost) {
			f.on(inspectProxy, proxyJSON(t, func(i map[string]any) {
				i["Mounts"] = []map[string]any{{"Type": "volume", "Destination": "/etc/squid"}}
			}))
		},
		"proxy on host net": func(f *fakeHost) {
			f.on(inspectProxy, proxyJSON(t, func(i map[string]any) {
				i["NetworkSettings"] = map[string]any{"Networks": map[string]any{"persona-work": map[string]any{}, "host": map[string]any{}}}
			}))
		},
	} {
		setup(fake)
		if err := d.CheckNetwork(context.Background()); !errors.Is(err, ErrNetwork) {
			t.Errorf("%s: err = %v, want ErrNetwork", name, err)
		}
		fake.on(inspectNet, isolatedNet)
		fake.on(inspectProxy, proxyJSON(t, nil))
		fake.on(squid, "")
	}
}

func TestWebNetworkStartWaitsForHealthyProxy(t *testing.T) {
	interval, timeout := pollInterval, proxyStartTimeout
	pollInterval, proxyStartTimeout = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { pollInterval, proxyStartTimeout = interval, timeout })

	config := testConfig(NetworkWeb)
	d, fake := newTestDocker(t, config)
	fake.on(lsContainer, "abc")
	fake.on(inspectWork, containerJSON(t, config, false, nil))
	fake.on("docker network ls -q --filter name=^persona-work$", "net")
	fake.on("docker container ls -aq --filter name=^/persona-work-egress$", "")
	fake.on("docker container create --init --pull=never --name persona-work-egress", "id")
	fake.on("docker network connect bridge persona-work-egress", "")
	fake.on("docker container start", "")
	states := []string{`{"Running":true,"Health":{"Status":"starting"}}`, `{"Running":true,"Health":{"Status":"healthy"}}`}
	fake.answers["docker inspect --format {{json .State}} persona-work-egress"] = func() (string, error) {
		state := states[0]
		if len(states) > 1 {
			states = states[1:]
		}
		return state, nil
	}
	fake.on(inspectNet, isolatedNet)
	fake.on(inspectProxy, proxyJSON(t, nil))
	fake.on(squid, "")
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v (calls %q)", err, fake.calls)
	}
	if !fake.called("docker container start persona-work") {
		t.Fatalf("work container not started: %q", fake.calls)
	}

	d, fake = newTestDocker(t, config)
	fake.on(lsContainer, "abc")
	fake.on(inspectWork, containerJSON(t, config, false, nil))
	fake.on("docker network ls", "net")
	fake.on("docker container ls -aq --filter name=^/persona-work-egress$", "abc")
	fake.on("docker container start persona-work-egress", "")
	fake.on("docker inspect --format {{json .State}}", `{"Running":true,"Health":{"Status":"starting"}}`)
	if err := d.Start(context.Background()); !errors.Is(err, ErrNetwork) || slices.Contains(fake.calls, "docker container start persona-work") {
		t.Fatalf("never-healthy proxy: err = %v, calls = %q", err, fake.calls)
	}
}

func TestCommandsOverSSH(t *testing.T) {
	config := testConfig(NetworkNone)
	d, _ := newTestDocker(t, config)
	daemon, err := d.Daemon()
	if err != nil || strings.Join(daemon, " ") != "docker container exec -i --user 1000:1000 --workdir /home/persona persona-work "+SandboxDaemon {
		t.Fatalf("Daemon() = %q, %v", daemon, err)
	}
	process, err := d.Process(7)
	if err != nil || strings.Join(process, " ") != "docker container exec -i --user 1000:1000 --workdir /home/persona/bots/7 persona-work node "+SandboxWorker {
		t.Fatalf("Process() = %q, %v", process, err)
	}
	if _, err := d.Process(0); err == nil {
		t.Fatal("Process(0) accepted")
	}

	config.SSH = &SSH{Host: "box", User: "ops", Port: 2222, KnownHostsFile: "/etc/persona/known%hosts", HostKeyAlias: "box"}
	args, err := config.HostCommand("docker", "exec", "it's")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	for _, want := range []string{"ssh -T -a -x", "StrictHostKeyChecking=yes", `UserKnownHostsFile="/etc/persona/known%%hosts"`, "-l ops -p 2222 -- box", `'docker' 'exec' 'it'\''s'`} {
		if !strings.Contains(line, want) {
			t.Errorf("ssh command lacks %q: %s", want, line)
		}
	}
	config.SSH.Host = "-oProxyCommand=x"
	if _, err := config.HostCommand("docker"); err == nil {
		t.Fatal("option-like host accepted")
	}
}
