package computers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Personed/Persona/internal/controller"
)

// --- docker driver (docker.ts), over internal/controller ---

type dockerDriver struct {
	config Config
	target *controller.Target
	docker *controller.Docker
}

func newDocker(config Config) driver {
	if config.Network == "" {
		config.Network = "none"
	}
	d := &dockerDriver{config: config}
	raw, err := json.Marshal(config)
	if err == nil {
		if parsed, err := controller.ParseDockerConfig(raw); err == nil {
			if docker, err := controller.NewDocker(parsed, nil); err == nil {
				d.docker = docker
				d.target = controller.NewTarget(docker)
			}
		}
	}
	return d
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

func (d *dockerDriver) directory() string { return "/home/persona" }
func (d *dockerDriver) network() string   { return d.config.Network }

func (d *dockerDriver) inspect(ctx context.Context) (string, error) {
	if d.target == nil {
		return "", errors.New("configuração do computador inválida")
	}
	status, err := d.target.Status(ctx)
	if err != nil {
		return "", err
	}
	switch status.State {
	case controller.StateRunning:
		return "running", nil
	case controller.StateStopped:
		return "stopped", nil
	}
	return "missing", nil
}

func (d *dockerDriver) checkNetwork(ctx context.Context) error {
	if d.target == nil {
		return errors.New("configuração do computador inválida")
	}
	return nil // Status already checked the network inside the lifecycle lock.
}

func (d *dockerDriver) start(ctx context.Context) error {
	_, err := d.target.Change(ctx, controller.ActionStart)
	return err
}

func (d *dockerDriver) stop(ctx context.Context) error {
	_, err := d.target.Change(ctx, controller.ActionStop)
	return err
}

func (d *dockerDriver) rebuild(ctx context.Context) error {
	_, err := d.target.Change(ctx, controller.ActionRebuild)
	return err
}

// daemon returns the argv of `persona worker serve` inside the container for
// the Bot's workspace (docker.ts process: docker exec -i).
func (d *dockerDriver) daemon(botID int64) ([]string, error) {
	if d.docker == nil {
		return nil, errors.New("configuração do computador inválida")
	}
	config := d.docker.Config()
	workdir := d.directory() + "/bots/" + strconv.FormatInt(botID, 10) + "/workspace"
	return config.HostCommand("docker", "container", "exec", "-i", "--user", controller.SandboxUser,
		"--workdir", workdir, config.Names().Container, "persona", "worker", "serve")
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

type controllerStatus struct {
	State   string `json:"state"`
	Network string `json:"network"`
}

func (c *controllerDriver) request(ctx context.Context, action, method string) (controllerStatus, error) {
	var out controllerStatus
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
