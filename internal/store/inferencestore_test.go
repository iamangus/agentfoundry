package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/angoo/agentfoundry/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInferenceStorePaginationAndUpsert(t *testing.T) {
	url := os.Getenv("INFERENCE_TEST_DB_URL")
	if url == "" {
		t.Skip("INFERENCE_TEST_DB_URL must point at a disposable PostgreSQL database")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("inference_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
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
	if err = (&db.Pool{Pool: pool}).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := NewInferenceStore(pool)
	started := time.Date(2026, 10, 29, 10, 0, 0, 123456789, time.UTC)
	for i := 0; i < 205; i++ {
		a := InferenceAttempt{ID: fmt.Sprintf("call-%03d", i), RunID: "run", InputID: "turn", AgentID: "agent", ProviderID: "provider", StartedAt: started, Status: "completed"}
		if err = s.Save(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err = s.Save(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	other := InferenceAttempt{ID: "other", RunID: "run", InputID: "other-turn", StartedAt: started}
	if err = s.Save(ctx, other); err != nil {
		t.Fatal(err)
	}
	first, err := s.List(ctx, "run", "turn", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Attempts) != 200 || first.NextCursor == "" {
		t.Fatalf("bad first page: %d", len(first.Attempts))
	}
	second, err := s.List(ctx, "run", "turn", first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Attempts) != 5 || second.NextCursor != "" {
		t.Fatalf("bad final page: %d", len(second.Attempts))
	}
	seen := map[string]bool{}
	for _, a := range append(first.Attempts, second.Attempts...) {
		if seen[a.ID] {
			t.Fatal("duplicate cursor result")
		}
		seen[a.ID] = true
	}
	if len(seen) != 205 {
		t.Fatal("upsert or pagination lost attempts")
	}
	if _, err = s.List(ctx, "run", "turn", "invalid"); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}
