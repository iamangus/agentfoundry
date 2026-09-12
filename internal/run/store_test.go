package run

import "testing"

func TestCreatePersistentStartsWaiting(t *testing.T) {
	s := New()
	r := s.CreatePersistent("assistant", "user-1")
	if !r.Persistent {
		t.Fatal("persistent run was not marked persistent")
	}
	if r.Status != StatusWaiting {
		t.Fatalf("status = %q, want %q", r.Status, StatusWaiting)
	}
	if r.Owner != "user-1" {
		t.Fatalf("owner = %q", r.Owner)
	}
}
