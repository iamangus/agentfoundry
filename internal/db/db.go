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
