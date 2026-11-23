package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Personed/Persona/internal/localmodels"
)

func TestLocalModels(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"version":"0.5.1"}`))
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.1:8b"}]}`))
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	models := NewLocalModels([]localmodels.Endpoint{
		{Kind: localmodels.Ollama, BaseURL: server.URL},
		{Kind: localmodels.LMStudio, BaseURL: "http://127.0.0.1:1"},
	})
	ctx := context.Background()

	providers, err := ModelProviders(ctx, models)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 2 || providers[0].ID != "ollama" || !providers[0].Configured || providers[1].Configured {
		t.Fatalf("providers = %+v", providers)
	}
	model, ok, err := ConfiguredModel(ctx, models, &ModelRef{Provider: "ollama", ModelID: "llama3.1:8b"})
	if err != nil || !ok || model.Name != "llama3.1:8b" {
		t.Fatalf("ConfiguredModel = %+v %v %v", model, ok, err)
	}

	up.Store(false)
	if err := models.Refresh(ctx, "ollama"); err == nil {
		t.Fatal("Refresh of a stopped server succeeded")
	}
	if available, _ := models.Available(ctx, ""); len(available) != 0 {
		t.Fatalf("Available after a failed refresh = %+v", available)
	}
	up.Store(true)
	if err := models.Refresh(ctx, "ollama"); err != nil {
		t.Fatal(err)
	}
	if available, _ := models.Available(ctx, "ollama"); len(available) != 1 {
		t.Fatalf("Available after refresh = %+v", available)
	}
}
