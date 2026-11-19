// Package hosting defines credential storage for hosted and self-hosted installs:
// provider keys, MCP OAuth tokens and device sessions. Values are encrypted at rest
// and never logged.
package hosting

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound reports a missing credential.
var ErrNotFound = errors.New("hosting: credential not found")

// Ref addresses one credential. Scope is "service", "provider", "mcp" or "device";
// Owner is the Bot ID or device ID when the scope has one.
type Ref struct {
	Scope string `json:"scope"`
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name"`
}

// Credential is a stored secret with its metadata. Secret is only set by Get.
type Credential struct {
	Ref       Ref               `json:"ref"`
	Secret    []byte            `json:"-"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	ExpiresAt time.Time         `json:"expiresAt,omitzero"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// Credentials is the encrypted credential store. Existing service credentials must
// survive migration from the TypeScript store.
type Credentials interface {
	Get(ctx context.Context, ref Ref) (Credential, error)
	Put(ctx context.Context, credential Credential) error
	Delete(ctx context.Context, ref Ref) error
	// List returns metadata without secrets.
	List(ctx context.Context, scope, owner string) ([]Credential, error)
}
