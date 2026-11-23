package localmodels

import (
	"bytes"
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
		// Pi parses models.json after stripping its BOM and comments.
		if err := json.Unmarshal(stripJSONComments(content), &raw); err != nil {
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".models-*.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}

// stripJSONComments drops the UTF-8 BOM and // and /* */ comments outside
// string literals, matching how Pi reads models.json.
func stripJSONComments(content []byte) []byte {
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	out := make([]byte, 0, len(content))
	inString, escaped := false, false
	for i := 0; i < len(content); i++ {
		c := content[i]
		switch {
		case inString && escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case !inString && c == '/' && i+1 < len(content) && content[i+1] == '/':
			for i < len(content) && content[i] != '\n' {
				i++
			}
			if i == len(content) {
				return out
			}
			out = append(out, '\n')
			continue
		case !inString && c == '/' && i+1 < len(content) && content[i+1] == '*':
			i += 2
			for i+1 < len(content) && (content[i] != '*' || content[i+1] != '/') {
				i++
			}
			i++
			continue
		}
		out = append(out, c)
	}
	return out
}

// Name is the provider display name Pi shows for the server kind.
func (k Kind) Name() string { return names[k] }
