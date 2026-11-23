// Package localmodels detects local model servers (Ollama, llama.cpp,
// LM Studio) and generates Pi provider configuration for them.
package localmodels

import "time"

// Kind names a supported local model server.
type Kind string

const (
	// Ollama is `ollama serve` on :11434.
	Ollama Kind = "ollama"
	// LlamaCpp is `llama-server` on :8080.
	LlamaCpp Kind = "llamacpp"
	// LMStudio is the LM Studio server on :1234.
	LMStudio Kind = "lmstudio"
)

// Model is one model exposed by a local server.
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
}

// Server is a detected local model server with its model list.
type Server struct {
	Kind    Kind    `json:"kind"`
	BaseURL string  `json:"baseUrl"`
	Models  []Model `json:"models"`
}

// defaultTimeout bounds a single server probe; local servers answer or fail fast.
const defaultTimeout = 3 * time.Second
