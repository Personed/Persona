package hosting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "persona.config.json")
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestLoadConfig(t *testing.T) {
	filename := writeConfig(t, `{
		"model": {"provider":"deepseek","modelId":"deepseek-flash"},
		"dataDir": "old-data",
		"host": "127.0.0.1",
		"port": "4310",
		"desktopCredentialFile": "desktop-token",
		"computer": {"kind":"controller","id":"foo","network":"restricted","endpoint":"https://controller.test","credentialFile":"controller-token"},
		"workComputer": {"kind":"docker","id":"foo","image":"persona-workspace:1.2.0","volume":"persona-foo","ssh":{
			"host":"computer.test","knownHostsFile":"ssh/known_hosts","identityFile":"ssh/key","hostKeyAlias":"foo"
		}}
	}`)
	for name, value := range map[string]string{
		"PERSONA_CONFIG": filename, "PERSONA_HOST": "0.0.0.0", "PERSONA_PORT": "4321",
		"PERSONA_DATA_DIR": "new-data", "PERSONA_STATIC_DIR": "web",
		"PERSONA_DEPLOYMENT": "container", "PERSONA_PUBLIC_URL": "https://persona.test",
	} {
		t.Setenv(name, value)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(filename)
	if cfg.ConfigFile != filename || cfg.Host != "0.0.0.0" || cfg.Port != 4321 ||
		cfg.Deployment != "container" || cfg.PublicURL != "https://persona.test" ||
		cfg.DataDir != filepath.Join(base, "new-data") || cfg.StaticDir != filepath.Join(base, "web") {
		t.Fatalf("bad runtime overrides: %+v", cfg)
	}
	if cfg.Model.ModelID != "deepseek-flash" || cfg.DesktopCredentialFile != filepath.Join(base, "desktop-token") ||
		cfg.Computer.CredentialFile != filepath.Join(base, "controller-token") ||
		cfg.WorkComputer.Network != "none" || cfg.WorkComputer.CPUs != 2 ||
		cfg.WorkComputer.SSH.IdentityFile != filepath.Join(base, "ssh", "key") ||
		cfg.WorkComputer.SSH.KnownHostsFile != filepath.Join(base, "ssh", "known_hosts") {
		t.Fatalf("bad computer defaults, model or paths: %+v", cfg)
	}
}

func noEnv(string) (string, bool) { return "", false }

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfigFile(writeConfig(t, `{}`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 4310 || cfg.Deployment != "native" ||
		cfg.Computer.Kind != "process" || filepath.Base(cfg.DataDir) != "data" {
		t.Fatalf("bad defaults: %+v", cfg)
	}
}

func TestLoadConfigRejectsInvalidSettings(t *testing.T) {
	for name, input := range map[string]string{
		"unknown":       `{"hnost":"127.0.0.1"}`,
		"port":          `{"port":65536}`,
		"null port":     `{"port":null}`,
		"http":          `{"publicUrl":"http://example.test"}`,
		"url secret":    `{"publicUrl":"https://secret-value@example.test"}`,
		"url path":      `{"publicUrl":"https://example.test/path"}`,
		"model":         `{"model":{"provider":"sample"}}`,
		"computer kind": `{"computer":{"kind":"process","id":"other"}}`,
		"controller":    `{"computer":{"kind":"controller","id":"local","endpoint":"not-a-url"}}`,
		"work computer": `{"workComputer":{"kind":"process"}}`,
		"trailing":      `{} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfigFile(writeConfig(t, input), noEnv)
			if err == nil || strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("expected safe validation error, got %v", err)
			}
		})
	}
}
