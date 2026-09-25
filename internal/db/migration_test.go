package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigrateEmptyDatabaseAndRepeat(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the real Postgres migration check")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("agentfoundry_migration_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db := &Pool{pool}
	for i := 0; i < 2; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migration pass %d: %v", i+1, err)
		}
	}
	for _, table := range []string{"agent_definitions", "inference_providers", "agent_runs", "agent_run_inputs", "chat_sessions"} {
		var found string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, schema+"."+table).Scan(&found); err != nil || found == "" {
			t.Fatalf("table %s missing: %q, %v", table, found, err)
		}
	}
}

func TestMigrationFailureRollsBackEarlierDDL(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the real Postgres migration check")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("agentfoundry_rollback_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE TABLE mcp_servers (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := (&Pool{pool}).Migrate(ctx); err == nil {
		t.Fatal("expected migration failure against incomplete mcp_servers table")
	}
	var rolledBack bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, schema+".api_keys").Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatalf("earlier DDL survived failed migration: rolled_back=%t, error=%v", rolledBack, err)
	}
}
