package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is controller.json: where to listen, the credential file relative to the
// config, and the docker computer used as the template for every container.
type Config struct {
	Host           string          `json:"host"`
	Port           int             `json:"port"`
	CredentialFile string          `json:"credentialFile"`
	Computer       json.RawMessage `json:"computer"`

	docker     DockerConfig
	credential string
}

// LoadConfig reads controller.json and its credential.
func LoadConfig(filename string) (Config, error) {
	config := Config{Host: "127.0.0.1"}
	data, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, fmt.Errorf("controller: read config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("controller: invalid config: %w", err)
	}
	if config.Port < 1 || config.Port > 65535 || config.CredentialFile == "" || len(config.Computer) == 0 {
		return Config{}, errors.New("controller: config needs port, credentialFile and computer")
	}
	var kind struct{ Kind string }
	if err := json.Unmarshal(config.Computer, &kind); err != nil || kind.Kind != "docker" {
		return Config{}, errors.New("controller: computer must be a docker sandbox")
	}
	if config.docker, err = ParseDockerConfig(config.Computer); err != nil {
		return Config{}, err
	}
	path := config.CredentialFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(filename), path)
	}
	secret, err := os.ReadFile(path)
	if err != nil {
		return Config{}, errors.New("controller: credential file unreadable")
	}
	if config.credential = strings.TrimSpace(string(secret)); len(config.credential) < 32 {
		return Config{}, errors.New("controller: credential must have at least 32 characters")
	}
	return config, nil
}

// NewServer builds the control service for config. Containers of other ids reuse
// the template with their own volume.
func (c Config) NewServer(run Runner) (*Server, error) {
	driver, err := NewDocker(c.docker, run)
	if err != nil {
		return nil, err
	}
	return NewServer(driver, c.credential, func(id string) (Driver, error) {
		config := c.docker
		config.ID, config.Volume = id, "persona-"+id+"-work"
		return NewDocker(config, run)
	}), nil
}

// Run serves the controller until ctx ends, then stops the containers it used.
func Run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 2 && args[0] == "health" {
		return Health(ctx, args[1])
	}
	if len(args) != 1 {
		return errors.New("usage: persona controller <controller.json> | persona controller health <controller.json>")
	}
	config, err := LoadConfig(args[0])
	if err != nil {
		return err
	}
	service, err := config.NewServer(nil)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(config.Host, strconv.Itoa(config.Port)))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: service, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	_, _ = fmt.Fprintln(stdout, "Persona work-computer controller started.")
	select {
	case err = <-served:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	closeErr := service.Close(shutdown)
	_ = server.Close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, closeErr)
}

// Health checks a running controller through its authenticated readiness route.
func Health(ctx context.Context, filename string) error {
	config, err := LoadConfig(filename)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(config.Port))+"/health", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+config.credential)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return errors.New("controller: not reachable")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("controller: health returned %d", response.StatusCode)
	}
	return nil
}
