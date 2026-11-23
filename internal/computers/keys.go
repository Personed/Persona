package computers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func randRead(b []byte) (int, error) { return rand.Read(b) }

// Keyring stores Jev provider keys, persona-computer:<provider> credentials of
// assistance.ts, as a small file under the data directory.
type Keyring struct{ path string }

// NewKeyring returns the provider-key store for the service.
func NewKeyring(dataDir string) *Keyring {
	return &Keyring{path: filepath.Join(dataDir, "computer-keys.json")}
}

func (k *Keyring) load() (map[string]string, error) {
	raw, err := os.ReadFile(k.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	return out, json.Unmarshal(raw, &out)
}

func (k *Keyring) save(provider, key string) error {
	keys, err := k.load()
	if err != nil {
		return err
	}
	if key == "" {
		delete(keys, provider)
	} else {
		keys[provider] = key
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return os.WriteFile(k.path, data, 0o600)
}

func (c *Catalog) assistKey(provider string) string {
	if c.keyring == nil {
		return ""
	}
	keys, err := c.keyring.load()
	if err != nil {
		return ""
	}
	return keys[provider]
}

func (c *Catalog) saveKey(provider, key string) error {
	return c.keyring.save(provider, key)
}

// askJevTest runs the connectivity probe of assistance.ts test(): a Jev
// decision call that must answer "ready".
func askJevTest(ctx context.Context, provider, key string) (AssistantTestResult, error) {
	var endpoint, auth string
	switch provider {
	case "typesafe":
		endpoint = "https://api.typesafe.ai/v1/jev/decide"
		auth = "Bearer " + key
	case "openrouter":
		endpoint = "https://openrouter.ai/api/v1/chat/completions"
		auth = "Bearer " + key
	default:
		return AssistantTestResult{}, errors.New("辅助来源无效。")
	}
	started := time.Now()
	body, _ := json.Marshal(map[string]any{
		"value": "ready",
		"connection": map[string]any{
			"type":         "choice",
			"instructions": "Select the value in state.",
			"criteria":     map[string]string{"ready": "ready", "other": "other"},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(string(body)))
	if err != nil {
		return AssistantTestResult{}, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return AssistantTestResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode/100 != 2 {
		return AssistantTestResult{}, fmt.Errorf("辅助决策连接测试未通过。HTTP %d", response.StatusCode)
	}
	var result struct {
		Model   string `json:"model"`
		Answers struct {
			Connection *struct {
				Type   string `json:"type"`
				Choice string `json:"choice"`
			} `json:"connection"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return AssistantTestResult{}, err
	}
	if result.Answers.Connection == nil ||
		result.Answers.Connection.Type != "choice" ||
		result.Answers.Connection.Choice != "ready" {
		return AssistantTestResult{}, errors.New("辅助决策连接测试未通过。")
	}
	return AssistantTestResult{
		Model:        result.Model,
		Milliseconds: time.Since(started).Milliseconds(),
	}, nil
}
