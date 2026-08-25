package config_test

import (
	"encoding/json"
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/angoo/agentfoundry/internal/config"
)

func TestStructuredOutput_YAMLRoundTrip(t *testing.T) {
	input := `kind: agent
name: test-agent
system_prompt: You are a test agent.
structured_output:
    name: result
    strict: true
    schema:
        type: object
        properties:
            score:
                type: integer
        required:
            - score
        additionalProperties: false
`
	var def config.Definition
	if err := yaml.Unmarshal([]byte(input), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.StructuredOutput == nil {
		t.Fatal("expected StructuredOutput to be set")
	}
	if def.StructuredOutput.Name != "result" {
		t.Errorf("got Name=%q, want %q", def.StructuredOutput.Name, "result")
	}
	if !def.StructuredOutput.Strict {
		t.Error("expected Strict=true")
	}
	// Schema should round-trip as valid JSON
	var schema map[string]any
	if err := json.Unmarshal(def.StructuredOutput.Schema, &schema); err != nil {
		t.Errorf("Schema is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("got schema.type=%v, want object", schema["type"])
	}
}

func TestDefinition_StructuredOutputNilByDefault(t *testing.T) {
	input := `kind: agent
name: simple
system_prompt: Hello.
`
	var def config.Definition
	if err := yaml.Unmarshal([]byte(input), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.StructuredOutput != nil {
		t.Errorf("expected StructuredOutput to be nil, got %+v", def.StructuredOutput)
	}
}

func TestHandoff_YAMLRoundTrip(t *testing.T) {
	input := `kind: agent
name: triage
system_prompt: You triage requests.
handoff_to: resolver
handoffs:
    - billing
    - support
`
	var def config.Definition
	if err := yaml.Unmarshal([]byte(input), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.HandoffTo != "resolver" {
		t.Errorf("got HandoffTo=%q, want %q", def.HandoffTo, "resolver")
	}
	if len(def.Handoffs) != 2 || def.Handoffs[0] != "billing" || def.Handoffs[1] != "support" {
		t.Errorf("got Handoffs=%v, want [billing support]", def.Handoffs)
	}
}

func TestPreInferenceProcessors_YAMLRoundTrip(t *testing.T) {
	input := `kind: agent
name: notes-agent
system_prompt: You manage notes.
pre_inference_processors:
  - id: note-skills
    processor: mcp_tool
    phase: run_start
    on_error: warn
    timeout: 5
    config:
      server: notes
      tool: list_skills
      arguments:
        include_metadata: true
`
	var def config.Definition
	if err := yaml.Unmarshal([]byte(input), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(def.PreInferenceProcessors) != 1 {
		t.Fatalf("got %d processors, want 1", len(def.PreInferenceProcessors))
	}
	p := def.PreInferenceProcessors[0]
	if p.ID != "note-skills" || p.Processor != "mcp_tool" || p.Phase != "run_start" || p.OnError != "warn" || p.Timeout != 5 {
		t.Fatalf("unexpected processor: %+v", p)
	}
	var processorConfig map[string]any
	if err := json.Unmarshal(p.Config, &processorConfig); err != nil {
		t.Fatalf("unmarshal processor config: %v", err)
	}
	if processorConfig["server"] != "notes" || processorConfig["tool"] != "list_skills" {
		t.Fatalf("unexpected processor config: %v", processorConfig)
	}
}

func TestValidate_PreInferenceProcessor(t *testing.T) {
	def := validHandoffDefinition()
	def.PreInferenceProcessors = []config.PreInferenceProcessor{{Processor: "mcp_tool", Phase: "before_inference"}}
	if err := def.Validate(); err == nil {
		t.Fatal("expected unsupported phase to fail validation")
	}

	def.PreInferenceProcessors = []config.PreInferenceProcessor{{Processor: "mcp_tool", Phase: "run_start", OnError: "invalid"}}
	if err := def.Validate(); err == nil {
		t.Fatal("expected invalid on_error to fail validation")
	}
}

func validHandoffDefinition() *config.Definition {
	return &config.Definition{
		Kind:         config.KindAgent,
		Name:         "agent",
		SystemPrompt: "hello",
	}
}

func TestValidate_SelfHandoffRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Definition)
	}{
		{"HandoffTo equals name", func(d *config.Definition) { d.HandoffTo = d.Name }},
		{"Handoffs contains name", func(d *config.Definition) { d.Handoffs = []string{d.Name} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := validHandoffDefinition()
			tc.mutate(def)
			if err := def.Validate(); err == nil {
				t.Fatal("expected Validate to reject self-handoff, got nil")
			}
		})
	}
}

func TestValidate_HandoffOK(t *testing.T) {
	def := validHandoffDefinition()
	def.HandoffTo = "other"
	def.Handoffs = []string{"other", "another"}
	if err := def.Validate(); err != nil {
		t.Fatalf("expected Validate to accept non-self handoffs, got %v", err)
	}
}

func TestValidate_NoHandoffOK(t *testing.T) {
	def := validHandoffDefinition()
	if err := def.Validate(); err != nil {
		t.Fatalf("expected Validate to accept no handoffs, got %v", err)
	}
}

func TestShippedDefinitionsParse(t *testing.T) {
	paths := []string{
		"../../definitions/assistant.yaml",
		"../../definitions/summarizer.yaml",
		"../../definitions/researcher.yaml",
		"../../definitions/historian.yaml",
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Skipf("definitions dir not available: %v", err)
		}
		var def config.Definition
		if err := yaml.Unmarshal(data, &def); err != nil {
			t.Errorf("%s: unmarshal: %v", p, err)
			continue
		}
		if def.Kind != config.KindAgent || def.Name == "" {
			t.Errorf("%s: invalid definition", p)
		}
	}
}
