package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
)

// Work container constants shared with deploy/workspace.Dockerfile.
const (
	SandboxHome     = "/home/persona"
	SandboxDaemon   = "/usr/local/bin/pi-env"
	SandboxUser     = "1000:1000"
	SandboxWorker   = "/opt/persona/worker.mjs"
	FirewallCommand = "/usr/local/sbin/persona-computer-egress"
	ProxyURL        = "http://egress:3128"

	computerLabel = "persona.computer"
	specLabel     = "persona.spec"
)

// Fail-closed errors. Callers must not start or connect to a container after one.
var (
	// ErrMismatch reports a container that differs from the configuration or the
	// isolation requirements. Stop and rebuild it.
	ErrMismatch = errors.New("controller: work container does not match the configuration or isolation requirements; stop and rebuild it")
	// ErrNetwork reports a network, proxy or firewall that does not enforce the policy.
	ErrNetwork = errors.New("controller: work network does not enforce the configured egress policy")
)

// State is the lifecycle state of a work container.
type State string

// Container states.
const (
	StateMissing State = "missing"
	StateStopped State = "stopped"
	StateRunning State = "running"
)

// Docker manages one work container through the docker CLI, locally or over SSH.
type Docker struct {
	config DockerConfig
	run    Runner
	web    *webNetwork
}

// NewDocker validates config and returns its driver. A nil run uses ExecRunner.
func NewDocker(config DockerConfig, run Runner) (*Docker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if run == nil {
		run = ExecRunner
	}
	d := &Docker{config: config, run: run}
	if config.Network == NetworkWeb {
		d.web = &webNetwork{docker: d, network: config.Names().Network, proxy: config.Names().Proxy}
	}
	return d, nil
}

// Config returns the validated configuration.
func (d *Docker) Config() DockerConfig { return d.config }

func (d *Docker) command(ctx context.Context, args ...string) (string, error) {
	full, err := d.config.HostCommand(args...)
	if err != nil {
		return "", err
	}
	return d.run(ctx, full)
}

func (d *Docker) exists(ctx context.Context, filters ...string) (bool, error) {
	args := []string{"docker", "container", "ls", "-aq"}
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	id, err := d.command(ctx, args...)
	return id != "", err
}

type containerInfo struct {
	State struct {
		Running bool
		Health  *struct{ Status string }
	}
	Config struct {
		User   string
		Image  string
		Labels map[string]string
	}
	HostConfig struct {
		Privileged     bool
		ReadonlyRootfs bool
		Init           *bool
		CapDrop        []string
		CapAdd         []string
		NanoCpus       int64
		Memory         int64
		PidsLimit      *int64
		PidMode        string
		SecurityOpt    []string
		NetworkMode    string
	}
	NetworkSettings struct {
		Networks map[string]json.RawMessage
	}
	Mounts []struct {
		Type        string
		Name        string
		Destination string
	}
}

func (d *Docker) inspectContainer(ctx context.Context, name string) (containerInfo, error) {
	var info containerInfo
	out, err := d.command(ctx, "docker", "container", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return info, fmt.Errorf("controller: unreadable container inspection: %w", err)
	}
	return info, nil
}

// Inspect reports the container state and returns ErrMismatch when an existing
// container is not the one the configuration describes.
func (d *Docker) Inspect(ctx context.Context) (State, error) {
	names := d.config.Names()
	found, err := d.exists(ctx, "name=^/"+names.Container+"$")
	if err != nil {
		return "", err
	}
	if !found {
		return StateMissing, nil
	}
	info, err := d.inspectContainer(ctx, names.Container)
	if err != nil {
		return "", err
	}
	if err := d.verify(info); err != nil {
		return "", err
	}
	if info.State.Running {
		return StateRunning, nil
	}
	return StateStopped, nil
}

func (d *Docker) verify(info containerInfo) error {
	c, host := d.config, info.HostConfig
	var mounts []int
	for i, mount := range info.Mounts {
		if mount.Destination != "/tmp" {
			mounts = append(mounts, i)
		}
	}
	network := string(NetworkNone)
	if c.Network != NetworkNone {
		network = c.Names().Network
	}
	ok := info.Config.Labels[specLabel] == c.Spec() &&
		info.Config.User == SandboxUser &&
		!host.Privileged &&
		host.ReadonlyRootfs &&
		slices.Contains(host.CapDrop, "ALL") &&
		len(host.CapAdd) == 0 &&
		slices.Equal(host.SecurityOpt, []string{"no-new-privileges:true"}) &&
		host.NanoCpus == int64(math.Round(c.CPUs*1e9)) &&
		host.Memory == int64(c.MemoryMiB)*1024*1024 &&
		host.PidsLimit != nil && *host.PidsLimit == int64(c.Pids) &&
		host.PidMode == "" &&
		(!info.State.Running || len(info.NetworkSettings.Networks) == 1) &&
		host.NetworkMode == network &&
		len(mounts) == 1
	if ok {
		mount := info.Mounts[mounts[0]]
		ok = mount.Type == "volume" && mount.Name == c.Volume && mount.Destination == SandboxHome
	}
	if !ok {
		return ErrMismatch
	}
	return nil
}

// CheckNetwork verifies the egress policy without changing it: the proxy for web,
// the host firewall rules for restricted, nothing for none.
func (d *Docker) CheckNetwork(ctx context.Context) error {
	switch d.config.Network {
	case NetworkWeb:
		return d.web.check(ctx)
	case NetworkRestricted:
		if _, err := d.command(ctx, "sudo", "-n", FirewallCommand, "--check"); err != nil {
			return fmt.Errorf("%w: restricted egress firewall rules are missing: %w", ErrNetwork, err)
		}
	}
	return nil
}

// CreateArguments is the docker command that creates the work container.
func (c DockerConfig) CreateArguments() []string {
	names := c.Names()
	network := string(NetworkNone)
	if c.Network != NetworkNone {
		network = names.Network
	}
	memory := strconv.Itoa(c.MemoryMiB) + "m"
	args := []string{
		"docker", "container", "create", "--pull=never",
		"--name", names.Container,
		"--label", computerLabel + "=" + c.ID,
		"--label", specLabel + "=" + c.Spec(),
		"--user", SandboxUser,
		"--workdir", SandboxHome,
		"--init", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true",
		"--cpus", strconv.FormatFloat(c.CPUs, 'f', -1, 64),
		"--memory", memory, "--memory-swap", memory,
		"--pids-limit", strconv.Itoa(c.Pids),
		"--network", network,
		"--mount", "type=volume,source=" + c.Volume + ",target=" + SandboxHome,
	}
	if c.Network == NetworkWeb {
		for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			args = append(args, "--env", name+"="+ProxyURL)
		}
	}
	return append(args, "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m,mode=1777", c.Image)
}

func (d *Docker) provision(ctx context.Context) error {
	if d.web != nil {
		if err := d.web.start(ctx); err != nil {
			return err
		}
	}
	if err := d.CheckNetwork(ctx); err != nil {
		return err
	}
	if d.config.Network == NetworkRestricted {
		names := d.config.Names()
		id, err := d.command(ctx, "docker", "network", "ls", "-q", "--filter", "name=^"+names.Network+"$")
		if err != nil {
			return err
		}
		if id == "" {
			if _, err := d.command(ctx, "docker", "network", "create", "--driver", "bridge",
				"--label", computerLabel+"="+d.config.ID,
				"--opt", "com.docker.network.bridge.name="+names.Bridge, names.Network); err != nil {
				return err
			}
		}
		bridge, err := d.command(ctx, "docker", "network", "inspect", "--format",
			`{{index .Options "com.docker.network.bridge.name"}}`, names.Network)
		if err != nil {
			return err
		}
		if bridge != names.Bridge {
			return fmt.Errorf("%w: network %s is not on the firewalled bridge %s", ErrNetwork, names.Network, names.Bridge)
		}
	}
	_, err := d.command(ctx, d.config.CreateArguments()...)
	return err
}

// Start creates the container when missing and starts it after the network check
// passes. A mismatched container or failed check leaves it stopped.
func (d *Docker) Start(ctx context.Context) error {
	state, err := d.Inspect(ctx)
	if err != nil {
		return err
	}
	if state == StateMissing {
		err = d.provision(ctx)
	} else if d.web != nil {
		err = d.web.start(ctx)
	}
	if err != nil {
		return err
	}
	if err := d.CheckNetwork(ctx); err != nil {
		return err
	}
	if state != StateRunning {
		_, err = d.command(ctx, "docker", "container", "start", d.config.Names().Container)
	}
	return err
}

// Stop stops the container and its egress proxy.
func (d *Docker) Stop(ctx context.Context) error {
	state, err := d.Inspect(ctx)
	if err != nil {
		return err
	}
	if state == StateRunning {
		if _, err := d.command(ctx, "docker", "container", "stop", "--time", "10", d.config.Names().Container); err != nil {
			return err
		}
	}
	if d.web != nil {
		return d.web.stop(ctx)
	}
	return nil
}

// Rebuild removes this computer's container and proxy, then provisions and starts
// a new one. Only a container carrying this computer's label is removed.
func (d *Docker) Rebuild(ctx context.Context) error {
	name := d.config.Names().Container
	found, err := d.exists(ctx, "label="+computerLabel+"="+d.config.ID, "name=^/"+name+"$")
	if err != nil {
		return err
	}
	if found {
		if _, err := d.command(ctx, "docker", "container", "rm", "--force", name); err != nil {
			return err
		}
	}
	if d.web != nil {
		if err := d.web.rebuild(ctx); err != nil {
			return err
		}
	}
	if err := d.provision(ctx); err != nil {
		return err
	}
	return d.Start(ctx)
}

// Daemon is the command that runs pi-env inside the container on stdio.
func (d *Docker) Daemon() ([]string, error) {
	return d.config.HostCommand(d.exec(SandboxHome, SandboxDaemon)...)
}

// Process is the command that runs a Bot's worker inside the container on stdio.
func (d *Docker) Process(botID int) ([]string, error) {
	if botID <= 0 {
		return nil, errors.New("controller: invalid bot id")
	}
	return d.config.HostCommand(d.exec(SandboxHome+"/bots/"+strconv.Itoa(botID), "node", SandboxWorker)...)
}

func (d *Docker) exec(workdir string, command ...string) []string {
	return append([]string{"docker", "container", "exec", "-i", "--user", SandboxUser,
		"--workdir", workdir, d.config.Names().Container}, command...)
}

const gatewayMode = "com.docker.network.bridge.gateway_mode_ipv4"

type webNetwork struct {
	docker  *Docker
	network string
	proxy   string
}

func (w *webNetwork) check(ctx context.Context) error {
	out, err := w.docker.command(ctx, "docker", "network", "inspect", "--format", "{{json .}}", w.network)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	var network struct {
		Internal   bool
		EnableIPv6 bool
		Options    map[string]string
	}
	if err := json.Unmarshal([]byte(out), &network); err != nil || !network.Internal || network.EnableIPv6 || network.Options[gatewayMode] != "isolated" {
		return fmt.Errorf("%w: %s is not an isolated internal network", ErrNetwork, w.network)
	}
	proxy, err := w.docker.inspectContainer(ctx, w.proxy)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	host := proxy.HostConfig
	_, internal := proxy.NetworkSettings.Networks[w.network]
	_, bridge := proxy.NetworkSettings.Networks["bridge"]
	ok := proxy.State.Running &&
		proxy.State.Health != nil && proxy.State.Health.Status == "healthy" &&
		proxy.Config.Image == w.docker.config.ProxyImage &&
		proxy.Config.User == SandboxUser &&
		host.Init != nil && *host.Init &&
		host.ReadonlyRootfs &&
		!host.Privileged &&
		slices.Contains(host.CapDrop, "ALL") &&
		len(host.CapAdd) == 0 &&
		slices.Contains(host.SecurityOpt, "no-new-privileges:true") &&
		len(proxy.NetworkSettings.Networks) == 2 && internal && bridge
	for _, mount := range proxy.Mounts {
		ok = ok && mount.Type == "tmpfs" && mount.Destination == "/tmp"
	}
	if !ok {
		return fmt.Errorf("%w: egress proxy %s does not match the isolation configuration", ErrNetwork, w.proxy)
	}
	if _, err := w.docker.command(ctx, "docker", "exec", w.proxy, "squid", "-k", "check"); err != nil {
		return fmt.Errorf("%w: egress proxy is not serving: %w", ErrNetwork, err)
	}
	return nil
}

func (w *webNetwork) proxyExists(ctx context.Context) (bool, error) {
	return w.docker.exists(ctx, "name=^/"+w.proxy+"$")
}

func (w *webNetwork) start(ctx context.Context) error {
	d := w.docker
	id, err := d.command(ctx, "docker", "network", "ls", "-q", "--filter", "name=^"+w.network+"$")
	if err != nil {
		return err
	}
	if id == "" {
		if _, err := d.command(ctx, "docker", "network", "create", "--internal", "--opt", gatewayMode+"=isolated", w.network); err != nil {
			return err
		}
	}
	found, err := w.proxyExists(ctx)
	if err != nil {
		return err
	}
	if !found {
		if _, err := d.command(ctx, "docker", "container", "create", "--init", "--pull=never",
			"--name", w.proxy, "--network", w.network, "--network-alias", "egress",
			"--user", SandboxUser, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges:true",
			"--memory", "128m", "--cpus", "0.5", "--pids-limit", "64",
			"--tmpfs", "/tmp:rw,nosuid,nodev,size=32m,mode=1777", d.config.ProxyImage); err != nil {
			return err
		}
		if _, err := d.command(ctx, "docker", "network", "connect", "bridge", w.proxy); err != nil {
			return err
		}
	}
	if _, err := d.command(ctx, "docker", "container", "start", w.proxy); err != nil {
		return err
	}
	if err := w.waitHealthy(ctx); err != nil {
		return err
	}
	return w.check(ctx)
}

func (w *webNetwork) waitHealthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, proxyStartTimeout)
	defer cancel()
	for {
		out, err := w.docker.command(ctx, "docker", "inspect", "--format", "{{json .State}}", w.proxy)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrNetwork, err)
		}
		var state struct {
			Running bool
			Health  *struct{ Status string }
		}
		if err := json.Unmarshal([]byte(out), &state); err != nil {
			return fmt.Errorf("%w: unreadable proxy state", ErrNetwork)
		}
		status := ""
		if state.Health != nil {
			status = state.Health.Status
		}
		if !state.Running || status == "unhealthy" {
			return fmt.Errorf("%w: egress proxy failed to start", ErrNetwork)
		}
		if status == "healthy" {
			return nil
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return fmt.Errorf("%w: egress proxy did not become healthy", ErrNetwork)
		}
	}
}

func (w *webNetwork) stop(ctx context.Context) error {
	found, err := w.proxyExists(ctx)
	if err == nil && found {
		_, err = w.docker.command(ctx, "docker", "container", "stop", "--time", "10", w.proxy)
	}
	return err
}

func (w *webNetwork) rebuild(ctx context.Context) error {
	found, err := w.proxyExists(ctx)
	if err == nil && found {
		_, err = w.docker.command(ctx, "docker", "container", "rm", "--force", w.proxy)
	}
	return err
}
