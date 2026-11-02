package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/store"
)

type availableModel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

func providerBaseURL(providerType, base string) string {
	if strings.TrimSpace(base) != "" {
		return strings.TrimRight(strings.TrimSpace(base), "/")
	}
	switch providerType {
	case "deepseek":
		return "https://api.deepseek.com"
	case "openai":
		return "https://api.openai.com/v1"
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	case "anthropic":
		return "https://api.anthropic.com/v1"
	case "ollama":
		return "http://localhost:11434/v1"
	}
	return ""
}

func (h *Handler) visibleProvider(r *http.Request, id string) (*store.ProviderRecord, error) {
	ac := auth.FromContext(r)
	if ac == nil {
		return nil, fmt.Errorf("unauthorized")
	}
	providers, err := h.providerStore.List(r.Context(), ac.Subject, ac.Teams, ac.IsGlobalAdmin)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		if p.ID == id {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("provider not found")
}

func (h *Handler) listProviderModels(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r) == nil {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	p, err := h.visibleProvider(r, r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "provider not found"})
		return
	}
	h.writeAvailableModels(w, r, p)
}

// Draft discovery does not persist the provider or expose credentials to the UI.
func (h *Handler) discoverModels(w http.ResponseWriter, r *http.Request) {
	if auth.FromContext(r) == nil {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var p store.ProviderRecord
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&p); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if p.ID != "" {
		saved, err := h.visibleProvider(r, p.ID)
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": "provider not found"})
			return
		}
		// Never forward saved secrets to a caller-selected destination.
		if providerBaseURL(p.ProviderType, p.BaseURL) != providerBaseURL(saved.ProviderType, saved.BaseURL) || p.ProviderType != saved.ProviderType {
			if p.APIKey == "" || p.APIKey == "***" {
				writeJSON(w, 400, map[string]string{"error": "enter an API key to query a changed provider URL or type"})
				return
			}
		} else {
			if p.APIKey == "" || p.APIKey == "***" {
				p.APIKey = saved.APIKey
			}
			for k, v := range p.Headers {
				if v == "***" {
					p.Headers[k] = saved.Headers[k]
				}
			}
		}
	}
	if p.APIKey == "***" {
		p.APIKey = ""
	}
	for k, v := range p.Headers {
		if v == "***" {
			delete(p.Headers, k)
		}
	}
	h.writeAvailableModels(w, r, &p)
}

func (h *Handler) writeAvailableModels(w http.ResponseWriter, r *http.Request, p *store.ProviderRecord) {
	models, err := fetchAvailableModels(r, p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"data": models})
}

func fetchAvailableModels(r *http.Request, p *store.ProviderRecord) ([]availableModel, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	base := providerBaseURL(p.ProviderType, p.BaseURL)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("a valid HTTP(S) provider base URL is required")
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	models := []availableModel{}
	seen := map[string]bool{}
	after := ""
	for page := 0; page < 100; page++ {
		target := base + "/models"
		if after != "" {
			target += "?after_id=" + url.QueryEscape(after)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("invalid provider URL")
		}
		if p.APIKey != "" {
			if p.ProviderType == "anthropic" {
				req.Header.Set("x-api-key", p.APIKey)
			} else {
				req.Header.Set("Authorization", "Bearer "+p.APIKey)
			}
		}
		if p.ProviderType == "anthropic" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		for k, v := range p.Headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("model discovery failed: provider unreachable or request timed out; you can still enter a model manually")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("model discovery returned HTTP %d; check credentials and base URL, or enter a model manually", resp.StatusCode)
		}
		if readErr != nil || len(body) > 8*1024*1024 {
			return nil, fmt.Errorf("model discovery response could not be read or was too large")
		}
		var data struct {
			Data []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &data); err != nil || data.Data == nil {
			return nil, fmt.Errorf("provider does not expose a supported model list; enter a model manually")
		}
		for _, m := range data.Data {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			if m.Name == "" {
				m.Name = m.DisplayName
			}
			models = append(models, availableModel{ID: m.ID, Name: m.Name})
		}
		if !data.HasMore || p.ProviderType != "anthropic" {
			sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
			return models, nil
		}
		if data.LastID == "" || data.LastID == after {
			return nil, fmt.Errorf("provider returned invalid model pagination")
		}
		after = data.LastID
	}
	return nil, fmt.Errorf("provider model list exceeded pagination limit")
}
