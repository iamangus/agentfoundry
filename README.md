# agentfile

A system for defining and running AI agents via YAML configuration. Agents are backed by any OpenAI-compatible LLM API (OpenRouter, OpenAI, Ollama, Together AI, Azure OpenAI, etc.) and have access to tools discovered from external MCP servers. Each agent is exposed as its own MCP server, so agents can be composed — one agent can call another as a tool.

## Concepts

**Agent** — A YAML definition that combines a system prompt, an LLM model, and a set of tools. When called, the agent runs a multi-turn LLM conversation, invoking tools as needed, and returns a final response.

**Tool** — A capability discovered from an external MCP server. Tools are referenced by agents using namespaced syntax: `server.tool` (e.g. `srvd.searxng_web_search`).

**MCP Server** — Each agent is exposed as a Streamable HTTP MCP server at `/servers/{agent-name}`. A default server at `/servers/default` exposes all discovered tools. External MCP clients (Claude Desktop, Cursor, etc.) can connect to any of these endpoints.

**Composability** — An agent can list another agent in its `tools:` section. When called, the sub-agent runs its own LLM loop and returns a response, enabling multi-agent workflows.

## Quick Start

### Prerequisites

- Go 1.21+
- An API key for any OpenAI-compatible provider ([OpenRouter](https://openrouter.ai/), [OpenAI](https://platform.openai.com/), [Together AI](https://www.together.ai/), or a local [Ollama](https://ollama.com/) instance)

### Build and Run

```bash
go build -o agentfile ./cmd/agentfile/
export OPENROUTER_API_KEY="sk-or-..."   # or any OpenAI-compatible API key
./agentfile
```

### Docker

```bash
docker build -t agentfile .
docker run -p 3000:3000 \
  -e OPENROUTER_API_KEY="sk-or-..." \
  -v $(pwd)/data:/data \
  agentfile
```

The container stores all persistent data under `/data` (definitions and config). Mount a volume or bind-mount there to persist agent definitions and provide your own `agentfile.yaml`. The default definitions are baked into the image at `/data/definitions/`.

## Configuration

### agentfile.yaml

```yaml
listen: ":3000"
definitions_dir: "./definitions"

# Works with any OpenAI-compatible API
llm:
  base_url: "https://openrouter.ai/api/v1"   # or https://api.openai.com/v1, etc.
  api_key: "${OPENROUTER_API_KEY}"
  default_model: "openai/gpt-4o"
  headers:
    HTTP-Referer: "https://github.com/angoo/agentfile"
    X-Title: "agentfile"

mcp_servers:
  - name: "srvd"
    url: "https://mcp.srvd.dev/mcp"
    transport: "streamable-http"

  - name: "filesystem"
    url: "http://localhost:4000/sse"
    transport: "sse"
```

### Agent Definition

Agent definitions are YAML files in the `definitions/` directory. They are hot-reloaded when changed.

```yaml
kind: agent
name: researcher
description: "Researches topics by searching the web and summarizing findings"
model: openai/gpt-4o
system_prompt: |
  You are a research assistant. Search the web for information
  and produce well-organized research briefs.
tools:
  - srvd.searxng_web_search    # tool from external MCP server
  - srvd.web_url_read          # tool from external MCP server
  - summarizer                  # another agent used as a tool
max_turns: 15
```

## API

### Agents

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/agents` | List all agents |
| `GET` | `/api/v1/agents/{name}` | Get agent definition |
| `POST` | `/api/v1/agents` | Create agent (persisted to YAML) |
| `DELETE` | `/api/v1/agents/{name}` | Delete agent |
| `POST` | `/api/v1/agents/{name}/run` | Run an agent |

### Tools & Status

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/tools` | List all discovered tools |
| `GET` | `/api/v1/status` | Agents, tools, and connected MCP servers |
| `GET` | `/health` | Health check |

### MCP Servers

| Endpoint | Description |
|----------|-------------|
| `/servers/default` | MCP server exposing all discovered tools |
| `/servers/{agent-name}` | MCP server scoped to that agent's tools |

All MCP servers use the Streamable HTTP transport (POST/GET/DELETE on a single endpoint).

### Running an Agent

```bash
curl -s -X POST http://localhost:3000/api/v1/agents/researcher/run \
  -H "Content-Type: application/json" \
  -d '{"message": "What is the MCP protocol?"}' | jq
```

```json
{
  "agent": "researcher",
  "response": "The Model Context Protocol (MCP) is..."
}
```

### Creating an Agent via API

```bash
curl -s -X POST http://localhost:3000/api/v1/agents \
  -H "Content-Type: application/json" \
  -d '{
    "kind": "agent",
    "name": "greeter",
    "description": "A friendly greeting agent",
    "model": "openai/gpt-4o-mini",
    "system_prompt": "You are a friendly greeter. Say hello to everyone.",
    "max_turns": 3
  }'
```

### Connecting an MCP Client

Point any MCP client at an agent's endpoint:

```
http://localhost:3000/servers/researcher
```

The client will see only the tools that agent has access to.

## Project Structure

```
agentfile/
├── cmd/agentfile/main.go       # Daemon entrypoint
├── internal/
│   ├── config/                 # System config, agent definitions, YAML loader
│   ├── registry/               # Agent definition store
│   ├── mcpclient/              # MCP client pool (connects to external servers)
│   ├── agent/                  # Agent runtime (LLM conversation loop)
│   ├── mcp/                    # Per-agent Streamable HTTP MCP servers
│   ├── llm/                    # OpenAI-compatible LLM client
│   └── api/                    # REST API
├── definitions/                # Agent YAML definitions (hot-reloaded)
├── agentfile.yaml              # System configuration
├── Dockerfile
└── go.mod
```

## Architecture

```
                  ┌──────────────────────────────────────┐
                  │            agentfile                  │
                  │                                      │
  MCP clients ──> │  /servers/{agent}  (Streamable HTTP) │
                  │        │                             │
  REST calls ──>  │  /api/v1/agents/{name}/run           │
                  │        │                             │
                  │        v                             │
                  │  ┌──────────┐    ┌───────────────┐   │
                  │  │  Agent   │───>│  MCP Client   │   │
                  │  │  Runtime │    │  Pool          │   │
                  │  │  (LLM)  │    │               │   │
                  │  └──────────┘    └───────┬───────┘   │
                  └─────────────────────────│───────────┘
                                            │
                              ┌─────────────┼─────────────┐
                              v             v             v
                        ┌──────────┐  ┌──────────┐  ┌──────────┐
                        │ MCP Srv  │  │ MCP Srv  │  │ MCP Srv  │
                        │ (srvd)   │  │ (github) │  │ (files)  │
                        └──────────┘  └──────────┘  └──────────┘
                         External MCP servers (tools)
```

## MCP Server Transports

agentfile supports connecting to external MCP servers via two transports:

- **`sse`** (default) — Legacy Server-Sent Events transport
- **`streamable-http`** — Newer Streamable HTTP transport

All MCP servers that agentfile *exposes* use Streamable HTTP.
