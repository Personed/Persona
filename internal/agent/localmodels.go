package agent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/Personed/Persona/internal/localmodels"
)

// LocalModels is the model registry of the local model servers Pi reaches
// through models.json: one provider per server kind, listed until a scan
// finds it. The first use discovers every endpoint; Refresh rescans one.
type LocalModels struct {
	endpoints []localmodels.Endpoint
	mu        sync.Mutex
	loaded    bool
	servers   map[localmodels.Kind]localmodels.Server
}

// NewLocalModels returns the registry of the servers at endpoints.
func NewLocalModels(endpoints []localmodels.Endpoint) *LocalModels {
	return &LocalModels{endpoints: endpoints, servers: map[localmodels.Kind]localmodels.Server{}}
}

func (m *LocalModels) load(ctx context.Context) map[localmodels.Kind]localmodels.Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		for _, server := range localmodels.Discover(ctx, m.endpoints) {
			m.servers[server.Kind] = server
		}
		m.loaded = ctx.Err() == nil
	}
	return m.servers
}

// Available lists the detected servers' models, of one provider or of all.
func (m *LocalModels) Available(ctx context.Context, provider string) ([]Model, error) {
	servers := m.load(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	models := []Model{}
	for _, endpoint := range m.endpoints {
		server, ok := servers[endpoint.Kind]
		if !ok || provider != "" && provider != string(endpoint.Kind) {
			continue
		}
		for _, model := range server.Models {
			name := cmp.Or(model.Name, model.ID)
			models = append(models, Model{Provider: string(server.Kind), ID: model.ID, Name: name, Input: []string{"text"}})
		}
	}
	return models, nil
}

// Providers lists one provider per endpoint kind.
func (m *LocalModels) Providers(context.Context) ([]Provider, error) {
	providers := []Provider{}
	for _, endpoint := range m.endpoints {
		id := string(endpoint.Kind)
		if slices.ContainsFunc(providers, func(p Provider) bool { return p.ID == id }) {
			continue
		}
		providers = append(providers, Provider{ID: id, Name: endpoint.Kind.Name(), Dynamic: true})
	}
	return providers, nil
}

// Configured reports whether the provider's server was detected; local
// servers need no key.
func (m *LocalModels) Configured(ctx context.Context, provider string) (bool, error) {
	servers := m.load(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := servers[localmodels.Kind(provider)]
	return ok, nil
}

// Refresh rescans the provider's server. A server that does not answer is
// dropped from the catalog and reported as an error.
func (m *LocalModels) Refresh(ctx context.Context, provider string) error {
	m.load(ctx)
	kind := localmodels.Kind(provider)
	i := slices.IndexFunc(m.endpoints, func(e localmodels.Endpoint) bool { return e.Kind == kind })
	if i < 0 {
		return fmt.Errorf("agent: unknown local model provider %q", provider)
	}
	server, err := localmodels.Scan(ctx, m.endpoints[i])
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		delete(m.servers, kind)
		return err
	}
	m.servers[kind] = server
	return nil
}
