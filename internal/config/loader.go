package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// AgentRegistrar is the interface for registering agent definitions.
type AgentRegistrar interface {
	RegisterAgent(def *Definition) error
}

// Loader loads agent definitions from the filesystem and watches for changes.
type Loader struct {
	dir         string
	agentReg    AgentRegistrar
	watcher     *fsnotify.Watcher
	mu          sync.Mutex
	definitions map[string]*Definition // name -> definition
}

// NewLoader creates a new definition loader.
func NewLoader(dir string, agentReg AgentRegistrar) *Loader {
	return &Loader{
		dir:         dir,
		agentReg:    agentReg,
		definitions: make(map[string]*Definition),
	}
}

// LoadAll loads all YAML agent definitions from the definitions directory.
func (l *Loader) LoadAll() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Ensure directory exists
	if err := os.MkdirAll(l.dir, 0755); err != nil {
		return fmt.Errorf("create definitions dir: %w", err)
	}

	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("read definitions dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if err := l.loadFile(filepath.Join(l.dir, entry.Name())); err != nil {
			slog.Error("failed to load definition", "file", entry.Name(), "error", err)
			continue
		}
	}

	slog.Info("agent definitions loaded", "count", len(l.definitions))
	return nil
}

// loadFile parses a single YAML file and registers the definition.
func (l *Loader) loadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file %s: %w", path, err)
	}

	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	if err := def.Validate(); err != nil {
		return fmt.Errorf("validate %s: %w", path, err)
	}

	if err := l.agentReg.RegisterAgent(&def); err != nil {
		return err
	}
	l.definitions[def.Name] = &def
	slog.Info("registered agent", "name", def.Name)
	return nil
}

// SaveDefinition writes a definition to the filesystem as YAML and registers it.
func (l *Loader) SaveDefinition(def *Definition) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := yaml.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal definition: %w", err)
	}

	filename := def.Name + ".yaml"
	path := filepath.Join(l.dir, filename)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write definition file: %w", err)
	}

	if err := l.agentReg.RegisterAgent(def); err != nil {
		return err
	}
	l.definitions[def.Name] = def
	return nil
}

// DeleteDefinition removes a definition from the filesystem and registry.
func (l *Loader) DeleteDefinition(name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, ext := range []string{".yaml", ".yml"} {
		path := filepath.Join(l.dir, name+ext)
		_ = os.Remove(path)
	}

	delete(l.definitions, name)
	return nil
}

// GetDefinition returns a definition by name.
func (l *Loader) GetDefinition(name string) *Definition {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.definitions[name]
}

// GetRawDefinition returns the raw YAML bytes for a definition by name.
func (l *Loader) GetRawDefinition(name string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Try .yaml first, then .yml
	for _, ext := range []string{".yaml", ".yml"} {
		path := filepath.Join(l.dir, name+ext)
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("definition %q not found", name)
}

// SaveRawDefinition writes raw YAML bytes for a definition to disk, parses and registers it.
func (l *Loader) SaveRawDefinition(name string, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	if err := def.Validate(); err != nil {
		return err
	}

	filename := def.Name + ".yaml"
	path := filepath.Join(l.dir, filename)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write definition file: %w", err)
	}

	if err := l.agentReg.RegisterAgent(&def); err != nil {
		return err
	}
	// If name changed (shouldn't happen via UI, but be safe), clean up old file
	if name != def.Name {
		for _, ext := range []string{".yaml", ".yml"} {
			_ = os.Remove(filepath.Join(l.dir, name+ext))
		}
		delete(l.definitions, name)
	}
	l.definitions[def.Name] = &def
	return nil
}

// ListDefinitions returns all loaded definitions.
func (l *Loader) ListDefinitions() []*Definition {
	l.mu.Lock()
	defer l.mu.Unlock()
	defs := make([]*Definition, 0, len(l.definitions))
	for _, def := range l.definitions {
		defs = append(defs, def)
	}
	return defs
}

// Watch starts watching the definitions directory for changes.
func (l *Loader) Watch() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	l.watcher = watcher

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
					ext := strings.ToLower(filepath.Ext(event.Name))
					if ext == ".yaml" || ext == ".yml" {
						slog.Info("definition file changed, reloading", "file", event.Name)
						l.mu.Lock()
						if err := l.loadFile(event.Name); err != nil {
							slog.Error("failed to reload definition", "file", event.Name, "error", err)
						}
						l.mu.Unlock()
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Error("watcher error", "error", err)
			}
		}
	}()

	return watcher.Add(l.dir)
}

// Close stops the filesystem watcher.
func (l *Loader) Close() error {
	if l.watcher != nil {
		return l.watcher.Close()
	}
	return nil
}
