package computers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Personed/Persona/internal/hosting"
)

// Keyring stores Jev provider keys as persona-computer:<provider> credentials
// of assistance.ts, in the hosting credential store so existing keys survive
// migration and secrets stay encrypted.
type Keyring struct{ store hosting.Credentials }

// NewKeyring returns the provider-key store for the service, or nil when the
// deployment has no credential store (assistance.ts available: Boolean(credentials)).
func NewKeyring(store hosting.Credentials) *Keyring {
	if store == nil {
		return nil
	}
	return &Keyring{store: store}
}

func keyRef(provider string) hosting.Ref {
	return hosting.Ref{Scope: "provider", Name: "persona-computer:" + provider}
}

func (c *Catalog) assistKey(ctx context.Context, provider string) string {
	if c.keyring == nil {
		return ""
	}
	credential, err := c.keyring.store.Get(ctx, keyRef(provider))
	if err != nil {
		return ""
	}
	var secret struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if json.Unmarshal(credential.Secret, &secret) != nil || secret.Type != "api_key" {
		return ""
	}
	return secret.Key
}

func (c *Catalog) saveKey(ctx context.Context, provider, key string) error {
	ref := keyRef(provider)
	if key == "" {
		err := c.keyring.store.Delete(ctx, ref)
		if errors.Is(err, hosting.ErrNotFound) {
			return nil
		}
		return err
	}
	secret, _ := json.Marshal(map[string]string{"type": "api_key", "key": key})
	return c.keyring.store.Put(ctx, hosting.Credential{Ref: ref, Secret: secret})
}

// jevProviders are the decision endpoints of jev.ts.
var jevProviders = map[string]struct{ URL, Model string }{
	"typesafe":   {"https://api.typesafe.ai/v1/systemone", "jev-1.14.0"},
	"openrouter": {"https://openrouter.ai/api/v1/systemone", "typesafe/jev-1.14"},
}

// askJevTest runs the connectivity probe of assistance.ts test(): a Jev
// decision call that must answer "ready".
func askJevTest(ctx context.Context, provider, key string) (AssistantTestResult, error) {
	selected, ok := jevProviders[provider]
	if !ok {
		return AssistantTestResult{}, errors.New("辅助来源无效。")
	}
	started := time.Now()
	body, _ := json.Marshal(map[string]any{
		"model": selected.Model,
		"state": map[string]any{"value": "ready"},
		"questions": map[string]any{
			"connection": map[string]any{
				"type":         "choice",
				"instructions": "Select the value in state.",
				"criteria":     map[string]string{"ready": "ready", "other": "other"},
			},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, selected.URL,
		strings.NewReader(string(body)))
	if err != nil {
		return AssistantTestResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return AssistantTestResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode/100 != 2 {
		return AssistantTestResult{}, fmt.Errorf("辅助决策请求失败（%s，HTTP %d）。", provider, response.StatusCode)
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
	if err := json.NewDecoder(http.MaxBytesReader(nil, response.Body, 64*1024)).Decode(&result); err != nil {
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
