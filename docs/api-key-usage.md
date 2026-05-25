# Using AgentFoundry with an API Key

External consumers access AgentFoundry through `agentfoundry-ui`. All API
requests target the UI host and use a user-scoped API key for authentication.

The UI proxies all `/api/v1/` paths to the backend transparently.

## Creating an API Key

Provision API keys through the agentfoundry-ui web interface. The full key is
shown **once** on creation — copy it immediately.

## Authentication

Every request must include the API key as a Bearer token:

```
Authorization: Bearer afk_<key>
```

## API Reference

Replace `$UI_HOST` with your agentfoundry-ui hostname and `$KEY` with your
`afk_...` key.

> **Agent IDs vs Names:** Agent runs use the **AgentID** (a 32-character hex
> string shown on each agent card in the UI). List/Create/Update/Delete
> endpoints use the human-readable **name**. Run the list endpoint to find both.

### List Agents

```bash
curl -s -H "Authorization: Bearer $KEY" \
  "$UI_HOST/api/v1/agents"
```

The response includes `agent_id` (for runs) and `name` (for CRUD).

### Start a Run

```bash
curl -s -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"message": "Explain quantum computing in one sentence."}' \
  "$UI_HOST/api/v1/agents/<agent-id>/run"
```

Returns `202` with the run ID:

```json
{"run_id": "1748012100123456789"}
```

### Pass Conversation History

Include prior turns to maintain context across runs:

```bash
curl -s -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "message": "What did I just ask about?",
    "history": [
      {"role": "user", "content": "Explain quantum computing"},
      {"role": "assistant", "content": "Quantum computing uses qubits..."}
    ]
  }' \
  "$UI_HOST/api/v1/agents/<agent-id>/run"
```

The `history` field accepts an array of `{role, content}` objects. Valid roles: `user`, `assistant`, `system`, `tool`.

### Poll for Results

```bash
curl -s -H "Authorization: Bearer $KEY" \
  "$UI_HOST/api/v1/runs/<run_id>"
```

While running:

```json
{"id": "...", "agent": "...", "status": "running", "created_at": "..."}
```

When complete:

```json
{"id": "...", "agent": "...", "status": "completed", "response": "...", "created_at": "..."}
```

On failure:

```json
{"id": "...", "agent": "...", "status": "failed", "error": "...", "created_at": "..."}
```

### Cancel a Run

```bash
curl -s -X POST \
  -H "Authorization: Bearer $KEY" \
  "$UI_HOST/api/v1/runs/<run_id>/cancel"
```

### Streaming Responses (SSE)

For real-time token-by-token output, use the chat session + SSE flow:

**1. Create a session:**

```bash
curl -s -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"agent_name": "<name>"}' \
  "$UI_HOST/api/v1/chat/sessions"
```

Returns the session ID:

```json
{"id": "...", "agent_name": "...", "messages": [], ...}
```

**2. Send a message (starts the run):**

```bash
curl -s -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"message": "Explain quantum computing"}' \
  "$UI_HOST/api/v1/chat/sessions/<session_id>/messages"
```

Returns the run ID:

```json
{"run_id": "..."}
```

**3. Stream events:**

```bash
curl -sN -H "Authorization: Bearer $KEY" \
  "$UI_HOST/api/v1/chat/runs/<run_id>/events"
```

The SSE stream emits these event types:

- `token` — one token of the response
- `status` — status updates (e.g. "Thinking...")
- `done`  — response complete, data contains the full response
- `error` — run failed, data contains the error

Example output:

```
event: status
data: Thinking...

event: token
data: Quantum

event: token
data:  computing

event: done
data: Quantum computing uses qubits...
```

**Full streaming script:**

```bash
#!/usr/bin/env bash
set -euo pipefail

UI_HOST="${UI_HOST:-https://ui.agentfoundry.example.com}"
KEY="${API_KEY:?set API_KEY to your afk_... key}"
AGENT="${1:?usage: $0 <agent-name> <message>}"
MESSAGE="${2:?usage: $0 <agent-name> <message>}"

# Create session
session_id=$(curl -sf -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "$(jq -nc --arg name "$AGENT" '{agent_name: $name}')" \
  "$UI_HOST/api/v1/chat/sessions" | jq -r '.id')

# Send message to start run
run_id=$(curl -sf -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "$(jq -nc --arg msg "$MESSAGE" '{message: $msg}')" \
  "$UI_HOST/api/v1/chat/sessions/$session_id/messages" | jq -r '.run_id')

# Stream events
curl -sN -H "Authorization: Bearer $KEY" \
  "$UI_HOST/api/v1/chat/runs/$run_id/events"
```

## Complete Example (Polling)

```bash
#!/usr/bin/env bash
set -euo pipefail

UI_HOST="${UI_HOST:-https://ui.agentfoundry.example.com}"
KEY="${API_KEY:?set API_KEY to your afk_... key}"
AGENT_ID="${1:?usage: $0 <agent-id> <message>}"
MESSAGE="${2:?usage: $0 <agent-id> <message>}"

# Start the run (uses AgentID — find it with the list endpoint or on the agent card in the UI)
run_id=$(curl -sf -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "$(jq -nc --arg msg "$MESSAGE" '{message: $msg}')" \
  "$UI_HOST/api/v1/agents/$AGENT_ID/run" | jq -r '.run_id')

echo "Run started: $run_id"

# Poll until done
while true; do
  result=$(curl -sf -H "Authorization: Bearer $KEY" \
    "$UI_HOST/api/v1/runs/$run_id")
  status=$(echo "$result" | jq -r '.status')

  case "$status" in
    completed)
      echo "$result" | jq -r '.response'
      break
      ;;
    failed)
      echo "Error: $(echo "$result" | jq -r '.error')" >&2
      exit 1
      ;;
    *)
      sleep 1
      ;;
  esac
done
```

Save as `run-agent.sh`, then:

```bash
chmod +x run-agent.sh
API_KEY=afk_... ./run-agent.sh a1b2c3d4e5f67890abcdef1234567890 "Tell me a joke"
```
