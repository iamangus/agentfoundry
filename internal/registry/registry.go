package registry

import (
	"log/slog"
	"sync"

	"github.com/angoo/agentfile/internal/config"
)

// Registry stores agent definitions.
// Tool discovery is handled by the MCP client pool; the registry
// only stores agents.
type Registry struct {
	mu        sync.RWMutex
	agentDefs map[string]*config.Definition // name -> agent definition
}

// New creates a new empty registry.
func New() *Registry {
	return &Registry{
		agentDefs: make(map[string]*config.Definition),
	}
}

// RegisterAgent stores an agent definition.
func (r *Registry) RegisterAgent(def *config.Definition) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if def.MaxTurns == 0 {
		def.MaxTurns = 10
	}

	r.agentDefs[def.Name] = def
	slog.Info("agent registered", "name", def.Name, "tools", def.Tools)
	return nil
}

// GetAgentDef returns an agent definition by name.
func (r *Registry) GetAgentDef(name string) (*config.Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.agentDefs[name]
	return def, ok
}

// ListAgentDefs returns all registered agent definitions.
func (r *Registry) ListAgentDefs() []*config.Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]*config.Definition, 0, len(r.agentDefs))
	for _, def := range r.agentDefs {
		defs = append(defs, def)
	}
	return defs
}

// ListAgentNames returns all registered agent names.
func (r *Registry) ListAgentNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.agentDefs))
	for name := range r.agentDefs {
		names = append(names, name)
	}
	return names
}

// Remove removes an agent by name.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.agentDefs, name)
}
