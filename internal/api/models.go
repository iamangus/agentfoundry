package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
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
	SupportedParameters []string        `json:"supported_parameters"`
	DefaultParameters   json.RawMessage `json:"default_parameters"`
	ContextLength       int             `json:"context_length,omitempty"`
}

type llamaPropsResponse struct {
	DefaultGenerationSettings struct {
		N_ctx  int            `json:"n_ctx"`
		Params map[string]any `json:"params"`
	} `json:"default_generation_settings"`
	ChatTemplate     string         `json:"chat_template"`
	ChatTemplateCaps map[string]any `json:"chat_template_caps"`
}

// llamaSkipParams lists /props generation params that are not user-tunable
// sampling parameters (transport flags, internal counters, or structured values
// handled separately).
var llamaSkipParams = map[string]bool{
	"id":                true,
	"n_ctx":             true,
	"is_processing":     true,
	"speculative":       true,
	"timings_per_token": true,
	"stream":            true,
	"n_probs":           true,
	"min_keep":          true,
}

var orSuffixes = []string{":nitro", ":free", ":floor", ":online", ":thinking", ":extended", ":exacto"}

func stripORSuffix(modelID string) string {
	for {
		stripped := modelID
		for _, sfx := range orSuffixes {
			stripped = strings.TrimSuffix(stripped, sfx)
		}
		if stripped == modelID {
			return stripped
		}
		modelID = stripped
	}
}

func loadModelCache(key string) (*modelCapabilitiesResponse, bool) {
	if entry, ok := modelCache.Load(key); ok {
		ce := entry.(modelCacheEntry)
		if time.Now().Before(ce.expiry) {
			var result modelCapabilitiesResponse
			if err := json.Unmarshal(ce.data, &result); err == nil {
				return &result, true
			}
		}
		modelCache.Delete(key)
	}
	return nil, false
}

func storeModelCache(key string, result *modelCapabilitiesResponse) {
	data, _ := json.Marshal(result)
	modelCache.Store(key, modelCacheEntry{data: data, expiry: time.Now().Add(cacheTTL)})
}

func (h *Handler) getModelCapabilities(w http.ResponseWriter, r *http.Request) {
	modelID := r.URL.Query().Get("model")
	if modelID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	modelID = stripORSuffix(modelID)

	var prov *store.ProviderRecord
	if pid := r.URL.Query().Get("provider_id"); pid != "" {
		var err error
		prov, err = h.providerStore.GetByID(r.Context(), pid)
		if err != nil {
			slog.Error("model capabilities: provider not found", "provider_id", pid, "error", err)
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

	switch prov.ProviderType {
	case "llama-server":
		h.capsFromLlamaProps(w, r, prov, baseURL)
	case "openrouter":
		h.capsFromOpenRouter(w, r, prov, baseURL, modelID, false)
	default:
		// OpenAI-compatible providers (openai, anthropic, ollama, custom, "").
		// Try OpenRouter-style /models first, then fall back to llama.cpp /props.
		if !h.capsFromOpenRouter(w, r, prov, baseURL, modelID, true) {
			h.capsFromLlamaProps(w, r, prov, baseURL)
		}
	}
}

// capsFromOpenRouter fetches the OpenRouter-style /models endpoint. It writes
// the HTTP response and returns true when a definitive response was written.
// When fallback is true, a non-definitive result (transport error, unsupported
// format, missing model, or empty capabilities) returns false so the caller can
// probe /props instead.
func (h *Handler) capsFromOpenRouter(w http.ResponseWriter, r *http.Request, prov *store.ProviderRecord, baseURL, modelID string, fallback bool) bool {
	cacheKey := baseURL + "|" + modelID
	if result, ok := loadModelCache(cacheKey); ok {
		writeJSON(w, http.StatusOK, result)
		return true
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		slog.Error("failed to create models request", "error", err)
		if fallback {
			return false
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query models"})
		return true
	}
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("failed to fetch models from provider", "provider", prov.Name, "error", err)
		if fallback {
			return false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "provider unreachable"})
		return true
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("failed to read models response", "error", err)
		if fallback {
			return false
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read response"})
		return true
	}

	if resp.StatusCode != http.StatusOK {
		if fallback {
			return false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("provider returned %d: %s", resp.StatusCode, string(body))})
		return true
	}

	var modelsResp openRouterModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		slog.Error("failed to parse models response", "error", err)
		if fallback {
			return false
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to parse models"})
		return true
	}

	for _, m := range modelsResp.Data {
		if m.ID == modelID {
			if fallback && len(m.SupportedParameters) == 0 {
				// OpenAI-format /models (e.g. llama-server /v1/models) carry no
				// supported_parameters; let the caller probe /props.
				return false
			}
			result := &modelCapabilitiesResponse{
				SupportedParameters: m.SupportedParameters,
				DefaultParameters:   m.DefaultParameters,
			}
			storeModelCache(cacheKey, result)
			writeJSON(w, http.StatusOK, result)
			return true
		}
	}

	if fallback {
		return false
	}
	slog.Error("model capabilities: model not found in provider list", "model", modelID)
	writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("model %q not found", modelID)})
	return true
}

// capsFromLlamaProps fetches llama.cpp /props and maps the default generation
// settings into a modelCapabilitiesResponse.
func (h *Handler) capsFromLlamaProps(w http.ResponseWriter, r *http.Request, prov *store.ProviderRecord, baseURL string) {
	root := llamaPropsRoot(baseURL)
	cacheKey := root + "|props"
	if result, ok := loadModelCache(cacheKey); ok {
		writeJSON(w, http.StatusOK, result)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, root+"/props", nil)
	if err != nil {
		slog.Error("failed to create props request", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query provider"})
		return
	}
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("failed to fetch props from provider", "provider", prov.Name, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "provider unreachable"})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("failed to read props response", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read response"})
		return
	}

	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "model capabilities not available from provider"})
		return
	}

	var props llamaPropsResponse
	if err := json.Unmarshal(body, &props); err != nil || props.DefaultGenerationSettings.Params == nil {
		slog.Error("failed to parse props response", "error", err)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "model capabilities not available from provider"})
		return
	}

	result := propsToCapabilities(&props)
	storeModelCache(cacheKey, &result)
	writeJSON(w, http.StatusOK, result)
}

// llamaPropsRoot derives the llama.cpp server root from a base URL. llama.cpp
// serves its OpenAI-compatible endpoints under /v1 but its native endpoints
// (including /props) at the server root.
func llamaPropsRoot(baseURL string) string {
	root := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(root, "/v1") {
		root = strings.TrimSuffix(root, "/v1")
	}
	return root
}

func propsToCapabilities(props *llamaPropsResponse) modelCapabilitiesResponse {
	params := props.DefaultGenerationSettings.Params
	supported := make([]string, 0, len(params))
	defaults := make(map[string]any, len(params))
	for k, v := range params {
		if strings.Contains(k, ".") {
			continue
		}
		if llamaSkipParams[k] {
			continue
		}
		switch v.(type) {
		case float64, bool, string:
		default:
			// Arrays and nested objects (samplers, stop, dry_sequence_breakers)
			// are not simple scalar parameters.
			continue
		}
		supported = append(supported, k)
		defaults[k] = v
	}
	sort.Strings(supported)

	defRaw, _ := json.Marshal(defaults)
	return modelCapabilitiesResponse{
		SupportedParameters: supported,
		DefaultParameters:   defRaw,
		ContextLength:       props.DefaultGenerationSettings.N_ctx,
	}
}
