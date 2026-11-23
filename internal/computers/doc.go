// Package computers owns the work-computer catalog of the service: the
// "persona.computers" session document, per-Bot bindings, and the targets the
// Bot work and the computer.* procedures operate on. It mirrors
// packages/agent/src/computer.
package computers

import (
	"github.com/Personed/Persona/internal/agent"
)

// Config is a ComputerRecord's target, the computerSchema of runtime/config.ts.
// Kind "process" is the service's own host; "docker" a managed container;
// "controller" a remote sandbox behind an authenticated controller.
type Config struct {
	Kind           string  `json:"kind"`
	ID             string  `json:"id,omitempty"`
	Image          string  `json:"image,omitempty"`
	Volume         string  `json:"volume,omitempty"`
	Network        string  `json:"network,omitempty"`
	ProxyImage     string  `json:"proxyImage,omitempty"`
	CPUs           float64 `json:"cpus,omitempty"`
	MemoryMiB      int64   `json:"memoryMiB,omitempty"`
	Pids           int64   `json:"pids,omitempty"`
	TimeoutSeconds int64   `json:"timeoutSeconds,omitempty"`
	Endpoint       string  `json:"endpoint,omitempty"`
	CredentialFile string  `json:"credentialFile,omitempty"`
	SSH            *SSH    `json:"ssh,omitempty"`
}

// SSH is the optional bootstrap of a docker config's ssh target.
type SSH struct {
	Host           string `json:"host,omitempty"`
	Port           int    `json:"port,omitempty"`
	User           string `json:"user,omitempty"`
	IdentityFile   string `json:"identityFile,omitempty"`
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
}

// Scope restricts a computer to shared use or one Bot.
type Scope struct {
	Kind  string `json:"kind"`
	BotID int64  `json:"botId,omitempty"`
}

// Record is one catalog item.
type Record struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Config     Config `json:"config"`
	Authorized bool   `json:"authorized"`
	Scope      *Scope `json:"scope,omitempty"`
}

// Assistance is a binding's optional Jev assistance.
type Assistance struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"`
}

// Binding is one Bot's computer assignment in the catalog document.
type Binding struct {
	ComputerID   *string         `json:"computerId"`
	Revision     int64           `json:"revision"`
	RequestID    string          `json:"requestId"`
	ApprovalMode string          `json:"approvalMode,omitempty"`
	Services     map[string]bool `json:"services,omitempty"`
	Assistance   *Assistance     `json:"assistance,omitempty"`
}

// Doc is the catalog document, ComputersDoc of state.ts.
type Doc struct {
	Items                []Record           `json:"items"`
	DefaultID            *string            `json:"defaultId"`
	AttachmentComputerID *string            `json:"attachmentComputerId"`
	Bindings             map[string]Binding `json:"bindings"`
}

// ComputersDoc is the session-scoped document of state.ts.
var ComputersDoc = agent.Doc[Doc]{
	Kind: "persona.computers", Scope: "session",
	Initial: func() Doc {
		return Doc{Bindings: map[string]Binding{}}
	},
}
