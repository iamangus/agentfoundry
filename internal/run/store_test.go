package run

import (
	"encoding/json"
	"testing"
)

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

func TestRunJSONIncludesWorkflowID(t *testing.T) {
	s := New()
	r := s.Create("assistant", "user-1", "", "")
	if err := s.SetWorkflowID(r.ID, "workflow-123"); err != nil {
		t.Fatalf("SetWorkflowID: %v", err)
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["workflow_id"] != "workflow-123" {
		t.Fatalf("workflow_id = %v, want workflow-123", got["workflow_id"])
	}
}
