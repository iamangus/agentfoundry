package web

import (
	"testing"
)

func TestCloneAgentName_NoCopies(t *testing.T) {
	name, err := cloneAgentName("foo", func(s string) bool { return false })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "foo-copy" {
		t.Errorf("got %q, want %q", name, "foo-copy")
	}
}

func TestCloneAgentName_FirstCopyExists(t *testing.T) {
	existing := map[string]bool{"foo-copy": true}
	name, err := cloneAgentName("foo", func(s string) bool { return existing[s] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "foo-copy-2" {
		t.Errorf("got %q, want %q", name, "foo-copy-2")
	}
}

func TestCloneAgentName_ManyCopiesExist(t *testing.T) {
	// copies 1-4 exist, 5 is free
	existing := map[string]bool{
		"foo-copy":   true,
		"foo-copy-2": true,
		"foo-copy-3": true,
		"foo-copy-4": true,
	}
	name, err := cloneAgentName("foo", func(s string) bool { return existing[s] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "foo-copy-5" {
		t.Errorf("got %q, want %q", name, "foo-copy-5")
	}
}

func TestCloneAgentName_TooManyCopies(t *testing.T) {
	_, err := cloneAgentName("foo", func(s string) bool { return true })
	if err == nil {
		t.Error("expected error when all copy names are taken")
	}
}
