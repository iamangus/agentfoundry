# Structured Outputs (JSON Schema) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add proper `json_schema` structured output support to agent definitions, the run API, and the web UI.

**Architecture:** A new `StructuredOutput` struct mirrors the OpenAI `json_schema` block and is added to `config.Definition`. The agent runtime picks it up when building `ChatRequest.ResponseFormat`, with an API-level override taking priority over the definition. The web UI provides a toggle + JSON textarea that manages the `structured_output:` YAML key separately from the main editor.

**Tech Stack:** Go, `gopkg.in/yaml.v3`, standard `encoding/json`, HTMX, vanilla JS in template.

---

### Task 1: Add `StructuredOutput` type to config and update LLM `ResponseFormat`

**Files:**
- Modify: `internal/config/definition.go`
- Modify: `internal/llm/client.go`
- Create: `internal/config/definition_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/config/definition_test.go`:

```go
package config_test

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
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
	var def Definition
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
	var def Definition
	if err := yaml.Unmarshal([]byte(input), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.StructuredOutput != nil {
		t.Errorf("expected StructuredOutput to be nil, got %+v", def.StructuredOutput)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go test ./internal/config/... -run TestStructuredOutput -v
```

Expected: FAIL — `Definition` has no `StructuredOutput` field yet.

- [ ] **Step 3: Add `StructuredOutput` struct and field to `internal/config/definition.go`**

Replace the existing file content with:

```go
package config

import "encoding/json"

// Definition is the structure parsed from a YAML file.
// It represents an agent definition.
type Definition struct {
	Kind               Kind              `yaml:"kind" json:"kind"`
	Name               string            `yaml:"name" json:"name"`
	Description        string            `yaml:"description" json:"description"`
	Model              string            `yaml:"model,omitempty" json:"model,omitempty"`
	SystemPrompt       string            `yaml:"system_prompt" json:"system_prompt"`
	Tools              []string          `yaml:"tools,omitempty" json:"tools,omitempty"`
	MaxTurns           int               `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxConcurrentTools int               `yaml:"max_concurrent_tools,omitempty" json:"max_concurrent_tools,omitempty"`
	ForceJSON          bool              `yaml:"force_json,omitempty" json:"force_json,omitempty"`
	StructuredOutput   *StructuredOutput `yaml:"structured_output,omitempty" json:"structured_output,omitempty"`
}

// StructuredOutput configures JSON Schema constrained responses.
// It maps directly to the OpenAI json_schema response_format block.
type StructuredOutput struct {
	Name   string          `yaml:"name" json:"name"`
	Schema json.RawMessage `yaml:"schema" json:"schema"`
	Strict bool            `yaml:"strict,omitempty" json:"strict,omitempty"`
}

// Kind represents the type of definition.
type Kind string

const (
	KindAgent Kind = "agent"
)

// Validate checks that the definition has required fields set.
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
	return nil
}
```

- [ ] **Step 4: Update `ResponseFormat` in `internal/llm/client.go`**

Replace lines 76-79 (the `ResponseFormat` struct) with:

```go
// ResponseFormat instructs the model to produce output in a specific format.
type ResponseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

// JSONSchema is the json_schema block within a ResponseFormat.
// It mirrors the OpenAI structured outputs format exactly.
type JSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go test ./internal/config/... -v
```

Expected: PASS. Also run `go build ./...` to confirm compilation.

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/config/definition.go internal/config/definition_test.go internal/llm/client.go
git commit -m "feat: add StructuredOutput type to config and JSONSchema to LLM ResponseFormat"
```

---

### Task 2: Update agent runtime to apply structured output with override support

**Files:**
- Modify: `internal/agent/agent.go`
- Create: `internal/agent/agent_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/agent/agent_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go test ./internal/agent/... -v
```

Expected: FAIL — `RunWithHistory` doesn't accept a `*config.StructuredOutput` parameter yet.

- [ ] **Step 3: Update `RunWithHistory` signature and response format logic in `internal/agent/agent.go`**

Change the `RunWithHistory` signature (line 113) to add `structuredOutput *config.StructuredOutput` before `history`:

```go
func (rt *Runtime) RunWithHistory(ctx context.Context, def *config.Definition, userInput string, r Reporter, hr HistoryRecorder, structuredOutput *config.StructuredOutput, history []llm.Message, ephemeral ...*mcpclient.EphemeralConn) (string, []llm.Message, error) {
```

Update `RunWithReporter` (line 108) to pass `nil` for the new parameter:

```go
func (rt *Runtime) RunWithReporter(ctx context.Context, def *config.Definition, userInput string, r Reporter, history []llm.Message, ephemeral ...*mcpclient.EphemeralConn) (string, []llm.Message, error) {
	return rt.RunWithHistory(ctx, def, userInput, r, nil, nil, history, ephemeral...)
}
```

Replace the existing `force_json` block (lines 163-165) with the priority logic:

```go
// Build response format: structured_output override > definition's structured_output > force_json
so := structuredOutput
if so == nil {
    so = def.StructuredOutput
}
if so != nil {
    req.ResponseFormat = &llm.ResponseFormat{
        Type: "json_schema",
        JSONSchema: &llm.JSONSchema{
            Name:   so.Name,
            Schema: so.Schema,
            Strict: so.Strict,
        },
    }
} else if def.ForceJSON {
    req.ResponseFormat = &llm.ResponseFormat{Type: "json_object"}
}
```

- [ ] **Step 4: Fix the sub-agent call in `executeTool` (line 463)**

The sub-agent call uses `RunWithReporter` which is unchanged — no fix needed. Verify it compiles:

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 5: Run tests to verify they pass**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go test ./internal/agent/... -v
```

Expected: all 5 tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/agent.go internal/agent/agent_test.go
git commit -m "feat: add structured output override support to agent RunWithHistory"
```

---

### Task 3: Add `response_schema` override to the run API

**Files:**
- Modify: `internal/api/http.go`

- [ ] **Step 1: Add `ResponseSchema` to `runAgentRequest`**

In `internal/api/http.go`, replace the `runAgentRequest` struct (lines 160-164):

```go
// runAgentRequest is the JSON body for POST /api/v1/agents/{name}/run.
type runAgentRequest struct {
	Message        string                    `json:"message"`
	History        []llm.Message             `json:"history,omitempty"`
	MCPServers     []mcpclient.ServerConfig   `json:"mcp_servers,omitempty"`
	ResponseSchema *config.StructuredOutput  `json:"response_schema,omitempty"`
}
```

- [ ] **Step 2: Pass `ResponseSchema` to `RunWithHistory`**

In the `runAgent` handler, find the `RunWithHistory` call (line 247) and update it to pass the override:

```go
result, _, err := h.agentRuntime.RunWithHistory(ctx, defSnap, msgSnap, nil, hr, req.ResponseSchema, historySnap, ephemeral...)
```

- [ ] **Step 3: Build and verify**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go build ./...
```

Expected: no errors.

- [ ] **Step 4: Smoke test the API manually**

Start the server (if available) and verify the existing run endpoint still works by sending a request without `response_schema`. If a running server is not available, confirm compilation is sufficient.

- [ ] **Step 5: Commit**

```bash
git add internal/api/http.go
git commit -m "feat: add response_schema override to run API request"
```

---

### Task 4: Update web handler to extract and inject `structured_output`

**Files:**
- Modify: `internal/web/handler.go`

The strategy: when loading an agent for editing, strip `structured_output:` from the displayed YAML and pass it as JSON separately to the template. When saving, merge the JSON textarea value back into the YAML before writing to disk.

- [ ] **Step 1: Add `StructuredOutputJSON` to `agentEditorData`**

In `internal/web/handler.go`, replace the `agentEditorData` struct (lines 523-526):

```go
type agentEditorData struct {
	Name               string
	RawYAML            string
	StructuredOutputJSON string // JSON representation of structured_output, empty if not set
}
```

- [ ] **Step 2: Update `agentEditPartial` to strip `structured_output` from displayed YAML**

Replace the `agentEditPartial` handler (lines 553-565):

```go
func (h *Handler) agentEditPartial(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	raw, err := h.store.GetRawDefinition(name)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	// Parse to extract StructuredOutput separately from the YAML editor.
	var def config.Definition
	if err := yamlpkg.Unmarshal(raw, &def); err != nil {
		http.Error(w, "invalid agent YAML", http.StatusInternalServerError)
		return
	}

	var soJSON string
	if def.StructuredOutput != nil {
		b, err := json.MarshalIndent(def.StructuredOutput, "", "  ")
		if err == nil {
			soJSON = string(b)
		}
	}

	// Strip structured_output from displayed YAML so it's only edited via the JSON panel.
	var m map[string]any
	if err := yamlpkg.Unmarshal(raw, &m); err == nil {
		delete(m, "structured_output")
		if stripped, err := yamlpkg.Marshal(m); err == nil {
			raw = stripped
		}
	}

	data := agentEditorData{
		Name:                 name,
		RawYAML:              string(raw),
		StructuredOutputJSON: soJSON,
	}
	h.renderPartial(w, "agent-editor", data)
}
```

- [ ] **Step 3: Update `saveAgentYaml` to merge structured output**

Replace the `saveAgentYaml` handler (lines 629-643):

```go
func (h *Handler) saveAgentYaml(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	yamlStr := r.FormValue("yaml")
	soJSON := r.FormValue("structured_output_json")
	soEnabled := r.FormValue("structured_output_enabled") == "true"

	// Parse the submitted YAML into a Definition.
	var def config.Definition
	if err := yamlpkg.Unmarshal([]byte(yamlStr), &def); err != nil {
		http.Error(w, "invalid YAML: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Apply structured output: if enabled and JSON provided, set it; otherwise clear it.
	if soEnabled && soJSON != "" {
		var so config.StructuredOutput
		if err := json.Unmarshal([]byte(soJSON), &so); err != nil {
			http.Error(w, "invalid structured output JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		def.StructuredOutput = &so
	} else {
		def.StructuredOutput = nil
	}

	// Marshal back to canonical YAML and save.
	merged, err := yamlpkg.Marshal(&def)
	if err != nil {
		http.Error(w, "failed to marshal YAML: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.store.SaveRawDefinition(name, merged); err != nil {
		slog.Error("failed to save yaml", "name", name, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Respond with both the updated agent list (OOB) and a fresh editor.
	// Re-load and re-split so the editor reflects the saved state.
	raw, _ := h.store.GetRawDefinition(name)
	var savedDef config.Definition
	var soJSONOut string
	if err := yamlpkg.Unmarshal(raw, &savedDef); err == nil && savedDef.StructuredOutput != nil {
		if b, err := json.MarshalIndent(savedDef.StructuredOutput, "", "  "); err == nil {
			soJSONOut = string(b)
		}
	}
	// Strip structured_output from displayed YAML.
	var m map[string]any
	if err := yamlpkg.Unmarshal(raw, &m); err == nil {
		delete(m, "structured_output")
		if stripped, err := yamlpkg.Marshal(m); err == nil {
			raw = stripped
		}
	}
	h.renderPartial(w, "save-yaml-response", saveYamlData{
		Editor: agentEditorData{Name: name, RawYAML: string(raw), StructuredOutputJSON: soJSONOut},
		Agents: h.store.ListDefinitions(),
	})
}
```

- [ ] **Step 4: Update `createAgentYaml` to also strip `structured_output` from editor display**

Replace the `createAgentYaml` handler (lines 603-626):

```go
func (h *Handler) createAgentYaml(w http.ResponseWriter, r *http.Request) {
	rawYAML := r.FormValue("yaml")
	if rawYAML == "" {
		http.Error(w, "yaml is required", http.StatusBadRequest)
		return
	}
	if err := h.store.SaveRawDefinition("", []byte(rawYAML)); err != nil {
		slog.Error("failed to create agent from yaml", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var def config.Definition
	if err := yamlpkg.Unmarshal([]byte(rawYAML), &def); err != nil || def.Name == "" {
		h.renderPartial(w, "agent-list-items", agentsPageData{Agents: h.store.ListDefinitions()})
		return
	}
	raw, _ := h.store.GetRawDefinition(def.Name)

	var soJSON string
	var savedDef config.Definition
	if err := yamlpkg.Unmarshal(raw, &savedDef); err == nil && savedDef.StructuredOutput != nil {
		if b, err := json.MarshalIndent(savedDef.StructuredOutput, "", "  "); err == nil {
			soJSON = string(b)
		}
	}
	// Strip structured_output from displayed YAML.
	var m map[string]any
	if err := yamlpkg.Unmarshal(raw, &m); err == nil {
		delete(m, "structured_output")
		if stripped, err := yamlpkg.Marshal(m); err == nil {
			raw = stripped
		}
	}

	h.renderPartial(w, "save-yaml-response", saveYamlData{
		Editor: agentEditorData{Name: def.Name, RawYAML: string(raw), StructuredOutputJSON: soJSON},
		Agents: h.store.ListDefinitions(),
	})
}
```

- [ ] **Step 5: Build and verify**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go build ./...
```

Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/web/handler.go
git commit -m "feat: extract and inject structured_output in web agent editor handlers"
```

---

### Task 5: Add structured output toggle + JSON textarea to the agent editor UI

**Files:**
- Modify: `internal/web/templates/agents.html`

The `agent-editor` template needs:
1. Hidden `structured_output_enabled` input (updated by the checkbox via JS)
2. Hidden `structured_output_json` input (updated by the textarea via JS)
3. A visible toggle checkbox
4. A JSON textarea revealed when the checkbox is checked

- [ ] **Step 1: Update `agent-editor` template in `agents.html`**

In `agents.html`, replace the `agent-editor` template (lines 227-270) with:

```html
{{define "agent-editor"}}
<div class="flex flex-col h-full">
  <!-- Editor header -->
  <div class="flex items-center justify-between px-4 py-3 border-b shrink-0">
    <div class="flex items-center gap-2">
      <label for="sidebar-toggle" class="sidebar-toggle-btn" aria-label="Toggle sidebar">
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="18" height="18" rx="2"/><path d="M9 3v18"/>
        </svg>
      </label>
      <h2 class="text-sm font-semibold">{{.Name}}</h2>
      <span class="text-xs text-muted-foreground border rounded px-1.5 py-0.5" style="border-radius: 4px;">YAML</span>
    </div>
    <div class="flex items-center gap-3">
      <span id="save-status-{{.Name}}" class="text-xs text-muted-foreground"></span>
      <button
        type="submit"
        form="yaml-form-{{.Name}}"
        class="uk-button uk-button-primary uk-button-small"
        style="border-radius: 6px; font-size: 12px; padding: 4px 14px;"
      >
        Save
      </button>
    </div>
  </div>

  <!-- YAML form -->
  <div class="flex-1 flex flex-col p-4 gap-3 overflow-y-auto">
    <form
      id="yaml-form-{{.Name}}"
      hx-put="/agents/{{.Name}}/yaml"
      hx-target="#agent-editor"
      hx-swap="innerHTML"
      class="flex flex-col gap-3"
      style="min-height: 0;"
    >
      <textarea
        name="yaml"
        class="uk-textarea yaml-editor"
        style="min-height: 280px; flex: none;"
        spellcheck="false"
      >{{.RawYAML}}</textarea>

      <!-- Hidden fields submitted with the form -->
      <input type="hidden" name="structured_output_enabled" id="so-enabled-{{.Name}}" value="{{if .StructuredOutputJSON}}true{{else}}false{{end}}" />
      <input type="hidden" name="structured_output_json" id="so-json-{{.Name}}" value="{{.StructuredOutputJSON}}" />

      <!-- Structured Output section -->
      <div style="border: 1px solid hsl(var(--border)); border-radius: 8px; padding: 12px 14px;">
        <label style="display: flex; align-items: center; gap: 8px; cursor: pointer; user-select: none;">
          <input
            type="checkbox"
            id="so-toggle-{{.Name}}"
            {{if .StructuredOutputJSON}}checked{{end}}
            onchange="toggleStructuredOutput('{{.Name}}')"
            style="width: 15px; height: 15px;"
          />
          <span class="text-sm font-medium">Enable structured output (JSON Schema)</span>
        </label>
        <div id="so-panel-{{.Name}}" style="margin-top: 10px; {{if not .StructuredOutputJSON}}display:none;{{end}}">
          <p class="text-xs text-muted-foreground" style="margin-bottom: 6px;">
            JSON object with <code>name</code>, <code>schema</code>, and optionally <code>strict</code>.
            This is the same object you pass as <code>response_schema</code> in the run API.
          </p>
          <textarea
            id="so-textarea-{{.Name}}"
            class="uk-textarea"
            style="font-family: ui-monospace, monospace; font-size: 12px; min-height: 140px; border-radius: 6px;"
            spellcheck="false"
            placeholder='{"name": "result", "strict": true, "schema": {"type": "object", "properties": {}, "required": [], "additionalProperties": false}}'
            oninput="syncStructuredOutput('{{.Name}}')"
          >{{.StructuredOutputJSON}}</textarea>
        </div>
      </div>
    </form>
  </div>
</div>

<script>
function toggleStructuredOutput(name) {
  var checkbox = document.getElementById('so-toggle-' + name);
  var panel = document.getElementById('so-panel-' + name);
  var enabledInput = document.getElementById('so-enabled-' + name);
  var enabled = checkbox.checked;
  panel.style.display = enabled ? 'block' : 'none';
  enabledInput.value = enabled ? 'true' : 'false';
}

function syncStructuredOutput(name) {
  var textarea = document.getElementById('so-textarea-' + name);
  var jsonInput = document.getElementById('so-json-' + name);
  jsonInput.value = textarea.value;
}
</script>
{{end}}
```

Note: the `{{if not .StructuredOutputJSON}}` syntax needs a helper or inline approach. In Go templates, `not` is available. If it doesn't work, use `{{if eq .StructuredOutputJSON ""}}` instead.

- [ ] **Step 2: Fix the `{{if not .StructuredOutputJSON}}` if needed**

Go templates support `not` via the `not` function: `{{if not .StructuredOutputJSON}}`. If the template fails to parse, change that line to:

```html
style="margin-top: 10px; {{if eq .StructuredOutputJSON ""}}display:none;{{end}}"
```

- [ ] **Step 3: Build and verify template parses**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go build ./...
```

Expected: no errors (template is parsed at startup via `embed.FS`).

- [ ] **Step 4: Run full test suite**

```bash
go test ./...
```

Expected: all tests pass.

- [ ] **Step 5: Manual smoke test**

Start the server:
```bash
go run ./cmd/agentfile/
```

1. Open `/agents` in browser
2. Click an existing agent — verify YAML editor loads without `structured_output:` block
3. Toggle "Enable structured output" — verify JSON textarea appears
4. Paste a valid structured output JSON and save — verify it persists (re-open agent, checkbox checked, JSON populated)
5. Uncheck toggle and save — verify `structured_output:` is removed
6. Verify existing agent runs still work via `/chat`

- [ ] **Step 6: Commit**

```bash
git add internal/web/templates/agents.html
git commit -m "feat: add structured output toggle and JSON textarea to agent editor UI"
```

---

## API Usage Reference

Once implemented, structured outputs can be configured:

**In YAML definition:**
```yaml
structured_output:
  name: analysis_result
  strict: true
  schema:
    type: object
    properties:
      summary:
        type: string
      score:
        type: integer
    required: [summary, score]
    additionalProperties: false
```

**Via API override:**
```bash
curl -X POST /api/v1/agents/my-agent/run \
  -H "Content-Type: application/json" \
  -d '{
    "message": "Analyze this text...",
    "response_schema": {
      "name": "analysis_result",
      "strict": true,
      "schema": {
        "type": "object",
        "properties": {
          "summary": {"type": "string"},
          "score": {"type": "integer"}
        },
        "required": ["summary", "score"],
        "additionalProperties": false
      }
    }
  }'
```
