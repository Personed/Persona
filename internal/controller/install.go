package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Release mirrors deploy/release.json: the work and egress images with their
// build instructions. The entry fields point at TypeScript sources and stay
// required until the TypeScript backend is removed.
type Release struct {
	Version         string `json:"version"`
	ServiceImage    string `json:"serviceImage,omitempty"`
	ServiceBuild    *Build `json:"serviceBuild,omitempty"`
	WorkImage       string `json:"workImage"`
	ProxyImage      string `json:"proxyImage,omitempty"`
	ProxyBuild      *Build `json:"proxyBuild,omitempty"`
	ControllerEntry string `json:"controllerEntry"`
	ServerEntry     string `json:"serverEntry"`
	BridgeEntry     string `json:"bridgeEntry"`
	WorkerEntry     string `json:"workerEntry,omitempty"`
	StaticDir       string `json:"staticDir"`
	WorkBuild       *Build `json:"workBuild,omitempty"`
}

// Build names a Dockerfile and context for an image that may be built locally.
type Build struct {
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile"`
}

// Install prepares a hosted container deployment in dir, as
// apps/controller/src/install.ts does: it builds or pulls the work images from
// deploy/release.json (rooted at source, default the working directory), then
// writes controller.json, the controller credential and persona.config.json
// with the container overrides. A deployment that already has
// persona.config.json is left untouched.
func Install(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: persona controller install <dir> [source]")
	}
	dir := args[0]
	source := "."
	if len(args) == 2 {
		source = args[1]
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	configFile := filepath.Join(dir, "persona.config.json")
	if _, err := os.ReadFile(configFile); err == nil {
		_, _ = fmt.Fprintln(stdout, "托管配置已存在。")
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(filepath.Join(source, "deploy", "release.json"))
	if err != nil {
		return fmt.Errorf("controller install: read release: %w", err)
	}
	var release Release
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&release); err != nil {
		return fmt.Errorf("controller install: invalid release: %w", err)
	}
	if release.WorkImage == "" || release.ControllerEntry == "" || release.ServerEntry == "" || release.BridgeEntry == "" {
		return errors.New("controller install: release needs workImage, controllerEntry, serverEntry and bridgeEntry")
	}
	if err := prepareImages(ctx, release, source, stdout); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "正在准备配置…")
	return writeContainerConfiguration(dir, release, stdout)
}

// prepareImages checks Docker runs Linux containers, then builds or pulls each
// image the deployment needs, like packages/hosting's prepareWorkImages.
func prepareImages(ctx context.Context, release Release, source string, stdout io.Writer) error {
	_, _ = fmt.Fprintln(stdout, "Verificando o Docker e permissões de diretório…")
	system, err := runInstallCommand(ctx, []string{"docker", "info", "--format", "{{json .OSType}}"}, 30*time.Second)
	if err != nil {
		return errors.New("não foi possível conectar ao Docker; instale e inicie o Docker Desktop e tente novamente")
	}
	if strings.TrimSpace(system) != `"linux"` {
		return errors.New("alterne o Docker para o modo de contêineres Linux")
	}
	_, _ = fmt.Fprintln(stdout, "Preparando imagens de trabalho Linux…")
	images := []struct {
		image string
		build *Build
	}{
		{release.WorkImage, release.WorkBuild},
	}
	if release.ProxyImage != "" {
		images = append(images, struct {
			image string
			build *Build
		}{release.ProxyImage, release.ProxyBuild})
	}
	for _, item := range images {
		if _, err := runInstallCommand(ctx, []string{"docker", "image", "inspect", item.image}, 30*time.Second); err == nil {
			continue
		}
		if item.build != nil {
			contextDir := item.build.Context
			if !filepath.IsAbs(contextDir) {
				contextDir = filepath.Join(source, contextDir)
			}
			if _, err := runInstallCommand(ctx, []string{
				"docker", "build", "-f", filepath.Join(contextDir, item.build.Dockerfile),
				"-t", item.image, contextDir,
			}, 20*time.Minute); err != nil {
				return err
			}
		} else if _, err := runInstallCommand(ctx, []string{"docker", "pull", item.image}, 20*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// runInstallCommand is the shell runner installs use; tests replace it.
var runInstallCommand = installCommand

// installCommand runs one install step with the diagnostics of hosting's
// runCommand: trimmed stdout, stderr only in the error cause.
func installCommand(ctx context.Context, args []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, program(args[0]), args[1:]...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("a etapa de instalação não foi concluída; verifique a rede e o ambiente e tente novamente: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// writeContainerConfiguration publishes controller.json, the credential and
// persona.config.json with the same values the TypeScript installer produces:
// both services listen on every interface, the app on 4310 and the controller
// on 4311, and the app reaches the controller over the internal network.
func writeContainerConfiguration(dir string, release Release, stdout io.Writer) error {
	credentialFile := filepath.Join(dir, "controller-token")
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	file, err := os.OpenFile(credentialFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err == nil {
		if err := writeAndClose(file, base64.RawURLEncoding.EncodeToString(secret)); err != nil {
			return err
		}
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	id := hex.EncodeToString(suffix)
	network := NetworkNone
	if release.ProxyImage != "" {
		network = NetworkWeb
	}
	computer := DockerConfig{
		Kind:           "docker",
		ID:             id,
		Image:          release.WorkImage,
		Volume:         "persona-" + id + "-work",
		Network:        network,
		ProxyImage:     release.ProxyImage,
		CPUs:           2,
		MemoryMiB:      1024,
		Pids:           256,
		TimeoutSeconds: 120,
	}
	if err := computer.Validate(); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "controller.json"), Config{
		Host:           "0.0.0.0",
		Port:           4311,
		CredentialFile: credentialFile,
		Computer:       mustMarshal(computer),
	}); err != nil {
		return err
	}
	// The app reaches the controller over the compose network; its own computer
	// spec carries the shared credential, mirrored into workComputer.
	appComputer := map[string]any{
		"kind":           "controller",
		"id":             id,
		"endpoint":       "http://controller:4311",
		"credentialFile": credentialFile,
		"network":        string(network),
	}
	if err := writeJSON(configFile(dir), map[string]any{
		"host":         "0.0.0.0",
		"port":         4310,
		"dataDir":      "/data",
		"staticDir":    "/app/apps/web/dist",
		"deployment":   "container",
		"computer":     appComputer,
		"workComputer": appComputer,
	}); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := os.Chown(filepath.Join(dir, entry.Name()), 1000, 1000); err != nil {
				return err
			}
		}
		if err := os.Chown(dir, 1000, 1000); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintln(stdout, "Instalação concluída.")
	return nil
}

func configFile(dir string) string { return filepath.Join(dir, "persona.config.json") }

func mustMarshal(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

// writeJSON publishes value atomically and mode 0600, as hosting's
// writeConfiguration does.
func writeJSON(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary := filename + ".pending"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filename)
}

func writeAndClose(file *os.File, content string) (err error) {
	_, writeErr := file.WriteString(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}
