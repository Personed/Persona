package localmodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

// Endpoint is a probe target: a server kind at a base URL.
type Endpoint struct {
	Kind    Kind
	BaseURL string
}

// DefaultEndpoints lists the well-known loopback endpoints for each server
// kind. OLLAMA_HOST overrides the Ollama endpoint, matching the Ollama CLI.
func DefaultEndpoints() []Endpoint {
	ollama := "http://127.0.0.1:11434"
	if host := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); host != "" {
		if !strings.Contains(host, "://") {
			host = "http://" + host
		}
		// The Ollama CLI defaults to port 11434 when OLLAMA_HOST omits it.
		if parsed, err := url.Parse(host); err == nil && parsed.Port() == "" {
			parsed.Host = net.JoinHostPort(parsed.Hostname(), "11434")
			host = parsed.String()
		}
		ollama = host
	}
	return []Endpoint{
		{Kind: Ollama, BaseURL: ollama},
		{Kind: LlamaCpp, BaseURL: "http://127.0.0.1:8080"},
		{Kind: LMStudio, BaseURL: "http://127.0.0.1:1234"},
	}
}

// Discover probes every endpoint concurrently and returns the servers that
// answered with a model list, in endpoint order.
func Discover(ctx context.Context, endpoints []Endpoint) []Server {
	servers := make([]Server, len(endpoints))
	var wg sync.WaitGroup
	for i, endpoint := range endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server, err := Scan(ctx, endpoint)
			if err == nil {
				servers[i] = server
			}
		}()
	}
	wg.Wait()
	found := servers[:0]
	for _, server := range servers {
		if server.Kind != "" {
			found = append(found, server)
		}
	}
	return found
}

// Scan lists the models of one server. It returns an error when the server is
// unreachable or does not answer like the expected kind.
func Scan(ctx context.Context, endpoint Endpoint) (Server, error) {
	switch endpoint.Kind {
	case Ollama:
		return scanOllama(ctx, endpoint.BaseURL)
	case LlamaCpp:
		return scanOpenAI(ctx, endpoint, "/health")
	case LMStudio:
		return scanOpenAI(ctx, endpoint, "")
	default:
		return Server{}, fmt.Errorf("unknown local model server kind %q", endpoint.Kind)
	}
}

// Health re-probes the server's model list; an error means it is down.
func Health(ctx context.Context, endpoint Endpoint) error {
	_, err := Scan(ctx, endpoint)
	return err
}

func get(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func post(ctx context.Context, url string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func base(url string) string {
	return strings.TrimRight(url, "/")
}

// scanOllama checks /api/version, then lists /api/tags and asks /api/show for
// each model's trained context length.
func scanOllama(ctx context.Context, baseURL string) (Server, error) {
	var version struct {
		Version string `json:"version"`
	}
	if err := get(ctx, base(baseURL)+"/api/version", &version); err != nil {
		return Server{}, err
	}
	if version.Version == "" {
		return Server{}, errors.New("ollama: empty version")
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := get(ctx, base(baseURL)+"/api/tags", &tags); err != nil {
		return Server{}, err
	}
	models := make([]Model, len(tags.Models))
	var wg sync.WaitGroup
	for i, entry := range tags.Models {
		models[i] = Model{ID: entry.Name, Name: entry.Name}
		wg.Add(1)
		go func() {
			defer wg.Done()
			models[i].ContextWindow = ollamaContextWindow(ctx, baseURL, entry.Name)
		}()
	}
	wg.Wait()
	return Server{Kind: Ollama, BaseURL: base(baseURL), Models: models}, nil
}

// ollamaContextWindow reads the model_info context length (e.g.
// "llama.context_length"). It returns 0 when unknown.
func ollamaContextWindow(ctx context.Context, baseURL, name string) int {
	var show struct {
		Info map[string]any `json:"model_info"`
	}
	if err := post(ctx, base(baseURL)+"/api/show", map[string]string{"name": name}, &show); err != nil {
		return 0
	}
	for key, value := range show.Info {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		if number, ok := value.(float64); ok && number > 0 {
			return int(number)
		}
	}
	return 0
}

// scanOpenAI lists models through the OpenAI-compatible /v1/models. A non-empty
// pingPath is checked first (llama.cpp serves /health; LM Studio answers
// /v1/models itself).
func scanOpenAI(ctx context.Context, endpoint Endpoint, pingPath string) (Server, error) {
	if pingPath != "" {
		if err := get(ctx, base(endpoint.BaseURL)+pingPath, &struct{}{}); err != nil {
			return Server{}, err
		}
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := get(ctx, base(endpoint.BaseURL)+"/v1/models", &list); err != nil {
		return Server{}, err
	}
	models := make([]Model, len(list.Data))
	for i, entry := range list.Data {
		models[i] = Model{ID: entry.ID, Name: entry.ID}
	}
	return Server{Kind: endpoint.Kind, BaseURL: base(endpoint.BaseURL), Models: models}, nil
}
