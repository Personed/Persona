package computers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	sandboxHome      = "/home/persona"
	sandboxUser      = "1000:1000"
	firewallCommand  = "/usr/local/sbin/persona-computer-egress"
	computerLabel    = "persona.computer"
	computerSpecLabel = "persona.spec"
)

// --- docker driver (docker.ts) ---

type dockerDriver struct {
	config Config
}

func newDocker(config Config) driver {
	if config.Network == "" {
		config.Network = "none"
	}
	return &dockerDriver{config: config}
}

func (d *dockerDriver) binding() json.RawMessage {
	host := "docker-local"
	var user string
	var port int
	if d.config.SSH != nil {
		host = d.config.SSH.Host
		user = d.config.SSH.User
		port = d.config.SSH.Port
	}
	out, _ := json.Marshal(map[string]any{
		"kind": "docker", "id": d.config.ID, "volume": d.config.Volume,
		"host": host, "user": user, "port": port,
	})
	return out
}

func (d *dockerDriver) directory() string { return sandboxHome }
func (d *dockerDriver) network() string   { return d.config.Network }

type sandboxNames struct {
	container string
	network   string
	bridge    string
}

func (d *dockerDriver) names() sandboxNames {
	sum := sha256.Sum256([]byte(d.config.ID))
	return sandboxNames{
		container: "persona-" + d.config.ID,
		network:   "persona-" + d.config.ID,
		bridge:    "persona-c" + hex.EncodeToString(sum[:])[:6],
	}
}

// spec is the config hash pinned on the container label, sandboxSpec of docker.ts.
func (d *dockerDriver) spec() string {
	spec := d.config
	spec.SSH = nil
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func quoteArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// hostCommand prefixes args with ssh when the target is remote.
func (d *dockerDriver) hostCommand(args []string) []string {
	if d.config.SSH == nil {
		return args
	}
	ssh := d.config.SSH
	argv := []string{"ssh", "-T", "-F", "none", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=15"}
	if ssh.KnownHostsFile != "" {
		argv = append(argv, "-o", "UserKnownHostsFile="+ssh.KnownHostsFile, "-o", "GlobalKnownHostsFile=none")
	}
	if ssh.IdentityFile != "" {
		argv = append(argv, "-i", ssh.IdentityFile)
	}
	if ssh.Port != 0 {
		argv = append(argv, "-p", strconv.Itoa(ssh.Port))
	}
	host := ssh.Host
	if ssh.User != "" {
		host = ssh.User + "@" + ssh.Host
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = quoteArgument(arg)
	}
	return append(argv, host, strings.Join(quoted, " "))
}

func (d *dockerDriver) command(ctx context.Context, args []string) (string, error) {
	argv := d.hostCommand(args)
	if len(argv) == 0 {
		return "", errors.New("Comando de controle do computador ausente.")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return "", errors.New("Requisição de controle do computador não concluída. Verifique o Docker, chaves de host SSH e permissões.")
	}
	return strings.TrimSpace(string(out)), nil
}

const dockerInspection = `{"running":{{json .State.Running}},"spec":{{json (index .Config.Labels "persona.spec")}},"user":{{json .Config.User}},"privileged":{{json .HostConfig.Privileged}},"readonly":{{json .HostConfig.ReadonlyRootfs}},"caps":{{json .HostConfig.CapDrop}},"added":{{json .HostConfig.CapAdd}},"cpus":{{json .HostConfig.NanoCpus}},"memory":{{json .HostConfig.Memory}},"pids":{{json .HostConfig.PidsLimit}},"pidMode":{{json .HostConfig.PidMode}},"networks":{{json .NetworkSettings.Networks}},"security":{{json .HostConfig.SecurityOpt}},"network":{{json .HostConfig.NetworkMode}},"mounts":{{json .Mounts}}}`

type inspection struct {
	Running    bool              `json:"running"`
	Spec       string            `json:"spec"`
	User       string            `json:"user"`
	Privileged bool              `json:"privileged"`
	Readonly   bool              `json:"readonly"`
	Caps       []string          `json:"caps"`
	Added      []string          `json:"added"`
	CPUs       int64             `json:"cpus"`
	Memory     int64             `json:"memory"`
	Pids       int64             `json:"pids"`
	PidMode    string            `json:"pidMode"`
	Networks   map[string]any    `json:"networks"`
	Security   []string          `json:"security"`
	Network    string            `json:"network"`
	Mounts     []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
	} `json:"mounts"`
}

func (d *dockerDriver) inspect(ctx context.Context) (string, error) {
	names := d.names()
	id, err := d.command(ctx, []string{"docker", "container", "ls", "-aq",
		"--filter", "name=^/" + names.container + "$"})
	if err != nil {
		return "", err
	}
	if id == "" {
		return "missing", nil
	}
	raw, err := d.command(ctx, []string{"docker", "container", "inspect",
		"--format", dockerInspection, names.container})
	if err != nil {
		return "", err
	}
	var info inspection
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return "", err
	}
	var mounts []inspectionMount
	for _, m := range info.Mounts {
		if m.Destination != "/tmp" {
			mounts = append(mounts, inspectionMount(m))
		}
	}
	wantNetwork := names.network
	if d.config.Network == "none" {
		wantNetwork = "none"
	}
	mismatch := info.Spec != d.spec() ||
		info.User != sandboxUser ||
		info.Privileged ||
		!info.Readonly ||
		!contains(info.Caps, "ALL") ||
		len(info.Added) != 0 ||
		len(info.Security) != 1 || !contains(info.Security, "no-new-privileges:true") ||
		info.CPUs != int64(d.config.CPUs*1_000_000_000) ||
		info.Memory != d.config.MemoryMiB*1024*1024 ||
		info.Pids != d.config.Pids ||
		info.PidMode != "" ||
		(info.Running && len(info.Networks) != 1) ||
		info.Network != wantNetwork ||
		len(mounts) != 1 ||
		mounts[0].Type != "volume" ||
		mounts[0].Name != d.config.Volume ||
		mounts[0].Destination != sandboxHome
	if mismatch {
		return "", errors.New("工作容器与配置或隔离要求不一致。请停止后重建工作容器。")
	}
	if info.Running {
		return "running", nil
	}
	return "stopped", nil
}

type inspectionMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Destination string `json:"Destination"`
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func (d *dockerDriver) checkNetwork(ctx context.Context) error {
	if d.config.Network == "web" {
		return d.webCheck(ctx)
	}
	if d.config.Network != "restricted" {
		return nil
	}
	_, err := d.command(ctx, []string{"sudo", "-n", firewallCommand, "--check"})
	return err
}

func (d *dockerDriver) webCheck(ctx context.Context) error {
	// Web proxy health check, WebNetwork.check of web-network.ts.
	names := d.names()
	_, err := d.command(ctx, []string{"docker", "network", "inspect",
		"--format", `{{index .Options "com.docker.network.bridge.name"}}`,
		names.network + "-proxy"})
	return err
}

func (d *dockerDriver) provision(ctx context.Context) error {
	if err := d.checkNetwork(ctx); err != nil {
		return err
	}
	if d.config.Network == "restricted" {
		names := d.names()
		exists, err := d.command(ctx, []string{"docker", "network", "ls", "-q",
			"--filter", "name=^" + names.network + "$"})
		if err != nil {
			return err
		}
		if exists == "" {
			if _, err := d.command(ctx, []string{"docker", "network", "create",
				"--driver", "bridge",
				"--label", computerLabel + "=" + d.config.ID,
				"--opt", "com.docker.network.bridge.name=" + names.bridge,
				names.network}); err != nil {
				return err
			}
		}
		actual, err := d.command(ctx, []string{"docker", "network", "inspect",
			"--format", `{{index .Options "com.docker.network.bridge.name"}}`,
			names.network})
		if err != nil {
			return err
		}
		if actual != names.bridge {
			return errors.New("沙箱网络缺少受限防火墙接口。")
		}
	}
	args := d.createArguments()
	_, err := d.command(ctx, args)
	return err
}

func (d *dockerDriver) createArguments() []string {
	names := d.names()
	args := []string{
		"docker", "container", "create", "--pull=never",
		"--name", names.container,
		"--label", computerLabel + "=" + d.config.ID,
		"--label", computerSpecLabel + "=" + d.spec(),
		"--user", sandboxUser,
		"--workdir", sandboxHome,
		"--init", "--read-only", "--cap-drop=ALL",
		"--security-opt=no-new-privileges:true",
		"--cpus", strconv.FormatFloat(d.config.CPUs, 'f', -1, 64),
		"--memory", strconv.FormatInt(d.config.MemoryMiB, 10) + "m",
		"--memory-swap", strconv.FormatInt(d.config.MemoryMiB, 10) + "m",
		"--pids-limit", strconv.FormatInt(d.config.Pids, 10),
		"--network",
	}
	if d.config.Network == "none" {
		args = append(args, "none")
	} else {
		args = append(args, names.network)
	}
	args = append(args,
		"--mount", "type=volume,source="+d.config.Volume+",target="+sandboxHome,
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=128m,mode=1777",
		d.config.Image)
	return args
}

func (d *dockerDriver) start(ctx context.Context) error {
	state, err := d.inspect(ctx)
	if err != nil {
		return err
	}
	if state == "missing" {
		if err := d.provision(ctx); err != nil {
			return err
		}
	}
	if err := d.checkNetwork(ctx); err != nil {
		return err
	}
	if state != "running" {
		_, err := d.command(ctx, []string{"docker", "container", "start", d.names().container})
		return err
	}
	return nil
}

func (d *dockerDriver) stop(ctx context.Context) error {
	state, err := d.inspect(ctx)
	if err != nil {
		return err
	}
	if state == "running" {
		if _, err := d.command(ctx, []string{"docker", "container", "stop",
			"--time", "10", d.names().container}); err != nil {
			return err
		}
	}
	return nil
}

func (d *dockerDriver) rebuild(ctx context.Context) error {
	name := d.names().container
	id, err := d.command(ctx, []string{"docker", "container", "ls", "-aq",
		"--filter", "label=" + computerLabel + "=" + d.config.ID,
		"--filter", "name=^/" + name + "$"})
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := d.command(ctx, []string{"docker", "container", "rm", "--force", name}); err != nil {
			return err
		}
	}
	if err := d.provision(ctx); err != nil {
		return err
	}
	return d.start(ctx)
}

// --- controller driver (controller.ts) ---

type controllerDriver struct {
	config Config
}

func newController(config Config) (driver, error) {
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("控制端地址无效。")
	}
	return &controllerDriver{config: config}, nil
}

func (c *controllerDriver) binding() json.RawMessage {
	out, _ := json.Marshal(map[string]any{
		"kind": "controller", "id": c.config.ID, "endpoint": c.config.Endpoint,
	})
	return out
}

func (c *controllerDriver) directory() string { return "/home/persona" }
func (c *controllerDriver) network() string   { return c.config.Network }

func (c *controllerDriver) request(ctx context.Context, action, method string) (struct {
	State   string `json:"state"`
	Network string `json:"network"`
}, error) {
	var out struct {
		State   string `json:"state"`
		Network string `json:"network"`
	}
	token, err := os.ReadFile(c.config.CredentialFile)
	if err != nil {
		return out, err
	}
	endpoint := strings.TrimRight(c.config.Endpoint, "/") + "/" + url.PathEscape(c.config.ID) + "/" + action
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode/100 != 2 {
		return out, errors.New("工作电脑控制端未完成请求。请核对实际状态。")
	}
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *controllerDriver) inspect(ctx context.Context) (string, error) {
	out, err := c.request(ctx, "status", http.MethodGet)
	if err != nil {
		return "", err
	}
	return out.State, nil
}

func (c *controllerDriver) checkNetwork(ctx context.Context) error {
	out, err := c.request(ctx, "status", http.MethodGet)
	if err != nil {
		return err
	}
	if out.Network != c.config.Network {
		return errors.New("工作电脑网络策略与配置不匹配。")
	}
	return nil
}

func (c *controllerDriver) start(ctx context.Context) error {
	_, err := c.request(ctx, "start", http.MethodPost)
	return err
}

func (c *controllerDriver) stop(ctx context.Context) error {
	_, err := c.request(ctx, "stop", http.MethodPost)
	return err
}

func (c *controllerDriver) rebuild(ctx context.Context) error {
	_, err := c.request(ctx, "rebuild", http.MethodPost)
	return err
}

// unused: keeps runtime import when Windows paths are needed later.
var _ = runtime.GOOS
var _ = fmt.Sprint
