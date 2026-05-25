package registry

import (
	"log/slog"
	"sync"

	"github.com/angoo/agentfoundry/internal/config"
)

type Registry struct {
	mu        sync.RWMutex
	agentDefs map[string]*config.Definition // name -> agent definition
	byID      map[string]*config.Definition // agent_id -> agent definition
}

func New() *Registry {
	return &Registry{
		agentDefs: make(map[string]*config.Definition),
		byID:      make(map[string]*config.Definition),
	}
}

func (r *Registry) RegisterAgent(def *config.Definition) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if def.MaxTurns == 0 {
		def.MaxTurns = 10
	}

	r.agentDefs[def.Name] = def
	if def.AgentID != "" {
		r.byID[def.AgentID] = def
	}
	slog.Info("agent registered", "name", def.Name, "agent_id", def.AgentID, "tools", def.Tools)
	return nil
}

func (r *Registry) GetAgentDef(name string) (*config.Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.agentDefs[name]
	return def, ok
}

func (r *Registry) GetAgentByID(agentID string) (*config.Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.byID[agentID]
	return def, ok
}

func (r *Registry) ListAgentDefs() []*config.Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]*config.Definition, 0, len(r.agentDefs))
	for _, def := range r.agentDefs {
		defs = append(defs, def)
	}
	return defs
}

func (r *Registry) ListAgentNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.agentDefs))
	for name := range r.agentDefs {
		names = append(names, name)
	}
	return names
}

func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if def, ok := r.agentDefs[name]; ok {
		if def.AgentID != "" {
			delete(r.byID, def.AgentID)
		}
	}
	delete(r.agentDefs, name)
}
