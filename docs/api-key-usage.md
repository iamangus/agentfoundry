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

### List Agents

```bash
curl -s -H "Authorization: Bearer $KEY" \
  "$UI_HOST/api/v1/agents"
```

### Start a Run

```bash
curl -s -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"message": "Explain quantum computing in one sentence."}' \
  "$UI_HOST/api/v1/agents/<name>/run"
```

Returns `202` with the run ID:

```json
{"run_id": "1748012100123456789"}
```

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

## Complete Example

```bash
#!/usr/bin/env bash
set -euo pipefail

UI_HOST="${UI_HOST:-https://ui.agentfoundry.example.com}"
KEY="${API_KEY:?set API_KEY to your afk_... key}"
AGENT="${1:?usage: $0 <agent-name> <message>}"
MESSAGE="${2:?usage: $0 <agent-name> <message>}"

# Start the run
run_id=$(curl -sf -X POST \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d "$(jq -nc --arg msg "$MESSAGE" '{message: $msg}')" \
  "$UI_HOST/api/v1/agents/$AGENT/run" | jq -r '.run_id')

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
API_KEY=afk_... ./run-agent.sh my-agent "Tell me a joke"
```
