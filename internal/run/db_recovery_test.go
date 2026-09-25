package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/angoo/agentfoundry/internal/db"
	"github.com/angoo/agentfoundry/internal/session"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatabaseRunKeysSurviveRestartAndConcurrentProcesses(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "run_test_" + hex.EncodeToString(b)
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
	a, err := NewDB(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	bStore, err := NewDB(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	created, fresh, err := a.CreatePersistentWithKeyChecked("eve", "owner", "frontend:conversation")
	if err != nil || !fresh {
		t.Fatalf("create: %+v %t %v", created, fresh, err)
	}
	duplicate, fresh, err := bStore.CreatePersistentWithKeyChecked("eve", "owner", "frontend:conversation")
	if err != nil || fresh || duplicate.ID != created.ID {
		t.Fatalf("cross-process duplicate: %+v %t %v", duplicate, fresh, err)
	}
	initial, fresh, err := a.CreateByTaskChecked("coder", "owner", "", "job-attempt-1")
	if err != nil || !fresh {
		t.Fatalf("dispatch: %+v %t %v", initial, fresh, err)
	}
	recovered, fresh, err := bStore.CreateByTaskChecked("coder", "owner", "", "job-attempt-1")
	if err != nil || fresh || recovered.ID != initial.ID {
		t.Fatalf("cross-process dispatch: %+v %t %v", recovered, fresh, err)
	}
	if err := a.RecordInput(created.ID, "input-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.FinishReceipt(created.ID, "input-1", "processed"); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDB(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := restarted.InputStatus(created.ID, "input-1"); err != nil || status != "processed" {
		t.Fatalf("restored receipt: %q %v", status, err)
	}
	if err := restarted.UpdateStatus(created.ID, StatusCanceled, "", "canceled"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.UpdateStatus(created.ID, StatusCompleted, "late", ""); !errors.Is(err, ErrTerminal) {
		t.Fatalf("late completion replaced cancellation: %v", err)
	}
	restarted, err = NewDB(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if terminal, ok := restarted.Get(created.ID); !ok || terminal.Status != StatusCanceled {
		t.Fatalf("restored terminal status: %+v %t", terminal, ok)
	}
	if run, ok := restarted.Get(initial.ID); !ok || run.TaskID != "job-attempt-1" {
		t.Fatalf("restored dispatch: %+v %t", run, ok)
	}
	sessions, err := session.NewDB(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := sessions.CreateChecked("agent-id", "eve", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.AddMessage(chat.ID, session.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	sessions, err = session.NewDB(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if restoredChat := sessions.Get(chat.ID); restoredChat == nil || len(restoredChat.Messages) != 1 || restoredChat.Messages[0].Content != "hello" {
		t.Fatalf("restored conversation: %+v", restoredChat)
	}
}
