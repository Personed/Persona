package localmodels

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.5.1"}`))
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.1:8b"},{"name":"qwen2.5:14b"}]}`))
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Name == "llama3.1:8b" {
			_, _ = w.Write([]byte(`{"model_info":{"general.architecture":"llama","llama.context_length":131072}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model_info":{}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestDiscoverOllama(t *testing.T) {
	servers := Discover(context.Background(), []Endpoint{{Kind: Ollama, BaseURL: fakeOllama(t).URL}})
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	server := servers[0]
	if server.Kind != Ollama || len(server.Models) != 2 {
		t.Fatalf("unexpected server: %+v", server)
	}
	if server.Models[0].ID != "llama3.1:8b" || server.Models[0].ContextWindow != 131072 {
		t.Fatalf("unexpected model: %+v", server.Models[0])
	}
	if server.Models[1].ContextWindow != 0 {
		t.Fatalf("expected unknown context window, got %d", server.Models[1].ContextWindow)
	}
}

func TestDiscoverLlamaCppAndLMStudio(t *testing.T) {
	llama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"local.gguf"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(llama.Close)
	studio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen-7b"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(studio.Close)

	servers := Discover(context.Background(), []Endpoint{
		{Kind: Ollama, BaseURL: "http://127.0.0.1:1"},
		{Kind: LlamaCpp, BaseURL: llama.URL},
		{Kind: LMStudio, BaseURL: studio.URL},
	})
	if len(servers) != 2 {
		t.Fatalf("expected 2 servers (unreachable ollama skipped), got %+v", servers)
	}
	if servers[0].Kind != LlamaCpp || servers[0].Models[0].ID != "local.gguf" {
		t.Fatalf("unexpected llamacpp server: %+v", servers[0])
	}
	if servers[1].Kind != LMStudio || servers[1].Models[0].ID != "qwen-7b" {
		t.Fatalf("unexpected lmstudio server: %+v", servers[1])
	}
}

func TestDefaultEndpointsOllamaHost(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "localhost")
	if got := DefaultEndpoints()[0].BaseURL; got != "http://localhost:11434" {
		t.Fatalf("expected default port, got %s", got)
	}
	t.Setenv("OLLAMA_HOST", "host.docker.internal:9999")
	if got := DefaultEndpoints()[0].BaseURL; got != "http://host.docker.internal:9999" {
		t.Fatalf("expected explicit port kept, got %s", got)
	}
	t.Setenv("OLLAMA_HOST", "https://models.example.com")
	if got := DefaultEndpoints()[0].BaseURL; got != "https://models.example.com" {
		t.Fatalf("expected scheme default port, got %s", got)
	}
}

func TestSyncProviders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	existing := "\xef\xbb\xbf" + `{"providers":{ // apiKey per provider
"anthropic":{"apiKey":"sk-ant"},"ollama":{"apiKey":"my-key","baseUrl":"http://a//b",
"models":[{"id":"llama3.1:8b","contextWindow":4096,"maxTokens":1024}]}}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	servers := []Server{
		{Kind: Ollama, BaseURL: "http://127.0.0.1:11434", Models: []Model{{ID: "llama3.1:8b", ContextWindow: 131072}, {ID: "qwen3:4b"}}},
		{Kind: LMStudio, BaseURL: "http://127.0.0.1:1234", Models: []Model{{ID: "qwen-7b"}}},
	}
	changed, err := SyncProviders(path, servers)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected file to change")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Providers map[string]ProviderConfig `json:"providers"`
	}
	if err := json.Unmarshal(content, &file); err != nil {
		t.Fatal(err)
	}
	if file.Providers["anthropic"].APIKey != "sk-ant" {
		t.Fatal("unrelated provider lost")
	}
	ollama := file.Providers["ollama"]
	if ollama.APIKey != "my-key" {
		t.Fatalf("stored apiKey not preserved: %q", ollama.APIKey)
	}
	if ollama.BaseURL != "http://a//b" || ollama.API != "openai-completions" || len(ollama.Models) != 2 {
		t.Fatalf("unexpected ollama provider: %+v", ollama)
	}
	if custom := ollama.Models[0]; custom.ContextWindow != 4096 || custom.MaxTokens != 1024 || custom.API != "" {
		t.Fatalf("custom ollama model changed: %+v", custom)
	}
	if added := ollama.Models[1]; added.ID != "qwen3:4b" || added.API != "openai-completions" {
		t.Fatalf("unexpected added ollama model: %+v", added)
	}
	studio := file.Providers["lmstudio"]
	if studio.Models[0].ContextWindow != defaultContextWindow {
		t.Fatalf("expected default context window, got %d", studio.Models[0].ContextWindow)
	}
	changed, err = SyncProviders(path, servers)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second sync should be a no-op")
	}
}
