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

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/db"
	"github.com/angoo/agentfoundry/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInferenceProfilePersistenceAndDiscoveryAuthorization(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("set TEST_DATABASE_URL for profile persistence and authorization integration")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("inference_profiles_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	cfg, err := pgxpool.ParseConfig(dbURL)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-secret" {
			t.Error("wrong credentials")
		}
		fmt.Fprint(w, `{"data":[{"id":"catalog-model"}]}`)
	}))
	defer upstream.Close()
	providers := store.NewProviderStore(pool)
	primary, err := providers.Create(ctx, "private", "deepseek", upstream.URL, "private-secret", "primary-default", false, "user", "", "alice", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := providers.Create(ctx, "public", "openai", upstream.URL, "public-secret", "backup-default", true, "global", "", "alice", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions := store.NewDBStore(pool, inferenceRegistrar{})
	h := &Handler{providerStore: providers, store: definitions}
	alice := &auth.AuthContext{Subject: "alice", Teams: []string{"eng"}, IsTeamAdmin: true}
	bob := &auth.AuthContext{Subject: "bob", Teams: []string{"other"}, IsTeamAdmin: true}
	request := func(handler http.HandlerFunc, method, path, body string, ac *auth.AuthContext, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r = r.WithContext(auth.NewContext(r.Context(), ac))
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	body := fmt.Sprintf(`{"name":"Reliable","provider_id":%q,"backup_provider_id":%q}`, primary.ID, backup.ID)
	w := request(h.saveInferenceProfile, "POST", "/", body, alice, "")
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var profile store.InferenceProfile
	if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Model != "primary-default" || profile.BackupModel != "backup-default" || profile.CreatedBy != "alice" {
		t.Fatalf("incorrect defaults: %+v", profile)
	}
	w = request(h.listInferenceProfiles, "GET", "/", "", bob, "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("leaked private profile: %s", w.Body.String())
	}
	if w := request(h.saveInferenceProfile, "PUT", "/", body, bob, profile.ID); w.Code != 404 {
		t.Fatalf("unauthorized update: %d", w.Code)
	}
	for _, scope := range []string{"team", "global"} {
		badBody := fmt.Sprintf(`{"name":"Shared","scope":%q,"team":"eng","provider_id":%q}`, scope, primary.ID)
		w := request(h.saveInferenceProfile, "POST", "/", badBody, alice, "")
		if w.Code != 400 && w.Code != 403 {
			t.Fatalf("accepted inaccessible scope %s: %d", scope, w.Code)
		}
	}
	same := fmt.Sprintf(`{"name":"Same","provider_id":%q,"backup_provider_id":%q}`, primary.ID, primary.ID)
	if w := request(h.saveInferenceProfile, "POST", "/", same, alice, ""); w.Code != 400 {
		t.Fatalf("accepted identical backup: %d", w.Code)
	}
	// Applying copies ordinary routing fields; profiles are not runtime dependencies.
	def := &config.Definition{Kind: config.KindAgent, Name: "copied", SystemPrompt: "help", Scope: "user", CreatedBy: "alice", ProviderID: profile.ProviderID, Model: profile.Model, BackupProviderID: profile.BackupProviderID, BackupModel: profile.BackupModel}
	if err := definitions.SaveDefinition(def); err != nil {
		t.Fatal(err)
	}
	updated := fmt.Sprintf(`{"name":"Changed","provider_id":%q,"model":"new-model"}`, primary.ID)
	if w := request(h.saveInferenceProfile, "PUT", "/", updated, alice, profile.ID); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if got := definitions.GetDefinition("copied"); got.Model != "primary-default" || got.BackupProviderID != backup.ID {
		t.Fatalf("profile edit modified agent: %+v", got)
	}
	if w := request(h.deleteProvider, "DELETE", "/", "", alice, primary.ID); w.Code != 409 {
		t.Fatalf("deleted referenced provider: %d %s", w.Code, w.Body.String())
	}
	if w := request(h.listProviderModels, "GET", "/", "", bob, primary.ID); w.Code != 404 {
		t.Fatalf("private provider discovery allowed: %d", w.Code)
	}
	if w := request(h.listProviderModels, "GET", "/", "", alice, primary.ID); w.Code != 200 || !strings.Contains(w.Body.String(), "catalog-model") || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("discovery failed/leaked secrets: %d %s", w.Code, w.Body.String())
	}
	draft := fmt.Sprintf(`{"id":%q,"provider_type":"deepseek","base_url":%q,"api_key":"***"}`, primary.ID, upstream.URL)
	if w := request(h.discoverModels, "POST", "/", draft, alice, ""); w.Code != 200 {
		t.Fatalf("masked key discovery: %d %s", w.Code, w.Body.String())
	}
	draft = fmt.Sprintf(`{"id":%q,"provider_type":"deepseek","base_url":"https://different.example","api_key":"***"}`, primary.ID)
	if w := request(h.discoverModels, "POST", "/", draft, alice, ""); w.Code != 400 {
		t.Fatalf("allowed forwarding saved credentials to new URL: %d", w.Code)
	}
	if w := request(h.deleteInferenceProfile, "DELETE", "/", "", alice, profile.ID); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	if got := definitions.GetDefinition("copied"); got.Model != "primary-default" {
		t.Fatal("deleting profile changed agent")
	}
}
