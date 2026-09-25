package api

import (
	"encoding/json"
	"net/http"
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
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	InputID string `json:"input_id,omitempty"`
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

	if req.Type == "input_processed" || req.Type == "input_failed" {
		if req.InputID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "input_id is required"})
			return
		}
		status := "processed"
		if req.Type == "input_failed" {
			status = "failed"
		}
		if err := h.runs.FinishReceipt(streamID, req.InputID, status); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to record input result"})
			return
		}
	} else if req.Type == "turn_done" {
		matched, err := h.runs.FinishInput(streamID, req.InputID, req.Data, "")
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to record turn result"})
			return
		}
		if matched {
			h.streams.PublishDone(streamID, req.Data)
		}
	} else if req.Type == "turn_error" {
		matched, err := h.runs.FinishInput(streamID, req.InputID, "", req.Data)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to record turn error"})
			return
		}
		if matched {
			h.streams.PublishError(streamID, "Error: "+req.Data)
		}
	} else {
		h.streams.PublishEvent(streamID, req.Type, req.Data)
	}
	w.WriteHeader(http.StatusNoContent)
}
