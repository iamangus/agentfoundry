package api

import (
	"encoding/json"
	"net/http"

	"github.com/angoo/agentfoundry/internal/run"
)

type streamTokenRequest struct {
	Token string `json:"token"`
}

func (h *Handler) publishStreamToken(w http.ResponseWriter, r *http.Request) {
	streamID := r.PathValue("id")
	if streamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stream id is required"})
		return
	}

	var req streamTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	h.streams.PublishToken(streamID, req.Token)
	w.WriteHeader(http.StatusNoContent)
}

type streamEventRequest struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

func (h *Handler) publishStreamEvent(w http.ResponseWriter, r *http.Request) {
	streamID := r.PathValue("id")
	if streamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stream id is required"})
		return
	}

	var req streamEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Type == "turn_done" {
		// Complete this SSE turn while leaving the logical persistent run alive.
		h.streams.PublishDone(streamID, req.Data)
		_ = h.runs.UpdateStatus(streamID, run.StatusWaiting, req.Data, "")
	} else if req.Type == "turn_error" {
		h.streams.PublishError(streamID, "Error: "+req.Data)
		_ = h.runs.UpdateStatus(streamID, run.StatusWaiting, "", req.Data)
	} else {
		h.streams.PublishEvent(streamID, req.Type, req.Data)
	}
	w.WriteHeader(http.StatusNoContent)
}
