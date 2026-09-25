package session

import "testing"

func TestCompleteRunIsIdempotentAndDoesNotClearANewerRun(t *testing.T) {
	s := New()
	sess := s.Create("agent", "assistant", "owner")
	if err := s.AddMessage(sess.ID, Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActiveRunID(sess.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRun(sess.ID, "first", "answer"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActiveRunID(sess.ID, "second"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRun(sess.ID, "first", "duplicate"); err != nil {
		t.Fatal(err)
	}
	got := s.Get(sess.ID)
	if got.ActiveRunID != "second" || len(got.Messages) != 2 || got.Messages[1].Content != "answer" {
		t.Fatalf("session changed by stale completion: %+v", got)
	}
}

func TestStartRunClaimsSessionAndSnapshotsPreviousHistory(t *testing.T) {
	s := New()
	sess := s.Create("agent", "assistant", "owner")
	if err := s.AddMessage(sess.ID, Message{Role: "user", Content: "earlier"}); err != nil {
		t.Fatal(err)
	}
	history, err := s.StartRun(sess.ID, "run-1", Message{Role: "user", Content: "now"})
	if err != nil || len(history) != 1 || history[0].Content != "earlier" {
		t.Fatalf("start snapshot: %+v, %v", history, err)
	}
	if _, err := s.StartRun(sess.ID, "run-2", Message{Role: "user", Content: "too soon"}); err == nil {
		t.Fatal("concurrent session run accepted")
	}
	if got := s.Get(sess.ID); len(got.Messages) != 2 || got.ActiveRunID != "run-1" {
		t.Fatalf("rejected run modified session: %+v", got)
	}
}
