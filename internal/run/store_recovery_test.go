package run

import (
	"context"
	"errors"
	"testing"
)

func TestInFlightAndLookupPreserveRunOwnership(t *testing.T) {
	s := New()
	first := s.Create("writer", "owner-a", "", "same-task")
	if err := s.SetWorkflowID(first.ID, "workflow-a"); err != nil {
		t.Fatal(err)
	}
	second := s.Create("writer", "owner-b", "", "same-task")
	if err := s.SetWorkflowID(second.ID, "workflow-b"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(first.ID, StatusCompleted, "done", ""); err != nil {
		t.Fatal(err)
	}
	active := s.InFlight()
	if len(active) != 1 || active[0].ID != second.ID || active[0].Owner != "owner-b" {
		t.Fatalf("in flight: %+v", active)
	}
	active[0].Owner = "other"
	lookup, ok := s.Get(second.ID)
	if !ok || lookup.Owner != "owner-b" {
		t.Fatalf("stored run mutated by caller: %+v", lookup)
	}
	runs := s.ListByTaskID("same-task")
	if len(runs) != 2 || runs[0].ID != second.ID {
		t.Fatalf("task runs are not newest-first: %+v", runs)
	}
}

func TestPersistentClientKeyDeduplicatesActiveRun(t *testing.T) {
	s := New()
	first, created, err := s.CreatePersistentWithKeyChecked("eve", "owner", "frontend:conversation")
	if err != nil || !created {
		t.Fatalf("first creation: %+v, %t, %v", first, created, err)
	}
	again, created, err := s.CreatePersistentWithKeyChecked("eve", "owner", "frontend:conversation")
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("duplicate creation: %+v, %t, %v", again, created, err)
	}
	if err := s.UpdateStatus(first.ID, StatusFailed, "", "workflow unavailable"); err != nil {
		t.Fatal(err)
	}
	replacement, created, err := s.CreatePersistentWithKeyChecked("eve", "owner", "frontend:conversation")
	if err != nil || !created || replacement.ID == first.ID {
		t.Fatalf("terminal run was reused: %+v, %t, %v", replacement, created, err)
	}
}

func TestDispatchTaskKeySurvivesTerminalOutcome(t *testing.T) {
	s := New()
	first, created, err := s.CreateByTaskChecked("writer", "owner", "", "dispatch-1")
	if err != nil || !created {
		t.Fatalf("first dispatch: %+v, %t, %v", first, created, err)
	}
	if err := s.UpdateStatus(first.ID, StatusCompleted, "done", ""); err != nil {
		t.Fatal(err)
	}
	retried, created, err := s.CreateByTaskChecked("writer", "owner", "", "dispatch-1")
	if err != nil || created || retried.ID != first.ID || retried.Response != "done" {
		t.Fatalf("duplicate dispatch: %+v, %t, %v", retried, created, err)
	}
	other, created, err := s.CreateByTaskChecked("writer", "other owner", "", "dispatch-1")
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("other owner dispatch: %+v, %t, %v", other, created, err)
	}
}

func TestUnstartedExcludesWorkflowsWithRecordedIdentity(t *testing.T) {
	s := New()
	orphan := s.Create("agent", "owner", "", "orphan")
	started := s.Create("agent", "owner", "", "started")
	if err := s.SetWorkflowID(started.ID, "run-"+started.ID); err != nil {
		t.Fatal(err)
	}
	unstarted := s.Unstarted()
	if len(unstarted) != 1 || unstarted[0].ID != orphan.ID {
		t.Fatalf("unstarted runs: %+v", unstarted)
	}
}

func TestRestoredMCPConnectionCannotAttachToFinishedRun(t *testing.T) {
	s := New()
	r := s.CreatePersistent("agent", "owner")
	if !s.AddEphemeralNameIfActive(r.ID, "eve") {
		t.Fatal("active run rejected restored MCP server")
	}
	if err := s.UpdateStatus(r.ID, StatusCanceled, "", "canceled"); err != nil {
		t.Fatal(err)
	}
	if s.AddEphemeralNameIfActive(r.ID, "another") {
		t.Fatal("finished run accepted a late MCP connection")
	}
	stored, _ := s.Get(r.ID)
	if len(stored.EphemeralNames) != 1 || stored.EphemeralNames[0] != "eve" {
		t.Fatalf("unexpected attached servers: %v", stored.EphemeralNames)
	}
}

func TestTerminalRunCannotBeOverwrittenByLateWatcher(t *testing.T) {
	s := New()
	r := s.Create("agent", "owner", "", "")
	if err := s.UpdateStatus(r.ID, StatusCanceled, "", "canceled"); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForStatus(context.Background(), r.ID, StatusCompleted, "late result", ""); !errors.Is(err, ErrTerminal) {
		t.Fatalf("late result was not rejected: %v", err)
	}
	stored, _ := s.Get(r.ID)
	if stored.Status != StatusCanceled || stored.Response != "" {
		t.Fatalf("late watcher overwrote cancellation: %+v", stored)
	}
}
