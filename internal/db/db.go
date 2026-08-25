package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Pool struct {
	*pgxpool.Pool
}

func NewPool(ctx context.Context, dbURL string) (*Pool, error) {
	if dbURL == "" {
		return nil, fmt.Errorf("AUTH_DB_URL is required")
	}

	config, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parse db config: %w", err)
	}
	config.MaxConns = 10
	config.MinConns = 1
	config.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	slog.Info("connected to postgres")
	return &Pool{pool}, nil
}

func (p *Pool) Migrate(ctx context.Context) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS api_keys (
			id            TEXT PRIMARY KEY DEFAULT gen_random_uuid(),
			name          TEXT NOT NULL,
			key_hash      TEXT NOT NULL UNIQUE,
			key_prefix    TEXT NOT NULL,
			owner_subject TEXT NOT NULL,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_used_at  TIMESTAMPTZ,
			expires_at    TIMESTAMPTZ,
			revoked_at    TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_owner ON api_keys (owner_subject) WHERE revoked_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys (key_hash)`,

		`CREATE TABLE IF NOT EXISTS mcp_servers (
			id          TEXT PRIMARY KEY DEFAULT gen_random_uuid(),
			name        TEXT NOT NULL UNIQUE,
			url         TEXT NOT NULL,
			transport   TEXT NOT NULL DEFAULT 'sse',
			headers     JSONB DEFAULT '{}',
			scope       TEXT NOT NULL DEFAULT 'user',
			team        TEXT,
			created_by  TEXT NOT NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_servers_scope ON mcp_servers (scope, team)`,

		`CREATE TABLE IF NOT EXISTS mcp_tool_scopes (
			server_id   TEXT NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
			tool_name   TEXT NOT NULL,
			scope       TEXT NOT NULL,
			team        TEXT,
			created_by  TEXT NOT NULL,
			PRIMARY KEY (server_id, tool_name)
		)`,

		`CREATE TABLE IF NOT EXISTS agent_definitions (
			id                   TEXT PRIMARY KEY DEFAULT gen_random_uuid(),
			agent_id             TEXT NOT NULL UNIQUE,
			name                 TEXT NOT NULL UNIQUE,
			kind                 TEXT NOT NULL DEFAULT 'agent',
			description          TEXT NOT NULL DEFAULT '',
			model                TEXT NOT NULL DEFAULT '',
			system_prompt        TEXT NOT NULL,
			tools                JSONB DEFAULT '[]',
			max_turns            INT NOT NULL DEFAULT 10,
			max_concurrent_tools INT NOT NULL DEFAULT 0,
			force_json           BOOLEAN NOT NULL DEFAULT false,
			structured_output    JSONB,
			pre_inference_processors JSONB NOT NULL DEFAULT '[]',
			scope                TEXT NOT NULL DEFAULT 'user',
			team                 TEXT,
			created_by           TEXT NOT NULL,
			created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_definitions_agent_id ON agent_definitions (agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_definitions_scope ON agent_definitions (scope, team)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_definitions_created_by ON agent_definitions (created_by)`,

		`CREATE TABLE IF NOT EXISTS agent_versions (
			id              TEXT PRIMARY KEY DEFAULT gen_random_uuid(),
			agent_id        TEXT NOT NULL REFERENCES agent_definitions(agent_id) ON DELETE CASCADE,
			definition_yaml TEXT NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_versions_agent_id ON agent_versions (agent_id, created_at DESC)`,

		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS provider_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS memory_enabled BOOLEAN NOT NULL DEFAULT false`,
		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS memory_search_agent_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS memory_ingest_agent_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS tool_overrides JSONB NOT NULL DEFAULT '{}'`,
		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS model_params JSONB DEFAULT '{}'`,
		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS handoff_to TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS handoffs JSONB NOT NULL DEFAULT '[]'`,
		`ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS pre_inference_processors JSONB NOT NULL DEFAULT '[]'`,
		`ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS tool_overrides JSONB NOT NULL DEFAULT '{}'`,
		`ALTER TABLE inference_providers ADD COLUMN IF NOT EXISTS reasoning JSONB DEFAULT '{}'`,

		`CREATE TABLE IF NOT EXISTS inference_providers (
			id                TEXT PRIMARY KEY DEFAULT gen_random_uuid(),
			name              TEXT NOT NULL UNIQUE,
			provider_type     TEXT NOT NULL DEFAULT 'custom',
			base_url          TEXT NOT NULL DEFAULT '',
			api_key           TEXT NOT NULL DEFAULT '',
			default_model     TEXT NOT NULL DEFAULT '',
			schema_validation BOOLEAN NOT NULL DEFAULT true,
			headers           JSONB DEFAULT '{}',
			scope             TEXT NOT NULL DEFAULT 'user',
			team              TEXT,
			created_by        TEXT NOT NULL,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_inference_providers_scope ON inference_providers (scope, team)`,
	}

	for _, m := range migrations {
		if _, err := p.Exec(ctx, m); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	slog.Info("database migrations applied")
	return nil
}

func (p *Pool) Close() {
	p.Pool.Close()
}

func Tx(ctx context.Context, pool *Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
