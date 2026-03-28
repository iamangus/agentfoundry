package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/angoo/agentfile/internal/config"
	"github.com/angoo/agentfile/internal/llm"
)

// mockLLMClient captures the last request and returns a canned response.
type mockLLMClient struct {
	lastRequest *llm.ChatRequest
}

func (m *mockLLMClient) ChatCompletion(_ context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error) {
	m.lastRequest = req
	return &llm.ChatResponse{
		Choices: []llm.Choice{
			{Index: 0, Message: llm.Message{Role: "assistant", Content: "ok"}},
		},
	}, nil
}

// mockResolver always returns not found.
type mockResolver struct{}

func (m *mockResolver) GetAgentDef(_ string) (*config.Definition, bool) { return nil, false }

func newTestRuntime(client *mockLLMClient) *Runtime {
	return NewRuntime(&mockResolver{}, nil, client)
}

func TestRunWithHistory_NoResponseFormat(t *testing.T) {
	client := &mockLLMClient{}
	rt := newTestRuntime(client)
	def := &config.Definition{
		Kind: config.KindAgent, Name: "test", SystemPrompt: "You are a test.",
	}
	rt.RunWithHistory(context.Background(), def, "hello", nil, nil, nil, nil)
	if client.lastRequest.ResponseFormat != nil {
		t.Errorf("expected nil ResponseFormat, got %+v", client.lastRequest.ResponseFormat)
	}
}

func TestRunWithHistory_ForceJSON(t *testing.T) {
	client := &mockLLMClient{}
	rt := newTestRuntime(client)
	def := &config.Definition{
		Kind: config.KindAgent, Name: "test", SystemPrompt: "You are a test.",
		ForceJSON: true,
	}
	rt.RunWithHistory(context.Background(), def, "hello", nil, nil, nil, nil)
	if client.lastRequest.ResponseFormat == nil {
		t.Fatal("expected ResponseFormat to be set")
	}
	if client.lastRequest.ResponseFormat.Type != "json_object" {
		t.Errorf("got type=%q, want json_object", client.lastRequest.ResponseFormat.Type)
	}
	if client.lastRequest.ResponseFormat.JSONSchema != nil {
		t.Error("expected JSONSchema to be nil for json_object mode")
	}
}

func TestRunWithHistory_StructuredOutputFromDefinition(t *testing.T) {
	client := &mockLLMClient{}
	rt := newTestRuntime(client)
	schema := json.RawMessage(`{"type":"object","properties":{"score":{"type":"integer"}},"required":["score"]}`)
	def := &config.Definition{
		Kind: config.KindAgent, Name: "test", SystemPrompt: "You are a test.",
		StructuredOutput: &config.StructuredOutput{Name: "result", Schema: schema, Strict: true},
	}
	rt.RunWithHistory(context.Background(), def, "hello", nil, nil, nil, nil)
	rf := client.lastRequest.ResponseFormat
	if rf == nil {
		t.Fatal("expected ResponseFormat to be set")
	}
	if rf.Type != "json_schema" {
		t.Errorf("got type=%q, want json_schema", rf.Type)
	}
	if rf.JSONSchema == nil {
		t.Fatal("expected JSONSchema to be set")
	}
	if rf.JSONSchema.Name != "result" {
		t.Errorf("got JSONSchema.Name=%q, want result", rf.JSONSchema.Name)
	}
	if !rf.JSONSchema.Strict {
		t.Error("expected Strict=true")
	}
}

func TestRunWithHistory_OverrideReplacesDefinition(t *testing.T) {
	client := &mockLLMClient{}
	rt := newTestRuntime(client)
	defSchema := json.RawMessage(`{"type":"object"}`)
	overrideSchema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	def := &config.Definition{
		Kind: config.KindAgent, Name: "test", SystemPrompt: "You are a test.",
		StructuredOutput: &config.StructuredOutput{Name: "def_result", Schema: defSchema},
	}
	override := &config.StructuredOutput{Name: "override_result", Schema: overrideSchema, Strict: true}
	rt.RunWithHistory(context.Background(), def, "hello", nil, nil, override, nil)
	rf := client.lastRequest.ResponseFormat
	if rf == nil {
		t.Fatal("expected ResponseFormat to be set")
	}
	if rf.JSONSchema == nil || rf.JSONSchema.Name != "override_result" {
		t.Errorf("expected override name 'override_result', got %+v", rf.JSONSchema)
	}
}

func TestRunWithHistory_StructuredOutputWinsOverForceJSON(t *testing.T) {
	client := &mockLLMClient{}
	rt := newTestRuntime(client)
	schema := json.RawMessage(`{"type":"object"}`)
	def := &config.Definition{
		Kind: config.KindAgent, Name: "test", SystemPrompt: "You are a test.",
		ForceJSON:        true,
		StructuredOutput: &config.StructuredOutput{Name: "result", Schema: schema},
	}
	rt.RunWithHistory(context.Background(), def, "hello", nil, nil, nil, nil)
	rf := client.lastRequest.ResponseFormat
	if rf == nil {
		t.Fatal("expected ResponseFormat to be set")
	}
	if rf.Type != "json_schema" {
		t.Errorf("got type=%q, want json_schema (StructuredOutput should win over ForceJSON)", rf.Type)
	}
}

func TestRunWithHistory_OverrideNilUsesDefinition(t *testing.T) {
	client := &mockLLMClient{}
	rt := newTestRuntime(client)
	schema := json.RawMessage(`{"type":"object"}`)
	def := &config.Definition{
		Kind: config.KindAgent, Name: "test", SystemPrompt: "You are a test.",
		StructuredOutput: &config.StructuredOutput{Name: "def_result", Schema: schema},
	}
	// nil override → should use definition's StructuredOutput
	rt.RunWithHistory(context.Background(), def, "hello", nil, nil, nil, nil)
	rf := client.lastRequest.ResponseFormat
	if rf == nil || rf.JSONSchema == nil {
		t.Fatal("expected ResponseFormat with JSONSchema to be set")
	}
	if rf.JSONSchema.Name != "def_result" {
		t.Errorf("got JSONSchema.Name=%q, want def_result", rf.JSONSchema.Name)
	}
}
