package client

// Engine is the engines.get output: a Bot's Harness configuration and the models it offers.
type Engine struct {
	Config     EngineConfig `json:"config"`
	Revision   int          `json:"revision"`
	Busy       bool         `json:"busy"`
	Catalog    []Harness    `json:"catalog"`
	Connection *struct {
		Ready  bool          `json:"ready"`
		Models []EngineModel `json:"models"`
	} `json:"connection"`
}

// EngineConfig is the saved Harness, model and model options of a Bot.
type EngineConfig struct {
	Provider string            `json:"provider"`
	Model    *string           `json:"model"`
	Options  map[string]string `json:"options"`
}

// Harness is one native Harness and the models it reports.
type Harness struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Models []EngineModel `json:"models"`
}

// EngineModel is a model of a Harness; Effort is its reasoning control, if any.
type EngineModel struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Effort *Effort `json:"effort"`
}

// Effort is a model's reasoning-effort option: the option key, current value and choices.
type Effort struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Value   string   `json:"value"`
	Options []Option `json:"options"`
}

// Models returns the models of the configured Harness, preferring the live connection's list.
func (e Engine) Models() []EngineModel {
	if e.Connection != nil && len(e.Connection.Models) > 0 {
		return e.Connection.Models
	}
	for _, h := range e.Catalog {
		if h.ID == e.Config.Provider {
			return h.Models
		}
	}
	return nil
}
