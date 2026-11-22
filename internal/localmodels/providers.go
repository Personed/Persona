package localmodels

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Pi stores provider configuration in <agent dir>/models.json as
// {"providers": {"<id>": <ProviderConfig>}}. Local servers have no key of
// their own; any non-empty apiKey marks the provider configured.
const defaultAPIKey = "local"

const (
	defaultContextWindow = 32768
	defaultMaxTokens     = 8192
)

// ProviderConfig is one models.json provider entry (Pi's schema).
type ProviderConfig struct {
	Name    string        `json:"name,omitempty"`
	BaseURL string        `json:"baseUrl,omitempty"`
	APIKey  string        `json:"apiKey,omitempty"`
	API     string        `json:"api,omitempty"`
	Models  []ModelConfig `json:"models,omitempty"`
}

// ModelConfig is one model inside a ProviderConfig.
type ModelConfig struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	API           string   `json:"api,omitempty"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input,omitempty"`
	ContextWindow int      `json:"contextWindow"`
	MaxTokens     int      `json:"maxTokens"`
	Cost          Cost     `json:"cost"`
}

// Cost is the per-token price table Pi expects; local inference is free.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

var names = map[Kind]string{
	Ollama:   "Ollama",
	LlamaCpp: "llama.cpp",
	LMStudio: "LM Studio",
}

// ProviderConfig builds the models.json entry for a detected server. The map
// key is the provider id, equal to the server kind.
func (s Server) ProviderConfig() ProviderConfig {
	models := make([]ModelConfig, len(s.Models))
	for i, model := range s.Models {
		contextWindow := model.ContextWindow
		if contextWindow <= 0 {
			contextWindow = defaultContextWindow
		}
		models[i] = ModelConfig{
			ID:            model.ID,
			Name:          model.Name,
			API:           "openai-completions",
			Reasoning:     false,
			Input:         []string{"text"},
			ContextWindow: contextWindow,
			MaxTokens:     defaultMaxTokens,
		}
	}
	return ProviderConfig{
		Name:    names[s.Kind],
		BaseURL: s.BaseURL + "/v1",
		APIKey:  defaultAPIKey,
		API:     "openai-completions",
		Models:  models,
	}
}

// SyncProviders merges the detected servers into the models.json at path.
// Providers of other kinds are kept, and a re-detected provider keeps its
// stored apiKey. It reports whether the file changed.
func SyncProviders(path string, servers []Server) (bool, error) {
	raw := map[string]json.RawMessage{}
	if content, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(content, &raw); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	providers := map[string]json.RawMessage{}
	if existing, ok := raw["providers"]; ok {
		if err := json.Unmarshal(existing, &providers); err != nil {
			return false, fmt.Errorf("parse providers in %s: %w", path, err)
		}
	}
	for _, server := range servers {
		id := string(server.Kind)
		config := server.ProviderConfig()
		if existing, ok := providers[id]; ok {
			var prior struct {
				APIKey string `json:"apiKey"`
			}
			if json.Unmarshal(existing, &prior) == nil && prior.APIKey != "" {
				config.APIKey = prior.APIKey
			}
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			return false, err
		}
		providers[id] = encoded
	}
	encodedProviders, err := json.Marshal(providers)
	if err != nil {
		return false, err
	}
	raw["providers"] = encodedProviders
	content, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return false, err
	}
	content = append(content, '\n')
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(content) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}
