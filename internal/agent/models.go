package agent

import "context"

// Model is one model of a provider's catalog. ThinkingLevelMap maps a
// thinking level to the provider's value; a present nil entry disables the level.
type Model struct {
	Provider         string
	ID               string
	Name             string
	Reasoning        bool
	ThinkingLevelMap map[string]*string
	Input            []string
}

// AuthMethod is one way to sign in to a provider.
type AuthMethod struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Provider is a model provider. Methods lists its API key login (when it
// has one) and OAuth login, in that order.
type Provider struct {
	ID      string
	Name    string
	Methods []AuthMethod
	Dynamic bool
}

// Models is the model registry, pi-ai's Models.
type Models interface {
	// Available lists the models usable now, of one provider or, for "", of all.
	Available(ctx context.Context, provider string) ([]Model, error)
	Providers(ctx context.Context) ([]Provider, error)
	Configured(ctx context.Context, provider string) (bool, error)
	// Refresh reloads one provider's catalog from the network.
	Refresh(ctx context.Context, provider string) error
}

var extendedThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// ThinkingLevels is getSupportedThinkingLevels of pi-ai.
func ThinkingLevels(m Model) []string {
	if !m.Reasoning {
		return []string{"off"}
	}
	var levels []string
	for _, level := range extendedThinkingLevels {
		mapped, ok := m.ThinkingLevelMap[level]
		if ok && mapped == nil {
			continue
		}
		if (level == "xhigh" || level == "max") && !ok {
			continue
		}
		levels = append(levels, level)
	}
	return levels
}

// Option is one choice of a model control.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Control is a model control, thinkingControl in models.ts.
type Control struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Label   string   `json:"label"`
	Value   string   `json:"value"`
	Options []Option `json:"options"`
}

// CatalogModel is one item of models.list.
type CatalogModel struct {
	Provider     string  `json:"provider"`
	ModelID      string  `json:"modelId"`
	Name         string  `json:"name"`
	ProviderName string  `json:"providerName"`
	Effort       Control `json:"effort"`
}

// ProviderStatus is one item of models.providers.
type ProviderStatus struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Methods    []AuthMethod `json:"methods"`
	Configured bool         `json:"configured"`
	Dynamic    bool         `json:"dynamic"`
}

func thinkingControl(m Model) Control {
	levels := ThinkingLevels(m)
	options := make([]Option, len(levels))
	for i, level := range levels {
		options[i] = Option{ID: level, Label: level}
	}
	return Control{ID: "thinkingLevel", Kind: "effort", Label: "推理强度", Options: options}
}

// ModelCatalog lists the available models, modelCatalog in models.ts. Nil models list none.
func ModelCatalog(ctx context.Context, models Models) ([]CatalogModel, error) {
	out := []CatalogModel{}
	if models == nil {
		return out, nil
	}
	available, err := models.Available(ctx, "")
	if err != nil {
		return nil, err
	}
	providers, err := models.Providers(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, p := range providers {
		names[p.ID] = p.Name
	}
	for _, m := range available {
		name, ok := names[m.Provider]
		if !ok {
			name = m.Provider
		}
		out = append(out, CatalogModel{Provider: m.Provider, ModelID: m.ID, Name: m.Name, ProviderName: name, Effort: thinkingControl(m)})
	}
	return out, nil
}

// ModelProviders lists the providers and whether each is configured, modelProviders in models.ts.
func ModelProviders(ctx context.Context, models Models) ([]ProviderStatus, error) {
	out := []ProviderStatus{}
	if models == nil {
		return out, nil
	}
	providers, err := models.Providers(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		configured, err := models.Configured(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		methods := p.Methods
		if methods == nil {
			methods = []AuthMethod{}
		}
		out = append(out, ProviderStatus{ID: p.ID, Name: p.Name, Methods: methods, Configured: configured, Dynamic: p.Dynamic})
	}
	return out, nil
}

// ConfiguredModel finds the selected model among the available ones. It reports
// false when the selection is nil or not available, as requireConfiguredModel.
func ConfiguredModel(ctx context.Context, models Models, selection *ModelRef) (Model, bool, error) {
	if models == nil || selection == nil {
		return Model{}, false, nil
	}
	available, err := models.Available(ctx, selection.Provider)
	if err != nil {
		return Model{}, false, err
	}
	for _, m := range available {
		if m.ID == selection.ModelID {
			return m, true, nil
		}
	}
	return Model{}, false, nil
}

// Configure applies change to the Bot's pi.agent document, configure of pi-durable.
func (a *Agent) Configure(ctx context.Context, bot int64, change Change) error {
	return a.Harness.Commit(ctx, func(tx *Tx) error {
		config, err := Edit(tx, AgentDoc, bot)
		if err != nil {
			return err
		}
		config.apply(change)
		return nil
	})
}
