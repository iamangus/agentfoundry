package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angoo/agentfoundry/internal/run"
	"github.com/angoo/agentfoundry/internal/stream"
)

func TestPersistentTurnDoneClosesOnlyTheTurnStream(t *testing.T) {
	runs := run.New()
	ru := runs.CreatePersistent("assistant", "owner")
	streams := stream.NewManager()
	streams.Create(ru.ID)
	h := &Handler{runs: runs, streams: streams}

	req := httptest.NewRequest(http.MethodPost, "/api/internal/streams/"+ru.ID+"/events", strings.NewReader(`{"type":"turn_done","data":"answer"}`))
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
