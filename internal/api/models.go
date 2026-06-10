package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/angoo/agentfoundry/internal/store"
)

type modelCacheEntry struct {
	data   json.RawMessage
	expiry time.Time
}

var (
	modelCache sync.Map
	cacheTTL   = 5 * time.Minute
)

type openRouterModel struct {
	ID                  string          `json:"id"`
	SupportedParameters []string        `json:"supported_parameters"`
	DefaultParameters   json.RawMessage `json:"default_parameters"`
}

type openRouterModelsResponse struct {
	Data []openRouterModel `json:"data"`
}

type modelCapabilitiesResponse struct {
	SupportedParameters []string          `json:"supported_parameters"`
	DefaultParameters   json.RawMessage   `json:"default_parameters"`
}

func (h *Handler) getModelCapabilities(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("model")
	if modelID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}

	var prov *store.ProviderRecord
	if pid := r.URL.Query().Get("provider_id"); pid != "" {
		var err error
		prov, err = h.providerStore.GetByID(r.Context(), pid)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider not found"})
			return
		}
	} else {
		provs, err := h.providerStore.ListAll(r.Context())
		if err != nil {
			slog.Error("failed to list providers for model capabilities", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list providers"})
			return
		}
		for i := range provs {
			if provs[i].ProviderType == "openrouter" {
				p := provs[i]
				prov = &p
				break
			}
		}
		if prov == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no OpenRouter provider found, specify provider_id"})
			return
		}
	}

	baseURL := prov.BaseURL
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	cacheKey := baseURL + "|" + modelID
	if entry, ok := modelCache.Load(cacheKey); ok {
		ce := entry.(modelCacheEntry)
		if time.Now().Before(ce.expiry) {
			writeJSON(w, http.StatusOK, ce.data)
			return
		}
		modelCache.Delete(cacheKey)
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET", baseURL+"/models", nil)
	if err != nil {
		slog.Error("failed to create models request", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query models"})
		return
	}
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("failed to fetch models from provider", "provider", prov.Name, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "provider unreachable"})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("failed to read models response", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read response"})
		return
	}

	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("provider returned %d: %s", resp.StatusCode, string(body))})
		return
	}

	var modelsResp openRouterModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		slog.Error("failed to parse models response", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to parse models"})
		return
	}

	for _, m := range modelsResp.Data {
		if m.ID == modelID {
			result := modelCapabilitiesResponse{
				SupportedParameters: m.SupportedParameters,
				DefaultParameters:   m.DefaultParameters,
			}
			data, _ := json.Marshal(result)
			modelCache.Store(cacheKey, modelCacheEntry{data: data, expiry: time.Now().Add(cacheTTL)})
			writeJSON(w, http.StatusOK, result)
			return
		}
	}

	writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("model %q not found", modelID)})
}
