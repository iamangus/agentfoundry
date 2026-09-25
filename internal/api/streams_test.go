package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/run"
	"github.com/angoo/agentfoundry/internal/stream"
)

func TestPersistentTurnDoneClosesOnlyTheTurnStream(t *testing.T) {
	runs := run.New()
	ru := runs.CreatePersistent("assistant", "owner")
	if err := runs.BeginInput(ru.ID, "input-1"); err != nil {
		t.Fatal(err)
	}
	streams := stream.NewManager()
	streams.Create(ru.ID)
	h := &Handler{runs: runs, streams: streams}

	req := httptest.NewRequest(http.MethodPost, "/api/internal/streams/"+ru.ID+"/events", strings.NewReader(`{"type":"turn_done","input_id":"input-1","data":"answer"}`))
	req.SetPathValue("id", ru.ID)
	res := httptest.NewRecorder()
	h.publishStreamEvent(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d", res.Code)
	}
	updated, _ := runs.Get(ru.ID)
	if updated.Status != run.StatusWaiting || updated.Response != "answer" {
		t.Fatalf("unexpected run state: %+v", updated)
	}
	ch, unsubscribe := streams.Get(ru.ID).Subscribe()
	defer unsubscribe()
	event, ok := <-ch
	if !ok || event.Type != "done" || event.Data != "answer" {
		t.Fatalf("unexpected event: %+v, open=%v", event, ok)
	}
}

func TestStaleTurnResultCannotCompleteNewInput(t *testing.T) {
	runs := run.New()
	ru := runs.CreatePersistent("assistant", "owner")
	if err := runs.BeginInput(ru.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.FinishInput(ru.ID, "first", "old", ""); err != nil {
		t.Fatal(err)
	}
	if err := runs.BeginInput(ru.ID, "second"); err != nil {
		t.Fatal(err)
	}
	streams := stream.NewManager()
	streams.Create(ru.ID)
	h := &Handler{runs: runs, streams: streams}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/streams/"+ru.ID+"/events", strings.NewReader(`{"type":"turn_done","input_id":"first","data":"stale"}`))
	req.SetPathValue("id", ru.ID)
	res := httptest.NewRecorder()
	h.publishStreamEvent(res, req)
	updated, _ := runs.Get(ru.ID)
	if res.Code != http.StatusNoContent || updated.Status != run.StatusRunning || updated.CurrentInputID != "second" || updated.Response != "" {
		t.Fatalf("stale result changed run: status=%d run=%+v", res.Code, updated)
	}
}

func TestInputReceiptIsOwnerScopedAndSurvivesTurnCompletion(t *testing.T) {
	runs := run.New()
	ru := runs.CreatePersistent("assistant", "owner")
	if err := runs.BeginInput(ru.ID, "event-1"); err != nil {
		t.Fatal(err)
	}
	if err := runs.RecordInput(ru.ID, "event-1"); err != nil {
		t.Fatal(err)
	}
	h := &Handler{runs: runs, streams: stream.NewManager()}
	ack := httptest.NewRequest(http.MethodPost, "/api/internal/streams/"+ru.ID+"/events", strings.NewReader(`{"type":"input_processed","input_id":"event-1"}`))
	ack.SetPathValue("id", ru.ID)
	ackResult := httptest.NewRecorder()
	h.publishStreamEvent(ackResult, ack)
	if ackResult.Code != http.StatusNoContent {
		t.Fatalf("ack status: %d", ackResult.Code)
	}
	status := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+ru.ID+"/inputs/event-1", nil)
	status.SetPathValue("id", ru.ID)
	status.SetPathValue("inputID", "event-1")
	status = status.WithContext(auth.NewContext(status.Context(), &auth.AuthContext{Subject: "owner"}))
	statusResult := httptest.NewRecorder()
	h.getRunInputStatus(statusResult, status)
	if statusResult.Code != http.StatusOK || !strings.Contains(statusResult.Body.String(), `"processed"`) {
		t.Fatalf("input status: %d %s", statusResult.Code, statusResult.Body.String())
	}
	status = status.WithContext(auth.NewContext(status.Context(), &auth.AuthContext{Subject: "other"}))
	other := httptest.NewRecorder()
	h.getRunInputStatus(other, status)
	if other.Code != http.StatusNotFound {
		t.Fatalf("another owner could read input status: %d", other.Code)
	}
}
