# Agent Form Editor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the raw YAML editor in the agent UI with a proper form giving each field its own input, and add a hover-visible Clone button to duplicate agents.

**Architecture:** Three tasks: (1) restructure Go handler data types and add new form save/create handlers, (2) replace the `agent-editor` HTML template with form fields, (3) replace `agent-editor-new` template, add clone button, add clone handler with collision-safe naming. The existing YAML API endpoints (`/api/v1/agents/*`) are untouched throughout.

**Tech Stack:** Go `net/http`, `gopkg.in/yaml.v3`, HTMX, FrankenUI/Tailwind CSS, vanilla JS.

---

## File Map

| File | Change |
|---|---|
| `internal/web/handler.go` | Change `agentEditorData`, add helpers, add `saveAgentForm`/`createAgentFormNew`/`cloneAgent`/`cloneAgentName`, remove old YAML handlers, register new routes |
| `internal/web/handler_test.go` | New — test `cloneAgentName` collision logic |
| `internal/web/templates/agents.html` | Add `save-agent-response` template (Task 1), replace `agent-editor` (Task 2), replace `agent-editor-new` + update `agent-list-items` (Task 3) |

---

### Task 1: Restructure handler data types, add form handlers, add `save-agent-response` template

**Files:**
- Modify: `internal/web/handler.go`
- Modify: `internal/web/templates/agents.html`

- [ ] **Step 1: Update `agentEditorData` struct in `internal/web/handler.go`**

Replace the existing struct (around line 523):
```go
type agentEditorData struct {
	Def                  *config.Definition
	StructuredOutputJSON string // JSON for the structured output panel; empty if not set
}
```

- [ ] **Step 2: Add `strconv` to imports**

The imports block in `handler.go` currently has:
```go
import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	...
)
```

Add `"strconv"` to the import block.

- [ ] **Step 3: Add `joinLines` to the template FuncMap**

In `NewHandler`, add to the `funcMap`:
```go
"joinLines": func(ss []string) string {
    return strings.Join(ss, "\n")
},
```

- [ ] **Step 4: Add `structuredOutputJSON` helper function**

Add after the `saveYamlData` struct definition:
```go
// structuredOutputJSON marshals def.StructuredOutput to indented JSON, or returns "" if nil.
func structuredOutputJSON(def *config.Definition) string {
	if def == nil || def.StructuredOutput == nil {
		return ""
	}
	b, err := json.MarshalIndent(def.StructuredOutput, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
```

- [ ] **Step 5: Add `definitionFromForm` helper function**

```go
// definitionFromForm builds a Definition from form values.
// Kind and Name must be set by the caller.
func definitionFromForm(r *http.Request) *config.Definition {
	def := &config.Definition{
		Kind:         config.KindAgent,
		Description:  r.FormValue("description"),
		Model:        r.FormValue("model"),
		SystemPrompt: r.FormValue("system_prompt"),
		ForceJSON:    r.FormValue("force_json") != "",
	}
	if v := r.FormValue("max_turns"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			def.MaxTurns = n
		}
	}
	if v := r.FormValue("max_concurrent_tools"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			def.MaxConcurrentTools = n
		}
	}
	toolsStr := r.FormValue("tools")
	for _, line := range strings.Split(toolsStr, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			def.Tools = append(def.Tools, t)
		}
	}
	return def
}
```

- [ ] **Step 6: Update `agentEditPartial` handler**

Replace the existing `agentEditPartial` handler with this simpler version (no YAML stripping needed):

```go
func (h *Handler) agentEditPartial(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	def := h.store.GetDefinition(name)
	if def == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	h.renderPartial(w, "agent-editor", agentEditorData{
		Def:                  def,
		StructuredOutputJSON: structuredOutputJSON(def),
	})
}
```

- [ ] **Step 7: Update `newAgentEditor` handler**

Replace with:
```go
func (h *Handler) newAgentEditor(w http.ResponseWriter, r *http.Request) {
	h.renderPartial(w, "agent-editor-new", agentEditorData{Def: &config.Definition{}})
}
```

- [ ] **Step 8: Add `saveAgentForm` handler**

```go
// saveAgentForm handles PUT /agents/{name} — saves an existing agent via the form UI.
func (h *Handler) saveAgentForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	def := definitionFromForm(r)
	def.Name = name

	soJSON := r.FormValue("structured_output_json")
	soEnabled := r.FormValue("structured_output_enabled") == "true"
	if soEnabled && soJSON != "" {
		var so config.StructuredOutput
		if err := json.Unmarshal([]byte(soJSON), &so); err != nil {
			http.Error(w, "invalid structured output JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		def.StructuredOutput = &so
	}

	if err := def.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.SaveDefinition(def); err != nil {
		slog.Error("failed to save agent", "name", name, "error", err)
		http.Error(w, "failed to save", http.StatusInternalServerError)
		return
	}

	saved := h.store.GetDefinition(name)
	h.renderPartial(w, "save-agent-response", saveYamlData{
		Editor: agentEditorData{Def: saved, StructuredOutputJSON: structuredOutputJSON(saved)},
		Agents: h.store.ListDefinitions(),
	})
}
```

- [ ] **Step 9: Add `createAgentFormNew` handler**

```go
// createAgentFormNew handles POST /agents/form — creates a new agent via the form UI.
func (h *Handler) createAgentFormNew(w http.ResponseWriter, r *http.Request) {
	def := definitionFromForm(r)
	def.Name = r.FormValue("name")

	soJSON := r.FormValue("structured_output_json")
	soEnabled := r.FormValue("structured_output_enabled") == "true"
	if soEnabled && soJSON != "" {
		var so config.StructuredOutput
		if err := json.Unmarshal([]byte(soJSON), &so); err != nil {
			http.Error(w, "invalid structured output JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		def.StructuredOutput = &so
	}

	if err := def.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.SaveDefinition(def); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	saved := h.store.GetDefinition(def.Name)
	h.renderPartial(w, "save-agent-response", saveYamlData{
		Editor: agentEditorData{Def: saved, StructuredOutputJSON: structuredOutputJSON(saved)},
		Agents: h.store.ListDefinitions(),
	})
}
```

- [ ] **Step 10: Remove old YAML web handlers and update routes**

Delete the following handler functions entirely:
- `createAgentForm` (old form handler at `POST /agents`)
- `createAgentYaml` (YAML paste handler at `POST /agents/yaml`)
- `saveAgentYaml` (YAML save handler at `PUT /agents/{name}/yaml`)

Also delete `saveYamlData` struct... actually keep it — `saveAgentForm` and `createAgentFormNew` still use it. Just remove the three handler functions above.

In `RegisterRoutes`, remove these three lines:
```go
mux.HandleFunc("POST /agents/yaml", h.createAgentYaml)
mux.HandleFunc("POST /agents", h.createAgentForm)
mux.HandleFunc("PUT /agents/{name}/yaml", h.saveAgentYaml)
```

Add these three lines:
```go
mux.HandleFunc("PUT /agents/{name}", h.saveAgentForm)
mux.HandleFunc("POST /agents/form", h.createAgentFormNew)
mux.HandleFunc("POST /agents/{name}/clone", h.cloneAgent)  // cloneAgent added in Task 3
```

For now add a stub for `cloneAgent` so it compiles:
```go
func (h *Handler) cloneAgent(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
```

- [ ] **Step 11: Add `save-agent-response` template to `agents.html`**

Append after the existing `{{define "save-yaml-response"}}` block:

```html
{{define "save-agent-response"}}
{{/* OOB: refresh agent list in sidebar */}}
<div id="agent-list" hx-swap-oob="true">
  {{template "agent-list-items" (dict "Agents" .Agents)}}
</div>
{{/* Main: re-render editor */}}
{{template "agent-editor" .Editor}}
{{end}}
```

- [ ] **Step 12: Build**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go build ./...
```

Expected: no errors. (Templates still reference `.RawYAML`/`.Name` which renders empty — not a compile error.)

- [ ] **Step 13: Commit**

```bash
git add internal/web/handler.go internal/web/templates/agents.html
git commit -m "feat: restructure agent editor data types and add form save/create handlers"
```

---

### Task 2: Replace `agent-editor` template with form UI

**Files:**
- Modify: `internal/web/templates/agents.html`

- [ ] **Step 1: Replace the entire `{{define "agent-editor"}}` block**

Find `{{define "agent-editor"}}` and replace everything up to and including its `{{end}}` with:

```html
{{define "agent-editor"}}
<div class="flex flex-col h-full">
  <!-- Header -->
  <div class="flex items-center justify-between px-4 py-3 border-b shrink-0">
    <div class="flex items-center gap-2">
      <label for="sidebar-toggle" class="sidebar-toggle-btn" aria-label="Toggle sidebar">
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="18" height="18" rx="2"/><path d="M9 3v18"/>
        </svg>
      </label>
      <h2 class="text-sm font-semibold">{{.Def.Name}}</h2>
    </div>
    <div class="flex items-center gap-3">
      <button
        type="submit"
        form="agent-form-{{.Def.Name}}"
        class="uk-button uk-button-primary uk-button-small"
        style="border-radius: 6px; font-size: 12px; padding: 4px 14px;"
      >Save</button>
    </div>
  </div>

  <!-- Form -->
  <div class="flex-1 overflow-y-auto p-4">
    <form
      id="agent-form-{{.Def.Name}}"
      hx-put="/agents/{{.Def.Name}}"
      hx-target="#agent-editor"
      hx-swap="innerHTML"
      class="flex flex-col gap-4"
    >
      <!-- Description -->
      <div>
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Description</label>
        <input type="text" name="description" value="{{.Def.Description}}" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="Short description of what this agent does" />
      </div>

      <!-- Model -->
      <div>
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Model</label>
        <input type="text" name="model" value="{{.Def.Model}}" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="system default" />
      </div>

      <!-- System Prompt -->
      <div class="flex flex-col">
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">System Prompt</label>
        <textarea
          name="system_prompt"
          class="uk-textarea"
          style="font-family:ui-monospace,monospace;font-size:13px;min-height:240px;resize:vertical;border-radius:6px;line-height:1.6;"
          spellcheck="false"
        >{{.Def.SystemPrompt}}</textarea>
      </div>

      <!-- Tools -->
      <div>
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">
          Tools <span class="normal-case font-normal">(one per line, e.g. <code>srvd.web_search</code>)</span>
        </label>
        <textarea
          name="tools"
          class="uk-textarea"
          style="font-family:ui-monospace,monospace;font-size:12px;min-height:72px;resize:vertical;border-radius:6px;"
          spellcheck="false"
          placeholder="srvd.web_search&#10;srvd.web_url_read"
        >{{joinLines .Def.Tools}}</textarea>
      </div>

      <!-- Max Turns / Max Concurrent Tools -->
      <div class="flex gap-4">
        <div class="flex-1">
          <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Max Turns</label>
          <input type="number" name="max_turns" value="{{if .Def.MaxTurns}}{{.Def.MaxTurns}}{{end}}" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="10" min="0" />
        </div>
        <div class="flex-1">
          <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Max Concurrent Tools</label>
          <input type="number" name="max_concurrent_tools" value="{{if .Def.MaxConcurrentTools}}{{.Def.MaxConcurrentTools}}{{end}}" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="unlimited" min="0" />
        </div>
      </div>

      <!-- Force JSON -->
      <div style="border:1px solid hsl(var(--border));border-radius:8px;padding:12px 14px;">
        <label style="display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;">
          <input type="checkbox" name="force_json" value="true" {{if .Def.ForceJSON}}checked{{end}} style="width:15px;height:15px;" />
          <span class="text-sm font-medium">Force JSON output <span class="text-xs text-muted-foreground font-normal">(json_object mode, no schema)</span></span>
        </label>
      </div>

      <!-- Structured Output -->
      <input type="hidden" name="structured_output_enabled" id="so-enabled-{{.Def.Name}}" value="{{if .StructuredOutputJSON}}true{{else}}false{{end}}" />
      <input type="hidden" name="structured_output_json" id="so-json-{{.Def.Name}}" value="{{.StructuredOutputJSON}}" />
      <div style="border:1px solid hsl(var(--border));border-radius:8px;padding:12px 14px;">
        <label style="display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;">
          <input
            type="checkbox"
            id="so-toggle-{{.Def.Name}}"
            {{if .StructuredOutputJSON}}checked{{end}}
            onchange="toggleStructuredOutput('{{.Def.Name}}')"
            style="width:15px;height:15px;"
          />
          <span class="text-sm font-medium">Enable structured output (JSON Schema)</span>
        </label>
        <div id="so-panel-{{.Def.Name}}" style="margin-top:10px;{{if eq .StructuredOutputJSON ""}}display:none;{{end}}">
          <p class="text-xs text-muted-foreground" style="margin-bottom:6px;">
            JSON object with <code>name</code>, <code>schema</code>, and optionally <code>strict</code>.
            Same object passed as <code>response_schema</code> in the run API.
          </p>
          <textarea
            id="so-textarea-{{.Def.Name}}"
            class="uk-textarea"
            style="font-family:ui-monospace,monospace;font-size:12px;min-height:140px;border-radius:6px;"
            spellcheck="false"
            placeholder='{"name": "result", "strict": true, "schema": {"type": "object", "properties": {}, "required": [], "additionalProperties": false}}'
            oninput="syncStructuredOutput('{{.Def.Name}}')"
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
  panel.style.display = checkbox.checked ? 'block' : 'none';
  enabledInput.value = checkbox.checked ? 'true' : 'false';
}
function syncStructuredOutput(name) {
  var textarea = document.getElementById('so-textarea-' + name);
  var jsonInput = document.getElementById('so-json-' + name);
  jsonInput.value = textarea.value;
}
(function() {
  var scripts = document.currentScript ? [document.currentScript] : document.getElementsByTagName('script');
  var s = scripts[scripts.length - 1];
  var container = s.closest('form') || s.parentElement;
  var textareas = container ? container.querySelectorAll('textarea[id^="so-textarea-"]') : [];
  textareas.forEach(function(ta) {
    var name = ta.id.replace('so-textarea-', '');
    var jsonInput = document.getElementById('so-json-' + name);
    if (jsonInput) jsonInput.value = ta.value;
  });
}());
</script>
{{end}}
```

- [ ] **Step 2: Build**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go build ./...
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add internal/web/templates/agents.html
git commit -m "feat: replace agent-editor YAML textarea with form fields"
```

---

### Task 3: Replace `agent-editor-new`, add Clone button, implement `cloneAgent`

**Files:**
- Modify: `internal/web/handler.go`
- Create: `internal/web/handler_test.go`
- Modify: `internal/web/templates/agents.html`

- [ ] **Step 1: Write the failing test**

Create `internal/web/handler_test.go`:

```go
package web

import (
	"testing"
)

func TestCloneAgentName_NoCopies(t *testing.T) {
	name, err := cloneAgentName("foo", func(s string) bool { return false })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "foo-copy" {
		t.Errorf("got %q, want %q", name, "foo-copy")
	}
}

func TestCloneAgentName_FirstCopyExists(t *testing.T) {
	existing := map[string]bool{"foo-copy": true}
	name, err := cloneAgentName("foo", func(s string) bool { return existing[s] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "foo-copy-2" {
		t.Errorf("got %q, want %q", name, "foo-copy-2")
	}
}

func TestCloneAgentName_ManyCopiesExist(t *testing.T) {
	// copies 1-4 exist, 5 is free
	existing := map[string]bool{
		"foo-copy":   true,
		"foo-copy-2": true,
		"foo-copy-3": true,
		"foo-copy-4": true,
	}
	name, err := cloneAgentName("foo", func(s string) bool { return existing[s] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "foo-copy-5" {
		t.Errorf("got %q, want %q", name, "foo-copy-5")
	}
}

func TestCloneAgentName_TooManyCopies(t *testing.T) {
	_, err := cloneAgentName("foo", func(s string) bool { return true })
	if err == nil {
		t.Error("expected error when all copy names are taken")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go test ./internal/web/... -run TestCloneAgentName -v
```

Expected: FAIL — `cloneAgentName` is not defined yet.

- [ ] **Step 3: Add `cloneAgentName` helper and `cloneAgent` handler in `handler.go`**

Add `cloneAgentName` as a package-level function (testable without a Handler):

```go
// cloneAgentName returns the next available clone name for src.
// exists is called to check whether a candidate name is already taken.
// Returns an error if all 10 candidate names are taken.
func cloneAgentName(src string, exists func(string) bool) (string, error) {
	candidate := src + "-copy"
	if !exists(candidate) {
		return candidate, nil
	}
	for i := 2; i <= 10; i++ {
		candidate = fmt.Sprintf("%s-copy-%d", src, i)
		if !exists(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("too many copies of %q", src)
}
```

Replace the `cloneAgent` stub with the real implementation:

```go
// cloneAgent handles POST /agents/{name}/clone.
func (h *Handler) cloneAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	src := h.store.GetDefinition(name)
	if src == nil {
		http.Error(w, "agent not found: "+name, http.StatusNotFound)
		return
	}

	cloneName, err := cloneAgentName(name, func(s string) bool {
		return h.store.GetDefinition(s) != nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	clone := *src
	clone.Name = cloneName
	if err := h.store.SaveDefinition(&clone); err != nil {
		slog.Error("failed to clone agent", "source", name, "clone", cloneName, "error", err)
		http.Error(w, "failed to clone", http.StatusInternalServerError)
		return
	}

	h.renderPartial(w, "save-agent-response", saveYamlData{
		Editor: agentEditorData{Def: &clone, StructuredOutputJSON: structuredOutputJSON(&clone)},
		Agents: h.store.ListDefinitions(),
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go test ./internal/web/... -run TestCloneAgentName -v
```

Expected: all 4 tests PASS.

- [ ] **Step 5: Replace `agent-editor-new` template in `agents.html`**

Replace the entire `{{define "agent-editor-new"}}...{{end}}` block with:

```html
{{define "agent-editor-new"}}
<div class="flex flex-col h-full">
  <!-- Header -->
  <div class="flex items-center justify-between px-4 py-3 border-b shrink-0">
    <div class="flex items-center gap-2">
      <label for="sidebar-toggle" class="sidebar-toggle-btn" aria-label="Toggle sidebar">
        <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="18" height="18" rx="2"/><path d="M9 3v18"/>
        </svg>
      </label>
      <h2 class="text-sm font-semibold">New Agent</h2>
    </div>
    <div class="flex items-center gap-3">
      <button
        type="submit"
        form="agent-form-new"
        class="uk-button uk-button-primary uk-button-small"
        style="border-radius: 6px; font-size: 12px; padding: 4px 14px;"
      >Save</button>
    </div>
  </div>

  <!-- Form -->
  <div class="flex-1 overflow-y-auto p-4">
    <form
      id="agent-form-new"
      hx-post="/agents/form"
      hx-target="#agent-editor"
      hx-swap="innerHTML"
      class="flex flex-col gap-4"
    >
      <!-- Name (editable on new) -->
      <div>
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Name <span class="text-red-500">*</span></label>
        <input type="text" name="name" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="my-agent" required />
      </div>

      <!-- Description -->
      <div>
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Description</label>
        <input type="text" name="description" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="Short description of what this agent does" />
      </div>

      <!-- Model -->
      <div>
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Model</label>
        <input type="text" name="model" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="system default" />
      </div>

      <!-- System Prompt -->
      <div class="flex flex-col">
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">System Prompt <span class="text-red-500">*</span></label>
        <textarea
          name="system_prompt"
          class="uk-textarea"
          style="font-family:ui-monospace,monospace;font-size:13px;min-height:240px;resize:vertical;border-radius:6px;line-height:1.6;"
          spellcheck="false"
          placeholder="You are a helpful assistant..."
        ></textarea>
      </div>

      <!-- Tools -->
      <div>
        <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">
          Tools <span class="normal-case font-normal">(one per line, e.g. <code>srvd.web_search</code>)</span>
        </label>
        <textarea
          name="tools"
          class="uk-textarea"
          style="font-family:ui-monospace,monospace;font-size:12px;min-height:72px;resize:vertical;border-radius:6px;"
          spellcheck="false"
          placeholder="srvd.web_search&#10;srvd.web_url_read"
        ></textarea>
      </div>

      <!-- Max Turns / Max Concurrent Tools -->
      <div class="flex gap-4">
        <div class="flex-1">
          <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Max Turns</label>
          <input type="number" name="max_turns" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="10" min="0" />
        </div>
        <div class="flex-1">
          <label class="text-xs font-medium text-muted-foreground uppercase tracking-wide" style="display:block;margin-bottom:4px;">Max Concurrent Tools</label>
          <input type="number" name="max_concurrent_tools" class="uk-input" style="border-radius:6px;font-size:13px;" placeholder="unlimited" min="0" />
        </div>
      </div>

      <!-- Force JSON -->
      <div style="border:1px solid hsl(var(--border));border-radius:8px;padding:12px 14px;">
        <label style="display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;">
          <input type="checkbox" name="force_json" value="true" style="width:15px;height:15px;" />
          <span class="text-sm font-medium">Force JSON output <span class="text-xs text-muted-foreground font-normal">(json_object mode, no schema)</span></span>
        </label>
      </div>

      <!-- Structured Output -->
      <input type="hidden" name="structured_output_enabled" id="so-enabled-new" value="false" />
      <input type="hidden" name="structured_output_json" id="so-json-new" value="" />
      <div style="border:1px solid hsl(var(--border));border-radius:8px;padding:12px 14px;">
        <label style="display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;">
          <input
            type="checkbox"
            id="so-toggle-new"
            onchange="toggleStructuredOutput('new')"
            style="width:15px;height:15px;"
          />
          <span class="text-sm font-medium">Enable structured output (JSON Schema)</span>
        </label>
        <div id="so-panel-new" style="margin-top:10px;display:none;">
          <p class="text-xs text-muted-foreground" style="margin-bottom:6px;">
            JSON object with <code>name</code>, <code>schema</code>, and optionally <code>strict</code>.
            Same object passed as <code>response_schema</code> in the run API.
          </p>
          <textarea
            id="so-textarea-new"
            class="uk-textarea"
            style="font-family:ui-monospace,monospace;font-size:12px;min-height:140px;border-radius:6px;"
            spellcheck="false"
            placeholder='{"name": "result", "strict": true, "schema": {"type": "object", "properties": {}, "required": [], "additionalProperties": false}}'
            oninput="syncStructuredOutput('new')"
          ></textarea>
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
  panel.style.display = checkbox.checked ? 'block' : 'none';
  enabledInput.value = checkbox.checked ? 'true' : 'false';
}
function syncStructuredOutput(name) {
  var textarea = document.getElementById('so-textarea-' + name);
  var jsonInput = document.getElementById('so-json-' + name);
  jsonInput.value = textarea.value;
}
</script>
{{end}}
```

- [ ] **Step 6: Add Clone button to `agent-list-items` template**

In the `{{define "agent-list-items"}}` block, the agent row currently has:
```html
<button
  class="del-btn uk-button uk-button-danger uk-button-small text-xs ml-2"
  style="padding: 1px 8px; border-radius: 5px;"
  hx-delete="/agents/{{.Name}}"
  hx-target="#agent-list"
  hx-swap="innerHTML"
  hx-confirm="Delete agent {{.Name}}?"
>Del</button>
```

Replace it with (Clone button added before Del, both hidden until hover):
```html
<button
  class="del-btn uk-button uk-button-default uk-button-small text-xs ml-1"
  style="padding: 1px 8px; border-radius: 5px;"
  hx-post="/agents/{{.Name}}/clone"
  hx-target="#agent-editor"
  hx-swap="innerHTML"
>Clone</button>
<button
  class="del-btn uk-button uk-button-danger uk-button-small text-xs ml-1"
  style="padding: 1px 8px; border-radius: 5px;"
  hx-delete="/agents/{{.Name}}"
  hx-target="#agent-list"
  hx-swap="innerHTML"
  hx-confirm="Delete agent {{.Name}}?"
>Del</button>
```

Both buttons use `class="del-btn"` which already has the CSS opacity-0/opacity-1 on hover behaviour defined in the `<style>` block at the top of `agents.html`.

- [ ] **Step 7: Build and run all tests**

```bash
cd /home/angoo/repos/opendev/opendev-agents
go build ./...
go test ./...
```

Expected: build clean, all tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/web/handler.go internal/web/handler_test.go internal/web/templates/agents.html
git commit -m "feat: add agent-editor-new form, clone button, and clone handler"
```
