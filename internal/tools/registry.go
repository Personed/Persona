package tools

import (
	"context"
	"fmt"
	"sync"
)

// NewRegistry returns an empty Registry. Every registered product tool is granted to every Bot.
func NewRegistry() Registry {
	return &registry{byName: map[string]Tool{}}
}

type registry struct {
	mu     sync.RWMutex
	order  []string
	byName map[string]Tool
}

func (r *registry) Register(tool Tool) error {
	name := tool.Definition().Name
	if name == "" {
		return fmt.Errorf("tools: register: empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; ok {
		return fmt.Errorf("tools: register %q: already registered", name)
	}
	r.order = append(r.order, name)
	r.byName[name] = tool
	return nil
}

func (r *registry) List(_ context.Context, _ int64) ([]Definition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	definitions := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		definitions = append(definitions, r.byName[name].Definition())
	}
	return definitions, nil
}

// ErrDenied is returned by Call when the approver refuses a mutating call or none is supplied.
var ErrDenied = fmt.Errorf("tools: call denied")

func (r *registry) Call(ctx context.Context, call Call, approve Approver) (Result, error) {
	r.mu.RLock()
	tool, ok := r.byName[call.Name]
	r.mu.RUnlock()
	if !ok {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownTool, call.Name)
	}
	if definition := tool.Definition(); definition.Mutates {
		if approve == nil {
			return Result{}, ErrDenied
		}
		allowed, err := approve(ctx, call, definition)
		if err != nil {
			return Result{}, err
		}
		if !allowed {
			return Result{}, ErrDenied
		}
	}
	return tool.Run(ctx, call)
}
