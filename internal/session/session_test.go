package session_test

import (
	"testing"

	"github.com/angoo/agentfoundry/internal/session"
)

func TestStore_CreateAndGet(t *testing.T) {
	s := session.New()
	sess := s.Create("researcher", "user-1")

	if sess.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if sess.AgentName != "researcher" {
		t.Errorf("got agent %q, want %q", sess.AgentName, "researcher")
	}
	if sess.Owner != "user-1" {
		t.Errorf("got owner %q, want %q", sess.Owner, "user-1")
	}
	if len(sess.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(sess.Messages))
	}

	got := s.Get(sess.ID)
	if got == nil {
		t.Fatal("expected to find session")
	}
	if got.ID != sess.ID {
		t.Errorf("got ID %q, want %q", got.ID, sess.ID)
	}
}

func TestStore_GetNotFound(t *testing.T) {
	s := session.New()
	if s.Get("nonexistent") != nil {
		t.Error("expected nil for nonexistent session")
	}
}

func TestStore_ListByOwner(t *testing.T) {
	s := session.New()
	s.Create("agent-a", "user-1")
	s.Create("agent-b", "user-2")
	s.Create("agent-c", "user-1")

	sessions := s.ListByOwner("user-1")
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	sessions2 := s.ListByOwner("user-2")
	if len(sessions2) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions2))
	}

	sessions3 := s.ListByOwner("user-3")
	if len(sessions3) != 0 {
		t.Fatalf("got %d sessions, want 0", len(sessions3))
	}
}

func TestStore_AddMessage(t *testing.T) {
	s := session.New()
	sess := s.Create("agent-a", "user-1")

	msg := session.Message{Role: "user", Content: "hello", Time: sess.CreatedAt}
	if err := s.AddMessage(sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	got := s.Get(sess.ID)
	if len(got.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(got.Messages))
	}
	if got.Messages[0].Content != "hello" {
		t.Errorf("got content %q, want %q", got.Messages[0].Content, "hello")
	}
}

func TestStore_AddMessageNotFound(t *testing.T) {
	s := session.New()
	err := s.AddMessage("nonexistent", session.Message{})
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestStore_SetActiveRunID(t *testing.T) {
	s := session.New()
	sess := s.Create("agent-a", "user-1")

	if err := s.SetActiveRunID(sess.ID, "run-123"); err != nil {
		t.Fatalf("SetActiveRunID: %v", err)
	}

	got := s.Get(sess.ID)
	if got.ActiveRunID != "run-123" {
		t.Errorf("got active run ID %q, want %q", got.ActiveRunID, "run-123")
	}
}

func TestStore_ClearActiveRunID(t *testing.T) {
	s := session.New()
	sess := s.Create("agent-a", "user-1")
	s.SetActiveRunID(sess.ID, "run-123")
	s.ClearActiveRunID(sess.ID)

	got := s.Get(sess.ID)
	if got.ActiveRunID != "" {
		t.Errorf("got active run ID %q, want empty", got.ActiveRunID)
	}
}

func TestStore_FindByRunID(t *testing.T) {
	s := session.New()
	sess := s.Create("agent-a", "user-1")
	s.SetActiveRunID(sess.ID, "run-456")

	found := s.FindByRunID("run-456")
	if found == nil {
		t.Fatal("expected to find session by run ID")
	}
	if found.ID != sess.ID {
		t.Errorf("got session ID %q, want %q", found.ID, sess.ID)
	}

	if s.FindByRunID("nonexistent") != nil {
		t.Error("expected nil for nonexistent run ID")
	}
}

func TestStore_FindByRunID_AfterClear(t *testing.T) {
	s := session.New()
	sess := s.Create("agent-a", "user-1")
	s.SetActiveRunID(sess.ID, "run-789")
	s.ClearActiveRunID(sess.ID)

	if s.FindByRunID("run-789") != nil {
		t.Error("expected nil after clearing active run ID")
	}
}
