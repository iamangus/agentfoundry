package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/db"
	"github.com/angoo/agentfoundry/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type inferenceRegistrar struct{}

func (inferenceRegistrar) RegisterAgent(*config.Definition) error { return nil }

func TestInferenceProxyPrimaryAndBackupRouting(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL for inference proxy integration")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("inference_test_%d", time.Now().UnixNano())
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
	var primaryBody, backupBody map[string]any
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer primary-secret" {
			t.Error("wrong primary credentials")
		}
		_ = json.NewDecoder(r.Body).Decode(&primaryBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer backup-secret" || r.Header.Get("X-Custom") != "backup" {
			t.Error("wrong backup credentials/headers")
		}
		_ = json.NewDecoder(r.Body).Decode(&backupBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer backup.Close()
	providers := store.NewProviderStore(pool)
	p, err := providers.Create(ctx, "primary", "custom", primary.URL, "primary-secret", "primary-default", true, "user", "", "owner", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := providers.Create(ctx, "backup", "custom", backup.URL, "backup-secret", "backup-default", false, "user", "", "owner", map[string]string{"X-Custom": "backup"}, json.RawMessage(`{"reasoning":{"effort":"low"}}`))
	if err != nil {
		t.Fatal(err)
	}
	definitions := store.NewDBStore(pool, inferenceRegistrar{})
	def := &config.Definition{Name: "test", Kind: config.KindAgent, SystemPrompt: "help", ProviderID: p.ID, BackupProviderID: b.ID, ModelParams: json.RawMessage(`{"temperature":0.7}`)}
	if err := definitions.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{store: definitions, providerStore: providers, internalAPIKey: "internal"}
	for _, useBackup := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/inference", strings.NewReader(`{"model":"caller-override","messages":[{"role":"user","content":"hello"}],"response_format":{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}}`))
		req.SetPathValue("agentID", def.AgentID)
		req.Header.Set("Authorization", "Bearer internal")
		if useBackup {
			req.Header.Set("X-AgentFoundry-Backup", "true")
		}
		response := httptest.NewRecorder()
		handler.inferenceProxy(response, req)
		if response.Code != 200 {
			t.Fatalf("proxy response: %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret") {
			t.Fatal("provider credentials leaked")
		}
		if useBackup && (response.Header().Get("X-AgentFoundry-Provider") != b.ID || response.Header().Get("X-AgentFoundry-Model") != "backup-default") {
			t.Fatal("incorrect routing metadata")
		}
	}
	if primaryBody["model"] != "primary-default" || primaryBody["temperature"] != 0.7 {
		t.Fatalf("incorrect primary payload: %+v", primaryBody)
	}
	if backupBody["model"] != "backup-default" || backupBody["temperature"] != nil || backupBody["reasoning"] == nil {
		t.Fatalf("primary tuning leaked to backup: %+v", backupBody)
	}
	if backupBody["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatal("backup schema adaptation missing")
	}
	denied := httptest.NewRequest(http.MethodPost, "/inference", strings.NewReader(`{}`))
	denied.SetPathValue("agentID", def.AgentID)
	response := httptest.NewRecorder()
	handler.inferenceProxy(response, denied)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized inference accepted: %d", response.Code)
	}
}
