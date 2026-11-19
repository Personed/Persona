// Package controller runs the authenticated control service that owns the Docker
// work containers of remote Bots. Every check fails closed: a container, network or
// firewall that does not match the configuration is reported as an error and never
// started or connected to.
package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Network is the egress policy of a work container.
type Network string

// Network policies.
const (
	NetworkNone       Network = "none"       // no network interface
	NetworkRestricted Network = "restricted" // bridge filtered by the host firewall script
	NetworkWeb        Network = "web"        // internal network with an HTTP(S) egress proxy
)

var (
	identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	imageName  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:@-]+$`)
	sshHost    = regexp.MustCompile(`^[^\s-][^\s]*$`)
	sshUser    = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
)

// ValidID reports whether id is a valid computer or volume identifier.
func ValidID(id string) bool { return identifier.MatchString(id) }

// SSH reaches a remote Docker host. Paths are absolute after Parse.
type SSH struct {
	Host           string `json:"host"`
	User           string `json:"user,omitempty"`
	Port           int    `json:"port,omitempty"`
	IdentityFile   string `json:"identityFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile"`
	HostKeyAlias   string `json:"hostKeyAlias"`
	ConfigFile     string `json:"configFile,omitempty"`
}

// DockerConfig mirrors the "docker" computer of persona.config.json. Field order
// matches the TypeScript schema so Spec hashes stay equal for existing containers.
type DockerConfig struct {
	Kind           string  `json:"kind"`
	ID             string  `json:"id"`
	Image          string  `json:"image"`
	Volume         string  `json:"volume"`
	Network        Network `json:"network"`
	ProxyImage     string  `json:"proxyImage,omitempty"`
	CPUs           float64 `json:"cpus"`
	MemoryMiB      int     `json:"memoryMiB"`
	Pids           int     `json:"pids"`
	TimeoutSeconds int     `json:"timeoutSeconds"`
	SSH            *SSH    `json:"ssh,omitempty"`
}

// ParseDockerConfig decodes a docker computer, applies the schema defaults and
// rejects unknown fields and invalid values.
func ParseDockerConfig(data []byte) (DockerConfig, error) {
	config := DockerConfig{Network: NetworkNone, CPUs: 2, MemoryMiB: 1024, Pids: 256, TimeoutSeconds: 120}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return DockerConfig{}, fmt.Errorf("controller: invalid docker computer: %w", err)
	}
	return config, config.Validate()
}

// Validate checks the values the TypeScript schema checks.
func (c DockerConfig) Validate() error {
	var problems []string
	check := func(ok bool, problem string) {
		if !ok {
			problems = append(problems, problem)
		}
	}
	check(c.Kind == "docker", "kind must be docker")
	check(identifier.MatchString(c.ID), "invalid id")
	check(imageName.MatchString(c.Image), "invalid image")
	check(identifier.MatchString(c.Volume), "invalid volume")
	check(c.Network == NetworkNone || c.Network == NetworkRestricted || c.Network == NetworkWeb, "invalid network")
	check(c.ProxyImage == "" || imageName.MatchString(c.ProxyImage), "invalid proxyImage")
	check(c.Network != NetworkWeb || c.ProxyImage != "", "web network requires proxyImage")
	check(c.CPUs > 0, "cpus must be positive")
	check(c.MemoryMiB >= 128, "memoryMiB must be at least 128")
	check(c.Pids >= 16, "pids must be at least 16")
	check(c.TimeoutSeconds > 0, "timeoutSeconds must be positive")
	if s := c.SSH; s != nil {
		check(sshHost.MatchString(s.Host), "invalid ssh host")
		check(s.User == "" || sshUser.MatchString(s.User), "invalid ssh user")
		check(s.Port >= 0 && s.Port <= 65535, "invalid ssh port")
		check(s.KnownHostsFile != "", "ssh knownHostsFile required")
		check(identifier.MatchString(s.HostKeyAlias), "invalid ssh hostKeyAlias")
	}
	if len(problems) > 0 {
		return errors.New("controller: invalid docker computer: " + strings.Join(problems, ", "))
	}
	return nil
}

// Spec is the hash stored on the container label. A change to any isolation-relevant
// field makes Inspect reject the existing container until it is rebuilt. SSH is
// excluded because it only says how the host is reached.
func (c DockerConfig) Spec() string {
	c.SSH = nil
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Names are the Docker object names of one computer.
type Names struct {
	Container string
	Network   string
	// Bridge is the Linux interface the firewall script matches with persona-c+.
	Bridge string
	Proxy  string
}

// Names returns the Docker object names for c.
func (c DockerConfig) Names() Names {
	sum := sha256.Sum256([]byte(c.ID))
	name := "persona-" + c.ID
	return Names{
		Container: name,
		Network:   name,
		// Linux interface names allow 15 visible characters; "persona-c" uses 9.
		Bridge: "persona-c" + hex.EncodeToString(sum[:])[:6],
		Proxy:  name + "-egress",
	}
}
