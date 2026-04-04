package config

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Definition is the structure parsed from a YAML file.
// It represents an agent definition.
type Definition struct {
	Kind               Kind              `yaml:"kind" json:"kind"`
	Name               string            `yaml:"name" json:"name"`
	Description        string            `yaml:"description" json:"description,omitempty"`
	Model              string            `yaml:"model,omitempty" json:"model,omitempty"`
	SystemPrompt       string            `yaml:"system_prompt" json:"system_prompt"`
	Tools              []string          `yaml:"tools,omitempty" json:"tools,omitempty"`
	MaxTurns           int               `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxConcurrentTools int               `yaml:"max_concurrent_tools,omitempty" json:"max_concurrent_tools,omitempty"`
	ForceJSON          bool              `yaml:"force_json,omitempty" json:"force_json,omitempty"`
	StructuredOutput   *StructuredOutput `yaml:"structured_output,omitempty" json:"structured_output,omitempty"`
	Scope              string            `yaml:"scope,omitempty" json:"scope,omitempty"`
	Team               string            `yaml:"team,omitempty" json:"team,omitempty"`
	CreatedBy          string            `yaml:"created_by,omitempty" json:"created_by,omitempty"`
}

// StructuredOutput configures JSON Schema constrained responses.
// It maps directly to the OpenAI json_schema response_format block.
type StructuredOutput struct {
	Name string `yaml:"name" json:"name"`
	// Schema holds the JSON Schema for the expected response.
	// The yaml:"schema" tag is not used by the library (UnmarshalYAML handles it manually),
	// but is kept for documentation purposes.
	Schema json.RawMessage `yaml:"schema" json:"schema"`
	Strict bool            `yaml:"strict,omitempty" json:"strict,omitempty"`
}

// UnmarshalYAML implements yaml.Unmarshaler for StructuredOutput.
// It handles converting the YAML schema node to JSON RawMessage.
func (s *StructuredOutput) UnmarshalYAML(value *yaml.Node) error {
	type plain struct {
		Name   string `yaml:"name"`
		Strict bool   `yaml:"strict"`
	}
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	s.Name = p.Name
	s.Strict = p.Strict

	// Find the schema node and convert it to JSON
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == "schema" {
			schemaNode := value.Content[i+1]
			jsonBytes, err := yamlNodeToJSON(schemaNode)
			if err != nil {
				return fmt.Errorf("structured_output.schema: %w", err)
			}
			s.Schema = json.RawMessage(jsonBytes)
			break
		}
	}
	return nil
}

// MarshalYAML implements yaml.Marshaler for StructuredOutput.
// It converts the JSON schema back to a nested YAML map.
func (s StructuredOutput) MarshalYAML() (any, error) {
	type withSchema struct {
		Name   string `yaml:"name"`
		Strict bool   `yaml:"strict,omitempty"`
		Schema any    `yaml:"schema"`
	}
	type withoutSchema struct {
		Name   string `yaml:"name"`
		Strict bool   `yaml:"strict,omitempty"`
	}

	if len(s.Schema) == 0 {
		return withoutSchema{Name: s.Name, Strict: s.Strict}, nil
	}

	var schemaMap any
	if err := json.Unmarshal(s.Schema, &schemaMap); err != nil {
		return nil, fmt.Errorf("structured_output.schema: %w", err)
	}
	return withSchema{Name: s.Name, Strict: s.Strict, Schema: schemaMap}, nil
}

// yamlNodeToJSON converts a *yaml.Node to a JSON byte slice.
func yamlNodeToJSON(node *yaml.Node) ([]byte, error) {
	var v any
	if err := node.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// Kind represents the type of definition.
type Kind string

const (
	KindAgent Kind = "agent"
)

type Scope string

const (
	ScopeGlobal Scope = "global"
	ScopeTeam   Scope = "team"
	ScopeUser   Scope = "user"
)

func (d *Definition) Validate() error {
	if d.Name == "" {
		return ErrMissingName
	}
	if d.Kind == "" {
		return ErrMissingKind
	}
	if d.Kind != KindAgent {
		return ErrInvalidKind
	}
	if d.SystemPrompt == "" {
		return ErrMissingSystemPrompt
	}
	if d.Scope != "" {
		s := Scope(d.Scope)
		if s != ScopeGlobal && s != ScopeTeam && s != ScopeUser {
			return ErrInvalidScope
		}
	}
	if Scope(d.Scope) == ScopeTeam && d.Team == "" {
		return ErrTeamRequired
	}
	return nil
}

// VisibleTo returns true if the agent is visible to the given subject, teams, and admin status.
func (d *Definition) VisibleTo(subject string, teams []string, isGlobalAdmin bool) bool {
	if isGlobalAdmin {
		return true
	}
	switch Scope(d.Scope) {
	case ScopeGlobal, "":
		return true
	case ScopeTeam:
		for _, t := range teams {
			if t == d.Team {
				return true
			}
		}
		return false
	case ScopeUser:
		return d.CreatedBy == subject
	}
	return false
}

// CanEdit returns true if the given subject can edit the agent.
func (d *Definition) CanEdit(subject string, teams []string, isGlobalAdmin, isTeamAdmin bool) bool {
	if isGlobalAdmin {
		return true
	}
	switch Scope(d.Scope) {
	case ScopeGlobal:
		return false
	case ScopeTeam:
		if !d.IsMemberOfTeam(teams) {
			return false
		}
		if d.CreatedBy == subject {
			return true
		}
		if isTeamAdmin {
			return true
		}
		return false
	case ScopeUser, "":
		return d.CreatedBy == subject
	}
	return false
}

// CanDelete returns true if the given subject can delete the agent.
func (d *Definition) CanDelete(subject string, teams []string, isGlobalAdmin, isTeamAdmin bool) bool {
	if isGlobalAdmin {
		return true
	}
	switch Scope(d.Scope) {
	case ScopeGlobal:
		return false
	case ScopeTeam:
		if d.CreatedBy == subject {
			return true
		}
		if isTeamAdmin {
			return true
		}
		return false
	case ScopeUser, "":
		return d.CreatedBy == subject
	}
	return false
}

func (d *Definition) IsMemberOfTeam(teams []string) bool {
	for _, t := range teams {
		if t == d.Team {
			return true
		}
	}
	return false
}
