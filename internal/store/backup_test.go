package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

type backupRegistrar struct{ definitions []*config.Definition }

func (r *backupRegistrar) RegisterAgent(def *config.Definition) error {
	r.definitions = append(r.definitions, def)
	return nil
}

func TestBackupPersistenceAndRevisions(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL for Postgres persistence checks")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("backup_test_%d", time.Now().UnixNano())
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
	if err := (&db.Pool{Pool: pool}).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	registrar := &backupRegistrar{}
	store := NewDBStore(pool, registrar)
	def := &config.Definition{Name: "test", Kind: config.KindAgent, SystemPrompt: "help", ProviderID: "primary", BackupProviderID: "backup", BackupModel: "alternate", Scope: "user"}
	if err := store.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	check := func(got *config.Definition) {
		t.Helper()
		if got == nil || got.BackupProviderID != "backup" || got.BackupModel != "alternate" {
			t.Fatalf("lost backup configuration: %+v", got)
		}
	}
	check(store.GetDefinition("test"))
	check(store.GetDefinitionByID(def.AgentID))
	defs := store.ListDefinitions()
	if len(defs) != 1 {
		t.Fatalf("definitions: %d", len(defs))
	}
	check(defs[0])
	registrar.definitions = nil
	if err := store.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(registrar.definitions) != 1 {
		t.Fatal("failed to register loaded definition")
	}
	check(registrar.definitions[0])
	var revision string
	if err := pool.QueryRow(ctx, `SELECT definition_yaml FROM agent_versions WHERE agent_id=$1 ORDER BY created_at DESC LIMIT 1`, def.AgentID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	var restored config.Definition
	if err := yaml.Unmarshal([]byte(revision), &restored); err != nil {
		t.Fatal(err)
	}
	check(&restored)
	def.BackupProviderID = ""
	def.BackupModel = ""
	if err := store.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	if got := store.GetDefinition("test"); got.BackupProviderID != "" || got.BackupModel != "" {
		t.Fatal("could not clear backup")
	}
	if err := store.SaveDefinition(&restored); err != nil {
		t.Fatal(err)
	}
	check(store.GetDefinition("test"))
}
