package hosting

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Config is the shared persona.config.json settings with paths resolved to absolute paths.
type Config struct {
	Model                 *ModelConfig    `json:"model,omitempty"`
	PublicURL             string          `json:"publicUrl,omitempty"`
	Host                  string          `json:"host"`
	Port                  int             `json:"port"`
	DataDir               string          `json:"dataDir"`
	StaticDir             string          `json:"staticDir"`
	Deployment            string          `json:"deployment"`
	DesktopCredentialFile string          `json:"desktopCredentialFile,omitempty"`
	Computer              ComputerConfig  `json:"computer"`
	WorkComputer          *ComputerConfig `json:"workComputer,omitempty"`
	ConfigFile            string          `json:"-"`
}

// ModelConfig identifies the default model.
type ModelConfig struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// ComputerConfig selects a process, Docker, or controller work computer.
type ComputerConfig struct {
	Kind           string     `json:"kind"`
	ID             string     `json:"id,omitempty"`
	Image          string     `json:"image,omitempty"`
	Volume         string     `json:"volume,omitempty"`
	Network        string     `json:"network,omitempty"`
	ProxyImage     string     `json:"proxyImage,omitempty"`
	CPUs           float64    `json:"cpus,omitempty"`
	MemoryMiB      int        `json:"memoryMiB,omitempty"`
	PIDs           int        `json:"pids,omitempty"`
	TimeoutSeconds int        `json:"timeoutSeconds,omitempty"`
	SSH            *SSHConfig `json:"ssh,omitempty"`
	Endpoint       string     `json:"endpoint,omitempty"`
	CredentialFile string     `json:"credentialFile,omitempty"`
}

// SSHConfig configures a Docker computer on an SSH host.
type SSHConfig struct {
	Host           string `json:"host"`
	User           string `json:"user,omitempty"`
	Port           int    `json:"port,omitempty"`
	IdentityFile   string `json:"identityFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile"`
	HostKeyAlias   string `json:"hostKeyAlias"`
	ConfigFile     string `json:"configFile,omitempty"`
}

// LoadConfig reads PERSONA_CONFIG (or persona.config.json in the working directory)
// and applies the same runtime environment overrides as the TypeScript service.
func LoadConfig() (Config, error) {
	name, ok := os.LookupEnv("PERSONA_CONFIG")
	if !ok {
		name = "persona.config.json"
	}
	return loadConfigFile(name, os.LookupEnv)
}

func loadConfigFile(name string, lookup func(string) (string, bool)) (Config, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return Config{}, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return Config{}, errors.New("hosting: expected a config JSON object")
	}

	for key, variable := range map[string]string{
		"host": "PERSONA_HOST", "port": "PERSONA_PORT", "dataDir": "PERSONA_DATA_DIR",
		"staticDir": "PERSONA_STATIC_DIR", "deployment": "PERSONA_DEPLOYMENT", "publicUrl": "PERSONA_PUBLIC_URL",
	} {
		if value, ok := lookup(variable); ok {
			fields[key], _ = json.Marshal(value)
		}
	}
	for _, raw := range fields {
		if bytes.Equal(raw, []byte("null")) {
			return Config{}, errors.New("hosting: config fields may not be null")
		}
	}
	if port, ok := fields["port"]; ok {
		var value string
		if len(port) > 0 && port[0] == '"' && json.Unmarshal(port, &value) == nil {
			number, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return Config{}, errors.New("hosting: invalid port")
			}
			fields["port"] = strconv.AppendInt(nil, int64(number), 10)
		}
	}
	data, err = json.Marshal(fields)
	if err != nil {
		return Config{}, errors.New("hosting: invalid config object")
	}
	cfg := Config{
		ConfigFile: name, Host: "127.0.0.1", Port: 4310, DataDir: "data",
		StaticDir: "apps/web/dist", Deployment: "native", Computer: ComputerConfig{Kind: "process"},
	}
	if err := decodeConfig(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("hosting: file %q: %w", name, err)
	}
	if cfg.Port < 1 || cfg.Port > 65535 || cfg.Host == "" || cfg.DataDir == "" || cfg.StaticDir == "" ||
		(cfg.Deployment != "native" && cfg.Deployment != "container") {
		return Config{}, errors.New("hosting: invalid host, port, dataDir, staticDir, or deployment")
	}
	if _, ok := fields["desktopCredentialFile"]; ok && cfg.DesktopCredentialFile == "" {
		return Config{}, errors.New("hosting: invalid desktopCredentialFile")
	}
	if _, ok := fields["publicUrl"]; ok && !publicOrigin(cfg.PublicURL) {
		return Config{}, errors.New("hosting: publicUrl must be an HTTPS origin (HTTP is allowed on loopback)")
	}
	if cfg.Model != nil && (cfg.Model.Provider == "" || cfg.Model.ModelID == "") {
		return Config{}, errors.New("hosting: model requires provider and modelId")
	}
	if cfg.WorkComputer != nil && cfg.WorkComputer.Kind == "process" {
		return Config{}, errors.New("hosting: workComputer must use Docker or controller")
	}
	cfg.DataDir = configPath(name, cfg.DataDir)
	cfg.StaticDir = configPath(name, cfg.StaticDir)
	cfg.DesktopCredentialFile = configPath(name, cfg.DesktopCredentialFile)
	cfg.Computer.resolvePaths(name)
	if cfg.WorkComputer != nil {
		cfg.WorkComputer.resolvePaths(name)
	}
	return cfg, nil
}

func decodeConfig(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		var invalid *configError
		if errors.As(err, &invalid) {
			return invalid
		}
		return errors.New("invalid config JSON field or type")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected a single config JSON value")
	}
	return nil
}

type configError struct{ problem string }

func (e *configError) Error() string { return e.problem }

func configPath(file, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(filepath.Dir(file), path)
}

func publicOrigin(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.ForceQuery ||
		u.RawQuery != "" || strings.Contains(value, "#") ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" ||
		(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1"))
}

var (
	configID  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	imageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:@-]+$`)
	sshHost   = regexp.MustCompile(`^[^\s-][^\s]*$`)
	sshUser   = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
)

// UnmarshalJSON rejects fields not allowed by the selected computer kind.
func (c *ComputerConfig) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return &configError{"expected a computer object"}
	}
	var kind string
	_ = json.Unmarshal(fields["kind"], &kind)
	allowed := "kind"
	value := ComputerConfig{Kind: kind}
	switch kind {
	case "process":
	case "docker":
		allowed += " id image volume network proxyImage cpus memoryMiB pids timeoutSeconds ssh"
		value.Network, value.CPUs, value.MemoryMiB, value.PIDs, value.TimeoutSeconds = "none", 2, 1024, 256, 120
	case "controller":
		allowed += " id endpoint credentialFile network timeoutSeconds"
		value.TimeoutSeconds = 120
	default:
		return &configError{"invalid computer kind"}
	}
	for field, raw := range fields {
		if field == "" || !strings.Contains(" "+allowed+" ", " "+field+" ") || bytes.Equal(raw, []byte("null")) {
			return &configError{"invalid field for computer kind"}
		}
	}
	type computerFields ComputerConfig
	if err := decodeConfig(data, (*computerFields)(&value)); err != nil {
		return &configError{err.Error()}
	}
	if kind != "process" && (!configID.MatchString(value.ID) || value.TimeoutSeconds < 1 ||
		(value.Network != "none" && value.Network != "restricted" && value.Network != "web")) {
		return &configError{"invalid computer id, timeoutSeconds, or network"}
	}
	if kind == "docker" && (!imageName.MatchString(value.Image) || !configID.MatchString(value.Volume) ||
		(fields["proxyImage"] != nil && !imageName.MatchString(value.ProxyImage)) ||
		value.CPUs <= 0 || value.MemoryMiB < 128 || value.PIDs < 16) {
		return &configError{"invalid Docker computer configuration"}
	}
	if kind == "controller" {
		u, err := url.Parse(value.Endpoint)
		if err != nil || !u.IsAbs() || value.CredentialFile == "" {
			return &configError{"invalid controller endpoint or credentialFile"}
		}
	}
	*c = value
	return nil
}

// UnmarshalJSON validates the SSH target and its optional arguments.
func (s *SSHConfig) UnmarshalJSON(data []byte) error {
	type sshFields SSHConfig
	value := sshFields{Port: 22}
	if err := decodeConfig(data, &value); err != nil {
		return &configError{err.Error()}
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	if !sshHost.MatchString(value.Host) || value.Port < 1 || value.Port > 65535 ||
		(fields["user"] != nil && !sshUser.MatchString(value.User)) ||
		value.KnownHostsFile == "" || !configID.MatchString(value.HostKeyAlias) ||
		(fields["identityFile"] != nil && value.IdentityFile == "") ||
		(fields["configFile"] != nil && value.ConfigFile == "") {
		return &configError{"invalid SSH computer configuration"}
	}
	*s = SSHConfig(value)
	return nil
}

func (c *ComputerConfig) resolvePaths(file string) {
	c.CredentialFile = configPath(file, c.CredentialFile)
	if c.SSH != nil {
		c.SSH.KnownHostsFile = configPath(file, c.SSH.KnownHostsFile)
		c.SSH.IdentityFile = configPath(file, c.SSH.IdentityFile)
		c.SSH.ConfigFile = configPath(file, c.SSH.ConfigFile)
	}
}
