# agentfoundry

Backend service for defining, managing, and orchestrating AI agents. Agent definitions are stored as YAML files (locally or in S3 with versioning) and exposed as individual MCP servers for composability. Agent runs are dispatched to [agentfoundry-worker](https://github.com/angoo/agentfoundry-worker) via Temporal workflows. Supports OIDC authentication (Keycloak), role-based access control, team-scoped agents, and personal API keys.

## Concepts

**Agent** — A YAML definition that combines a system prompt, an LLM model, and a set of tools. When called, the agent is dispatched as a Temporal workflow to a worker process, which runs a multi-turn LLM conversation, invoking tools as needed, and returns a final response.

**Tool** — A capability discovered from an external MCP server. Tools are referenced by agents using namespaced syntax: `server.tool` (e.g. `srvd.searxng_web_search`).

**MCP Server** — Each agent is exposed as a Streamable HTTP MCP server at `/servers/{agent-name}`. A default server at `/servers/default` exposes all discovered tools. External MCP clients (Claude Desktop, Cursor, etc.) can connect to any of these endpoints.

**Composability** — An agent can list another agent in its `tools:` section. When called, the sub-agent runs its own LLM loop and returns a response, enabling multi-agent workflows.

**S3 Versioning** — Agent definitions can be stored in S3 (or any S3-compatible store like RustFS) instead of local files. Every save creates a new version, enabling version history, diffs, and rollback.

## Quick Start

### Prerequisites

- Go 1.21+
- A running [Temporal](https://temporal.io/) server
- [agentfoundry-worker](https://github.com/angoo/agentfoundry-worker) running (the worker handles LLM calls)
- (Optional) Keycloak for OIDC authentication
- (Optional) S3-compatible storage for versioned agent definitions

### Build and Run

```bash
go build -o agentfoundry ./cmd/agentfoundry/
./agentfoundry
```

### Docker

```bash
docker build -t agentfoundry .
docker run -p 3000:3000 \
  -v $(pwd)/data:/data \
  agentfoundry
```

The container stores all persistent data under `/data` (definitions and config). Mount a volume or bind-mount there to persist agent definitions and provide your own `agentfoundry.yaml`. The default definitions are baked into the image at `/data/definitions/`.

## Configuration

### agentfoundry.yaml

```yaml
listen: ":3000"
definitions_dir: "./definitions"  # local filesystem (default)

# S3 storage with versioning (optional, replaces local filesystem)
s3:
  enable: true
  bucket: "agentfoundry"
  prefix: "definitions/"
  endpoint: "https://rustfs.example.com"  # or any S3-compatible endpoint
  region: "us-east-1"

temporal:
  host_port: "localhost:7233"
  namespace: "default"
  api_key: "${TEMPORAL_API_KEY}"

mcp_servers:
  - name: "srvd"
    url: "https://mcp.srvd.dev/mcp"
    transport: "streamable-http"

  - name: "filesystem"
    url: "http://localhost:4000/sse"
    transport: "sse"
```

All auth configuration is via environment variables (see [Authentication & Authorization](#authentication--authorization)).

### Agent Definition

Agent definitions are YAML files. By default they are stored in the `definitions/` directory and hot-reloaded when changed. Alternatively, they can be stored in S3 with automatic versioning (see S3 configuration above).

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

Agents can optionally include scope and team fields for authorization:

```yaml
kind: agent
name: team-helper
scope: team            # "user" (default), "team", or "global"
team: engineering     # required when scope is "team"
```

## API

### Agents

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/agents` | List agents visible to the authenticated user |
| `GET` | `/api/v1/agents/{name}` | Get agent definition (scoped by visibility) |
| `POST` | `/api/v1/agents` | Create agent (scope: `user` by default, `team`/`global` require roles) |
| `PUT` | `/api/v1/agents/{name}` | Update agent (permission-checked) |
| `DELETE` | `/api/v1/agents/{name}` | Delete agent (permission-checked) |
| `POST` | `/api/v1/agents/{name}/run` | Run an agent |

#### Versioning (S3 storage only)

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/agents/{name}/versions` | List version history |
| `GET` | `/api/v1/agents/{name}/version?version_id=xyz` | View a specific version |
| `POST` | `/api/v1/agents/{name}/rollback?version_id=xyz` | Restore a previous version (owner-only) |

### Chat Sessions

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/chat/sessions` | Create a chat session (owner-scoped) |
| `GET` | `/api/v1/chat/sessions` | List current user's sessions |
| `GET` | `/api/v1/chat/sessions/{id}` | Get session details |
| `POST` | `/api/v1/chat/sessions/{id}/messages` | Send a message (owner-restricted, returns `run_id`) |
| `GET` | `/api/v1/chat/runs/{id}/events` | SSE stream for run events (owner-restricted) |

### API Keys

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/api-keys` | Create a personal API key |
| `GET` | `/api/v1/api-keys` | List your API keys |
| `DELETE` | `/api/v1/api-keys/{id}` | Revoke an API key |

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

## Authentication & Authorization

Auth is **disabled by default**. Set `AUTH_ISSUER` to enable it. When enabled, all endpoints except `/health` and `/servers/` require a valid `Authorization: Bearer <token>` header.

### Authentication Methods

| Method | Token format | Description |
|--------|-------------|-------------|
| **OIDC JWT** | `Bearer eyJ...` | Standard OpenID Connect access token from your IdP |
| **Personal API Key** | `Bearer afk_...` | SHA-256 hashed key stored in Postgres, resolves to owner's groups/roles at request time |

### Environment Variables

#### Required (to enable auth)

| Variable | Description | Example |
|----------|-------------|---------|
| `AUTH_ISSUER` | OIDC issuer URL | `https://keycloak.example.com/realms/opendev` |

#### OIDC Token Validation

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTH_AUDIENCE` | *(empty, skips client ID check)* | Expected `aud` claim in the token |
| `AUTH_ROLES_CLAIM` | `realm_access.roles` | Dot-separated path to the roles array in JWT claims |
| `AUTH_GROUPS_CLAIM` | `groups` | Dot-separated path to the groups array in JWT claims |

#### Access Control

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTH_ACCESS_ROLES` | `opendev-user` | Comma-separated realm roles that grant API access |
| `AUTH_ADMIN_ROLES` | `opendev-admin` | Comma-separated realm roles for global admin (full read/write/delete) |
| `AUTH_TEAM_ADMIN_ROLE` | `team-admin` | Realm role for team admins (can edit/delete any team agent) |

#### Keycloak Admin API (for API key group resolution)

| Variable | Description |
|----------|-------------|
| `KEYCLOAK_URL` | Base Keycloak URL (e.g. `https://keycloak.example.com`) |
| `KEYCLOAK_REALM` | Realm name (e.g. `opendev`) |
| `KEYCLOAK_ADMIN_CLIENT_ID` | Confidential client with `view-users` and `query-users` roles |
| `KEYCLOAK_ADMIN_CLIENT_SECRET` | Secret for the admin client |

#### Postgres (for API key storage)

| Variable | Description |
|----------|-------------|
| `AUTH_DB_URL` | Postgres connection URL (e.g. `postgres://user:pass@host:5432/dbname`) |

### Agent Scopes & Permissions

Agents have a `scope` field controlling visibility and permissions:

| Scope | Visible to | Editable by | Deletable by |
|-------|-----------|-------------|-------------|
| `global` | Everyone | Global admins only | Global admins only |
| `team` | Team members | Creator + team admins | Creator + team admins |
| `user` *(default)* | Creator only | Creator only | Creator only |

Team-scoped agents require a `team` field matching the user's group name.

### API Keys

Create/manage personal API keys via REST:

```bash
# Create a key
curl -X POST http://localhost:3000/api/v1/api-keys \
  -H "Authorization: Bearer <jwt>" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-script"}'
# Returns: {"id": "...", "name": "my-script", "key_prefix": "afk_a1b2c3d4", "full_key": "afk_a1b2c3d4..."}

# List your keys
curl http://localhost:3000/api/v1/api-keys \
  -H "Authorization: Bearer <jwt>"

# Revoke a key
curl -X DELETE http://localhost:3000/api/v1/api-keys/<id> \
  -H "Authorization: Bearer <jwt>"
```

Keys are stored as SHA-256 hashes. On each use, the owner's groups and roles are fetched from Keycloak Admin API with a 60-second cache, so revoking a user's Keycloak roles immediately affects their API key access.

### Example Configuration

```bash
# Minimal (auth enabled, Keycloak defaults)
export AUTH_ISSUER="https://keycloak.example.com/realms/opendev"
export AUTH_DB_URL="postgres://opendev:secret@localhost:5432/opendev"

# Full configuration
export AUTH_ISSUER="https://keycloak.example.com/realms/opendev"
export AUTH_AUDIENCE="agentfoundry"
export AUTH_ROLES_CLAIM="realm_access.roles"
export AUTH_GROUPS_CLAIM="groups"
export AUTH_ACCESS_ROLES="opendev-user"
export AUTH_ADMIN_ROLES="opendev-admin"
export AUTH_TEAM_ADMIN_ROLE="team-admin"
export KEYCLOAK_URL="https://keycloak.example.com"
export KEYCLOAK_REALM="opendev"
export KEYCLOAK_ADMIN_CLIENT_ID="opendev-admin"
export KEYCLOAK_ADMIN_CLIENT_SECRET="super-secret"
export AUTH_DB_URL="postgres://opendev:secret@localhost:5432/opendev"
```

### Keycloak Setup

#### 1. Create the Realm

Create a realm named `opendev` (or whatever matches your `AUTH_ISSUER` path).

#### 2. Create Realm Roles

| Role | Purpose |
|------|---------|
| `opendev-user` | Grants API access (all authenticated users should have this) |
| `opendev-admin` | Global admin — full read/write/delete on all agents |
| `team-admin` | Team admin — can edit/delete any agent in their teams |

#### 3. Create Groups for Teams

Create **top-level groups** (not nested) — one per team. The group name becomes the team name. For example, a group named `engineering` means users in that group are members of the `engineering` team.

```
opendev realm
├── Groups
│   ├── engineering
│   ├── marketing
│   └── product
```

Assign users to the appropriate groups.

#### 4. Create the OIDC Client (for user login)

Create a client with:
- **Client ID**: `agentfoundry` (matches `AUTH_AUDIENCE`)
- **Client authentication**: Off (public client for user login)
- **Valid redirect URIs**: Your frontend callback URL
- **Web origins**: Your frontend origin

#### 5. Create the Admin Service Account (for API key group resolution)

Create a **confidential client** with:
- **Client ID**: `opendev-admin` (matches `KEYCLOAK_ADMIN_CLIENT_ID`)
- **Client authentication**: On (client credentials enabled)
- **Service account roles**: Assign `view-users` and `query-users` from `realm-management`

Go to **Clients > opendev-admin > Service account roles** and add:
- `realm-management` → `view-users`
- `realm-management` → `query-users`

Record the client secret — this goes in `KEYCLOAK_ADMIN_CLIENT_SECRET`.

#### 6. Assign Roles to Users

For each user, assign:
- `opendev-user` role (required for API access)
- `opendev-admin` role (for global admins only)
- `team-admin` role (for team admins — grants admin across all their teams)

#### 7. (Optional) Create a Service Account Client for Machine-to-Machine

If external services need to call the API without a human user:
- Create a confidential client
- Assign it the appropriate realm roles (`opendev-user`, `opendev-admin`, etc.)
- Use client credentials grant to obtain a token

```bash
curl -X POST https://keycloak.example.com/realms/opendev/protocol/openid-connect/token \
  -d "grant_type=client_credentials" \
  -d "client_id=my-service" \
  -d "client_secret=secret"
```

The token's `azp` claim becomes the `Subject` in the auth context.

## Project Structure

```
agentfoundry/
├── cmd/agentfoundry/main.go       # Daemon entrypoint
├── internal/
│   ├── api/                    # REST API (agents, chat, API keys, versioning)
│   ├── auth/                   # OIDC JWT validation, API key store, middleware
│   ├── config/                 # System config, agent definitions, YAML loader
│   ├── db/                     # Postgres pool + auto-migration
│   ├── mcp/                    # Per-agent Streamable HTTP MCP servers
│   ├── mcpclient/              # MCP client pool (connects to external servers)
│   ├── registry/               # In-memory agent definition registry
│   ├── session/                # In-memory chat session store
│   ├── store/                  # S3-backed definition store with versioning
│   ├── stream/                 # SSE stream manager
│   └── temporal/               # Temporal workflow client (dispatches to worker)
├── definitions/                # Agent YAML definitions (filesystem mode)
├── agentfoundry.yaml           # System configuration
├── Dockerfile
└── go.mod
```

## Architecture

```
                    ┌──────────────────────────────────────┐
                    │            agentfoundry                  │
                    │         (backend orchestrator)          │
                    │                                      │
    MCP clients ──> │  /servers/{agent}  (Streamable HTTP) │
                    │        │                             │
    REST calls ──>  │  auth middleware ──────────────────┐ │
                    │        │                          │ │
                    │        v                          v │
                    │  ┌──────────┐            ┌────────┐ │
                    │  │  Agent   │            │ API Key│ │
                    │  │  Store   │            │ Store  │ │
                    │  │ (S3/FS) │            │(PgSQL) │ │
                    │  └──────────┘            └────────┘ │
                    │      │                             │
                    │      v                             │
                    │  Temporal ──> agentfoundry-worker    │
                    │  (dispatch)   (LLM calls + tools)   │
                    │      │                             │
                    │      v                             │
                    │  ┌───────────────┐                 │
                    │  │  MCP Client   │                 │
                    │  │  Pool          │                 │
                    │  └───────┬───────┘                 │
                    └──────────│─────────────────────────┘
                               │
             ┌─────────────────┼─────────────────┐
             v                 v                 v
       ┌──────────┐     ┌──────────┐     ┌──────────┐
       │ MCP Srv  │     │ MCP Srv  │     │ MCP Srv  │
       │ (srvd)   │     │ (github) │     │ (files)  │
       └──────────┘     └──────────┘     └──────────┘
        External MCP servers (tools)

    Auth flow:
    Bearer token ──> JWT or API key ──> Keycloak (JWT verify / Admin API groups)
```

## MCP Server Transports

agentfoundry supports connecting to external MCP servers via two transports:

- **`sse`** (default) — Legacy Server-Sent Events transport
- **`streamable-http`** — Newer Streamable HTTP transport

All MCP servers that agentfoundry *exposes* use Streamable HTTP.
