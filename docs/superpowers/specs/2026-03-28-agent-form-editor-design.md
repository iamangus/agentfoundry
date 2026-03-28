# Agent Form Editor Design

**Date:** 2026-03-28
**Status:** Approved

## Overview

Replace the raw YAML editor in the agent edit UI with a proper form that gives each `Definition` field its own dedicated input. Add a hover-visible Clone button to each sidebar agent row that duplicates an agent and opens the copy in the editor.

## Form Fields

| Field | Input | Notes |
|---|---|---|
| `name` | text input | Readonly on edit; editable on new agent |
| `description` | text input | Optional |
| `model` | text input | Optional; placeholder indicates system default is used when blank |
| `system_prompt` | large resizable textarea | Primary usability improvement |
| `tools` | textarea | One tool per line (e.g. `srvd.web_search`); blank lines ignored |
| `max_turns` | number input | Optional; blank = server default (10) |
| `max_concurrent_tools` | number input | Optional; blank = unlimited |
| `force_json` | checkbox | |
| `structured_output` | toggle + JSON textarea | Existing implementation, unchanged |

`kind` is always `"agent"` and is not shown in the form — set implicitly on save.

## Save Endpoint

The form submits as JSON to a new web route:

```
PUT /agents/{name}
Content-Type: application/json
```

Request body matches `config.Definition` JSON tags. The handler:
1. Decodes JSON into `config.Definition`
2. Sets `Kind = "agent"` implicitly
3. Splits `tools` from newline-delimited string to `[]string` (client sends newline-delimited; server splits on `\n`, trims whitespace, drops blanks)
4. Calls `def.Validate()`
5. Calls `store.SaveDefinition(&def)`
6. Returns the updated editor partial + OOB sidebar refresh

The existing `PUT /agents/{name}/yaml` raw endpoint is unchanged and continues to work for programmatic/API use.

## New Agent Form

The "New Agent" editor also uses the form layout. On create, the form POSTs to `POST /agents` with JSON body (existing endpoint already accepts `config.Definition` JSON — no change needed to the API handler; a new web-layer route `POST /agents/form` maps to the same save logic).

Actually: the existing web `POST /agents/yaml` is replaced by `POST /agents/form` for the UI path, which accepts JSON and uses `store.SaveDefinition`. The `POST /agents/yaml` route remains for programmatic YAML submission.

## Clone

A **Clone** button appears on hover on each agent sidebar row, matching the existing Del button pattern (`.del-btn` opacity-0, opacity-1 on row hover).

```
POST /agents/{name}/clone
```

Handler:
1. Loads the source definition
2. Generates a new name: `{name}-copy`; if that exists, try `{name}-copy-2`, `{name}-copy-3`, up to 10 attempts, then error
3. Saves the cloned definition via `store.SaveDefinition`
4. Returns the cloned agent's editor partial + OOB sidebar refresh (same response shape as save)

The cloned agent opens immediately in the editor.

## Template Changes

- `agent-editor` template: replace YAML textarea with form inputs as described
- `agent-editor-new` template: same form layout, name field editable
- `agent-list-items` template: add Clone button alongside Del button, same hover pattern

## Out of Scope

- Raw YAML export/import from the UI (still available via API)
- Per-field validation error display beyond the existing HTTP error approach
- Tools picker UI (users type tool names manually, same as before)
