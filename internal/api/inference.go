package api

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
)

func (h *Handler) inferenceProxy(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")

	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent ID is required"})
		return
	}

	if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != h.internalAPIKey {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	def := h.store.GetDefinitionByID(agentID)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}

	if def.ProviderID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent has no provider configured"})
		return
	}

	prov, err := h.providerStore.GetByID(r.Context(), def.ProviderID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "provider not found: " + err.Error()})
		return
	}

	if prov.BaseURL == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "provider has no base URL"})
		return
	}

	baseURL := strings.TrimRight(prov.BaseURL, "/")
	targetURL := baseURL + "/chat/completions"

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, r.Body)
	if err != nil {
		slog.Error("failed to create proxy request", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy error"})
		return
	}

	req.Header.Set("Content-Type", "application/json")
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}
	for k, v := range prov.Headers {
		if k != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("failed to proxy inference request", "provider", prov.Name, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "provider unreachable"})
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if flusher, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				flusher.Flush()
			}
			if err != nil {
				if err != io.EOF {
					slog.Error("inference proxy read error", "error", err)
				}
				return
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
}
