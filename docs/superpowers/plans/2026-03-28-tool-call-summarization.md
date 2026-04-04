# Tool Call Summarization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add optional async tool call summarization via a user-defined agent, force-JSON output mode for agents, and surface summaries in the run history UI.

**Architecture:** A `force_json` flag on `Definition` sets `response_format` on LLM requests. A `summary_agent` name in `SystemConfig` (or env var) names an agent that receives a formatted prompt after each tool call completes and returns `{"reason":"...","outcome":"..."}`. Summarization runs in a detached goroutine with `context.Background()`, patches `ToolCallRecord.Summary` via a new `HistoryManager` method, and is displayed in the conversation view UI.

**Tech Stack:** Go 1.25, OpenAI-compatible REST API, Go `html/template`, vanilla JS (existing pattern)

---

## File Map

| File | Change |
|------|--------|
| `internal/config/definition.go` | Add `ForceJSON bool` field |
| `internal/llm/client.go` | Add `ResponseFormat` type + field on `ChatRequest` |
| `internal/agent/agent.go` | Apply `ResponseFormat` when `def.ForceJSON` is true |
| `internal/config/system.go` | Add `SummaryAgent string` field + env var fallback |
| `internal/api/runhistory.go` | Add `ToolCallSummary` struct, `Summary` field on `ToolCallRecord`, `SetToolCallSummary` method |
| `internal/api/http.go` | Give adapter access to runtime + summary agent name; fire async summarize goroutine |
| `internal/web/templates/runs.html` | Embed tool call records on `.conv-turn`; render summary in JS |

---

### Task 1: Add `force_json` to agent Definition and wire it into LLM requests

**Files:**
- Modify: `internal/config/definition.go`
- Modify: `internal/llm/client.go`
- Modify: `internal/agent/agent.go`

- [ ] **Step 1: Add `ForceJSON` to `Definition`**

In `internal/config/definition.go`, add the field after `MaxConcurrentTools`:

```go
type Definition struct {
	Kind               Kind     `yaml:"kind" json:"kind"`
	Name               string   `yaml:"name" json:"name"`
	Description        string   `yaml:"description" json:"description"`
	Model              string   `yaml:"model,omitempty" json:"model,omitempty"`
	SystemPrompt       string   `yaml:"system_prompt" json:"system_prompt"`
	Tools              []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	MaxTurns           int      `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxConcurrentTools int      `yaml:"max_concurrent_tools,omitempty" json:"max_concurrent_tools,omitempty"`
	ForceJSON          bool     `yaml:"force_json,omitempty" json:"force_json,omitempty"`
}
```

- [ ] **Step 2: Add `ResponseFormat` type and field to `ChatRequest`**

In `internal/llm/client.go`, add the type and field:

```go
// ResponseFormat instructs the model to produce output in a specific format.
type ResponseFormat struct {
	Type string `json:"type"` // "json_object"
}
```

Add the field to `ChatRequest`:

```go
type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []ToolDef       `json:"tools,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}
```

- [ ] **Step 3: Apply `ResponseFormat` in the agent loop**

In `internal/agent/agent.go`, inside `RunWithHistory`, update the request construction (around line 151):

```go
req := &llm.ChatRequest{
    Model:    def.Model,
    Messages: messages,
}
if len(toolDefs) > 0 {
    req.Tools = toolDefs
}
if def.ForceJSON {
    req.ResponseFormat = &llm.ResponseFormat{Type: "json_object"}
}
```

- [ ] **Step 4: Verify it builds**

```bash
cd /home/angoo/repos/opendev/opendev-agents && go build ./...
```

Expected: no errors.

- [ ] **Step 5: Commit**

```bash
git add internal/config/definition.go internal/llm/client.go internal/agent/agent.go
git commit -m "feat: add force_json flag to agent definition for JSON response mode"
```

---

### Task 2: Add `summary_agent` to system config

**Files:**
- Modify: `internal/config/system.go`

- [ ] **Step 1: Add `SummaryAgent` field to `SystemConfig`**

In `internal/config/system.go`, add the field to `SystemConfig`:

```go
type SystemConfig struct {
	Listen         string                   `yaml:"listen"`
	DefinitionsDir string                   `yaml:"definitions_dir"`
	LLM            LLMConf                  `yaml:"llm"`
	MCPServers     []mcpclient.ServerConfig `yaml:"mcp_servers"`
	SummaryAgent   string                   `yaml:"summary_agent,omitempty"`
	OpenRouter     *OpenRouterConf          `yaml:"openrouter,omitempty"`
}
```

- [ ] **Step 2: Apply env var fallback in `LoadSystem`**

At the end of `LoadSystem`, before the `return cfg, nil` line, add:

```go
// Apply env var fallback for summary agent.
if cfg.SummaryAgent == "" {
    cfg.SummaryAgent = os.Getenv("TOOL_SUMMARY_AGENT")
}
```

- [ ] **Step 3: Verify it builds**

```bash
cd /home/angoo/repos/opendev/opendev-agents && go build ./...
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/config/system.go
git commit -m "feat: add summary_agent config key and TOOL_SUMMARY_AGENT env var fallback"
```

---

### Task 3: Add `ToolCallSummary` to run history and `SetToolCallSummary` method

**Files:**
- Modify: `internal/api/runhistory.go`

- [ ] **Step 1: Add `ToolCallSummary` struct and `Summary` field to `ToolCallRecord`**

In `internal/api/runhistory.go`, add the struct and update `ToolCallRecord`:

```go
// ToolCallSummary holds the AI-generated summary of a tool call.
type ToolCallSummary struct {
	Reason  string `json:"reason"`
	Outcome string `json:"outcome"`
}

type ToolCallRecord struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Arguments string           `json:"arguments"`
	Result    string           `json:"result,omitempty"`
	Status    ToolCallStatus   `json:"status"`
	Error     string           `json:"error,omitempty"`
	StartedAt time.Time        `json:"started_at"`
	Duration  time.Duration    `json:"duration"`
	Summary   *ToolCallSummary `json:"summary,omitempty"`
}
```

- [ ] **Step 2: Add `SetToolCallSummary` to `HistoryManager`**

Add this method to `HistoryManager` in `internal/api/runhistory.go`:

```go
// SetToolCallSummary finds the tool call record by ID (searching all turns)
// and sets its Summary field. Safe to call from a goroutine after the turn
// has been committed.
func (m *HistoryManager) SetToolCallSummary(runID, toolCallID string, s ToolCallSummary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.runs[runID]
	if !ok {
		return
	}
	for i := range h.Turns {
		for j := range h.Turns[i].ToolCalls {
			if h.Turns[i].ToolCalls[j].ID == toolCallID {
				h.Turns[i].ToolCalls[j].Summary = &s
				return
			}
		}
	}
}
```

- [ ] **Step 3: Verify it builds**

```bash
cd /home/angoo/repos/opendev/opendev-agents && go build ./...
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/api/runhistory.go
git commit -m "feat: add ToolCallSummary type and SetToolCallSummary to history manager"
```

---

### Task 4: Wire async summarization into the history recorder adapter

**Files:**
- Modify: `internal/api/http.go`

- [ ] **Step 1: Add runtime and summaryAgentName to the adapter**

In `internal/api/http.go`, update `historyRecorderAdapter`:

```go
type historyRecorderAdapter struct {
	hm               *HistoryManager
	runID            string
	runtime          *agent.Runtime
	summaryAgentName string
}
```

- [ ] **Step 2: Add required imports**

Ensure the import block in `internal/api/http.go` includes `"context"`, `"encoding/json"`, and `"strings"`. The file already has `"context"` and `"encoding/json"`. Add `"strings"` if not present.

Check existing imports at top of file and add `"strings"` to the import block if missing:

```go
import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/angoo/agentfile/internal/agent"
	"github.com/angoo/agentfile/internal/config"
	"github.com/angoo/agentfile/internal/llm"
	"github.com/angoo/agentfile/internal/mcpclient"
	"github.com/angoo/agentfile/internal/registry"
)
```

- [ ] **Step 3: Add `truncate` helper**

Add this unexported helper near the bottom of `internal/api/http.go` (before `writeJSON`):

```go
// truncateStr shortens s to at most n bytes, appending "…" if truncated.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
```

- [ ] **Step 4: Fire async summarization in `EndToolCall`**

Update `EndToolCall` on the adapter in `internal/api/http.go`:

```go
func (a *historyRecorderAdapter) EndToolCall(result string, status agent.ToolCallStatus, errMsg string) {
	var s ToolCallStatus
	switch status {
	case agent.ToolCallStatusSuccess:
		s = ToolCallStatusSuccess
	case agent.ToolCallStatusError:
		s = ToolCallStatusError
	}
	a.hm.EndToolCall(a.runID, result, s, errMsg)

	// Fire async summarization if configured.
	if a.summaryAgentName == "" || a.runtime == nil {
		return
	}
	// Capture values needed by the goroutine before it runs.
	// We need the tool call ID — grab it from the just-committed record.
	var toolCallID, toolName, arguments string
	func() {
		a.hm.mu.RLock()
		defer a.hm.mu.RUnlock()
		h, ok := a.hm.runs[a.runID]
		if !ok || len(h.Turns) == 0 {
			return
		}
		lastTurn := h.Turns[len(h.Turns)-1]
		if len(lastTurn.ToolCalls) == 0 {
			return
		}
		last := lastTurn.ToolCalls[len(lastTurn.ToolCalls)-1]
		toolCallID = last.ID
		toolName = last.Name
		arguments = last.Arguments
	}()
	if toolCallID == "" {
		return
	}
	runID := a.runID
	hm := a.hm
	rt := a.runtime
	agentName := a.summaryAgentName
	resultSnap := result

	go func() {
		def, ok := rt.GetAgentDef(agentName)
		if !ok {
			return
		}
		prompt := "Tool: " + toolName +
			"\nArguments: " + truncateStr(arguments, 500) +
			"\nResult: " + truncateStr(resultSnap, 1000)
		resp, err := rt.Run(context.Background(), def, prompt)
		if err != nil {
			return
		}
		// Strip markdown code fences if the model wrapped the JSON.
		resp = strings.TrimSpace(resp)
		if strings.HasPrefix(resp, "```") {
			if idx := strings.Index(resp[3:], "\n"); idx >= 0 {
				resp = resp[3+idx+1:]
			}
			resp = strings.TrimSuffix(resp, "```")
			resp = strings.TrimSpace(resp)
		}
		var summary ToolCallSummary
		if err := json.Unmarshal([]byte(resp), &summary); err != nil {
			return
		}
		hm.SetToolCallSummary(runID, toolCallID, summary)
	}()
}
```

- [ ] **Step 5: Add `GetAgentDef` method to `agent.Runtime`**

The goroutine calls `rt.GetAgentDef(agentName)` but `Runtime` doesn't expose that. Add a thin method in `internal/agent/agent.go`:

```go
// GetAgentDef resolves an agent definition by name.
func (rt *Runtime) GetAgentDef(name string) (*config.Definition, bool) {
	return rt.resolver.GetAgentDef(name)
}
```

- [ ] **Step 6: Pass runtime and summaryAgentName when creating the adapter in `runAgent`**

In `internal/api/http.go`, update the adapter construction inside the goroutine in `runAgent` (around line 239):

```go
hr := &historyRecorderAdapter{
    hm:               h.history,
    runID:            runID,
    runtime:          h.agentRuntime,
    summaryAgentName: h.summaryAgentName,
}
```

- [ ] **Step 7: Add `summaryAgentName` field to `Handler` and set it in `NewHandler`**

Update `Handler` struct:

```go
type Handler struct {
	store            DefinitionStore
	reg              *registry.Registry
	pool             *mcpclient.Pool
	agentRuntime     *agent.Runtime
	runs             *RunManager
	history          *HistoryManager
	summaryAgentName string
}
```

Update `NewHandler` signature and body:

```go
func NewHandler(reg *registry.Registry, pool *mcpclient.Pool, store DefinitionStore, agentRuntime *agent.Runtime, history *HistoryManager, summaryAgentName string) *Handler {
	return &Handler{
		store:            store,
		reg:              reg,
		pool:             pool,
		agentRuntime:     agentRuntime,
		runs:             NewRunManager(),
		history:          history,
		summaryAgentName: summaryAgentName,
	}
}
```

- [ ] **Step 8: Find where `NewHandler` is called in `cmd/` and pass the summary agent name**

```bash
grep -rn "NewHandler" /home/angoo/repos/opendev/opendev-agents/cmd/
```

Open that file, find the `NewHandler` call, and add `cfg.SummaryAgent` as the final argument. For example if the call is in `cmd/agentfile/main.go`:

```go
apiHandler := api.NewHandler(reg, pool, store, agentRuntime, history, cfg.SummaryAgent)
```

- [ ] **Step 9: Verify it builds**

```bash
cd /home/angoo/repos/opendev/opendev-agents && go build ./...
```

Expected: no errors.

- [ ] **Step 10: Commit**

```bash
git add internal/agent/agent.go internal/api/http.go
git commit -m "feat: wire async tool call summarization into history recorder adapter"
```

---

### Task 5: Surface tool call summaries in the UI

**Files:**
- Modify: `internal/web/templates/runs.html`

The conversation view builds tool calls from the raw LLM response JSON, which doesn't contain summaries. Summaries exist on `ToolCallRecord` (which is on the turn). We thread them through by embedding the turn's `ToolCallRecord` slice as a JSON attribute on the `.conv-turn` element, then matching by tool call ID in JS.

- [ ] **Step 1: Embed tool call records on `.conv-turn` in the conversation view**

In `internal/web/templates/runs.html`, find the `{{range $i, $turn := .Run.Turns}}` block inside `<!-- Conversation View -->` (around line 456). The current `.conv-turn` div is:

```html
<div class="conv-turn" data-turn="{{$turn.TurnNumber}}" data-request="{{$turn.Request}}" data-response="{{$turn.Response}}">
```

Replace it with (add `data-tool-records`):

```html
<div class="conv-turn" data-turn="{{$turn.TurnNumber}}" data-request="{{$turn.Request}}" data-response="{{$turn.Response}}" data-tool-records="{{jsonAttr $turn.ToolCalls}}">
```

- [ ] **Step 2: Register `jsonAttr` template function**

Find where the web templates are parsed/registered in `internal/web/handler.go`. Add a `jsonAttr` func to the template FuncMap that marshals a value to JSON and HTML-escapes it for use in a data attribute:

```go
import (
    "encoding/json"
    "html/template"
    // ... existing imports
)

// in the FuncMap:
"jsonAttr": func(v any) string {
    b, err := json.Marshal(v)
    if err != nil {
        return "[]"
    }
    return string(b)
},
```

- [ ] **Step 3: Add summary styles**

Inside the `<style>` block in `runs.html`, add:

```css
.conv-tool-summary {
  padding: 4px 12px 6px 12px;
  font-size: 11px;
  color: hsl(var(--muted-foreground));
  font-style: italic;
  line-height: 1.5;
  border-top: 1px solid hsl(var(--border) / 0.4);
}
.conv-tool-summary-reason::before { content: 'Why: '; font-weight: 600; font-style: normal; }
.conv-tool-summary-outcome::before { content: 'Result: '; font-weight: 600; font-style: normal; }
```

- [ ] **Step 4: Update `renderConversationView` in the JS to use tool records for summaries**

In the `conversation-script` template JS, update `renderConversationView` to read `data-tool-records` and match summaries by tool call ID:

```js
function renderConversationView(turnEl) {
  var requestJson = turnEl.dataset.request;
  var responseJson = turnEl.dataset.response;
  var toolRecordsJson = turnEl.dataset.toolRecords || '[]';
  var body = turnEl.querySelector('.conv-turn-body');
  if (!body) return;

  // Build a map from tool call ID -> summary
  var summaryMap = {};
  try {
    var records = JSON.parse(toolRecordsJson);
    records.forEach(function(r) {
      if (r.id && r.summary) summaryMap[r.id] = r.summary;
    });
  } catch(e) {}

  var html = '';

  var assistantContent = extractAssistantContent(responseJson);
  if (assistantContent) {
    html += '<div class="conv-message">';
    html += '<div class="conv-message-label">🤖 Assistant</div>';
    html += '<div class="conv-message-content">' + escapeHtml(assistantContent) + '</div>';
    html += '</div>';
  }

  var toolCalls = extractToolCalls(responseJson);
  if (toolCalls.length > 0) {
    html += '<div class="conv-message">';
    html += '<div class="conv-message-label">🔧 Tool Calls (' + toolCalls.length + ')</div>';

    for (var i = 0; i < toolCalls.length; i++) {
      var tc = toolCalls[i];
      var func = tc.function || {};
      var name = func.name || 'unknown';
      var args = func.arguments || '{}';
      var tcID = tc.id || '';
      var summary = summaryMap[tcID] || null;

      html += '<div class="conv-tool-call" onclick="this.classList.toggle(\'open\')">';
      html += '<div class="conv-tool-header">';
      html += '<span class="conv-tool-name">' + escapeHtml(name) + '</span>';
      html += '<div class="conv-tool-meta">';
      html += '<svg class="conv-tool-chevron w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">';
      html += '<polyline points="9 18 15 12 9 6"></polyline>';
      html += '</svg>';
      html += '</div></div>';

      if (summary) {
        html += '<div class="conv-tool-summary">';
        if (summary.reason) html += '<div class="conv-tool-summary-reason">' + escapeHtml(summary.reason) + '</div>';
        if (summary.outcome) html += '<div class="conv-tool-summary-outcome">' + escapeHtml(summary.outcome) + '</div>';
        html += '</div>';
      }

      html += '<div class="conv-tool-body">';
      html += '<div class="conv-tool-args">';
      html += '<div class="conv-tool-section-label">Arguments</div>';
      html += '<div class="conv-tool-section-content">' + escapeHtml(formatToolArgs(args)) + '</div>';
      html += '</div>';
      html += '</div></div>';
    }

    html += '</div>';
  }

  body.innerHTML = html;
}
```

- [ ] **Step 5: Verify it builds and templates parse**

```bash
cd /home/angoo/repos/opendev/opendev-agents && go build ./...
```

Expected: no errors.

- [ ] **Step 6: Smoke test manually**

Start the server, run an agent that makes tool calls, open `/runs`, select the run, view the conversation tab. If `summary_agent` is not configured, tool calls render as before. If configured and the summary agent has responded, `Why:` and `Result:` lines appear below the tool name.

- [ ] **Step 7: Commit**

```bash
git add internal/web/templates/runs.html internal/web/handler.go
git commit -m "feat: display tool call summaries in conversation view UI"
```

---

## Self-Review

**Spec coverage:**
- ✅ `force_json` flag on agent Definition → Task 1
- ✅ `response_format` on LLM request → Task 1
- ✅ `summary_agent` config + env var → Task 2
- ✅ `ToolCallSummary` data model → Task 3
- ✅ Async summarization with `context.Background()` → Task 4
- ✅ JSON parsing of summary response → Task 4 (with markdown fence stripping)
- ✅ Summary stored in `ToolCallRecord` → Tasks 3+4
- ✅ UI display of reason/outcome → Task 5
- ✅ `force_json: true` usable on the summary agent definition → Task 1 (all agents benefit)

**Type consistency check:**
- `ToolCallSummary` defined in Task 3, used in Task 4 (`hm.SetToolCallSummary`) and Task 5 (JS `summary.reason`/`summary.outcome`) ✅
- `truncateStr` defined in Task 4 Step 3, called in Task 4 Step 4 ✅
- `GetAgentDef` added to `Runtime` in Task 4 Step 5, called in Task 4 Step 4 ✅
- `NewHandler` signature change in Task 4 Step 7, call site updated in Task 4 Step 8 ✅
- `jsonAttr` registered in Task 5 Step 2, used in template in Task 5 Step 1 ✅

**Placeholder scan:** None found.
