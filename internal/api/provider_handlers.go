package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/store"
)

type createProviderRequest struct {
	Name             string            `json:"name"`
	ProviderType     string            `json:"provider_type"`
	BaseURL          string            `json:"base_url"`
	APIKey           string            `json:"api_key"`
	DefaultModel     string            `json:"default_model"`
	SchemaValidation bool              `json:"schema_validation"`
	Headers          map[string]string `json:"headers"`
	Reasoning        json.RawMessage   `json:"reasoning,omitempty"`
	Scope            string            `json:"scope"`
	Team             string            `json:"team,omitempty"`
}

type updateProviderRequest struct {
	Name             string            `json:"name"`
	ProviderType     string            `json:"provider_type"`
	BaseURL          string            `json:"base_url"`
	APIKey           string            `json:"api_key"`
	DefaultModel     string            `json:"default_model"`
	SchemaValidation bool              `json:"schema_validation"`
	Headers          map[string]string `json:"headers"`
	Reasoning        json.RawMessage   `json:"reasoning,omitempty"`
	Scope            string            `json:"scope"`
	Team             string            `json:"team,omitempty"`
}

func (h *Handler) createProvider(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req createProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	scope := req.Scope
	if scope == "" {
		scope = "user"
	}

	if scope == "global" && !ac.IsGlobalAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only global admins can create global providers"})
		return
	}

	if scope == "team" && !ac.IsGlobalAdmin && !ac.IsTeamAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "team admin or global admin required for team-scoped providers"})
		return
	}

	rec, err := h.providerStore.Create(r.Context(), req.Name, req.ProviderType, req.BaseURL, req.APIKey, req.DefaultModel, req.SchemaValidation, scope, req.Team, ac.Subject, req.Headers, req.Reasoning)
	if err != nil {
		slog.Error("failed to create provider", "name", req.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	rec = maskProvider(rec)
	writeJSON(w, http.StatusCreated, rec)
}

func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	recs, err := h.providerStore.List(r.Context(), ac.Subject, ac.Teams, ac.IsGlobalAdmin)
	if err != nil {
		slog.Error("failed to list providers", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	for i := range recs {
		recs[i] = *maskProvider(&recs[i])
	}

	writeJSON(w, http.StatusOK, recs)
}

func (h *Handler) getProvider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	rec, err := h.providerStore.GetByName(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	rec = maskProvider(rec)
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) updateProvider(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	id := r.PathValue("id")

	existing, err := h.providerStore.GetByID(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	canEdit, _, err := h.providerStore.CanEdit(r.Context(), ac.Subject, ac.Teams, ac.IsGlobalAdmin, ac.IsTeamAdmin, existing.Name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if !canEdit {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you do not have permission to edit this provider"})
		return
	}

	var req updateProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	if req.Name == "" {
		req.Name = existing.Name
	}

	if req.APIKey == "***" || req.APIKey == "" {
		req.APIKey = existing.APIKey
	}

	rec, err := h.providerStore.Update(r.Context(), id, req.Name, req.ProviderType, req.BaseURL, req.APIKey, req.DefaultModel, req.SchemaValidation, req.Scope, req.Team, req.Headers, req.Reasoning)
	if err != nil {
		slog.Error("failed to update provider", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	rec = maskProvider(rec)
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	id := r.PathValue("id")

	existing, err := h.providerStore.GetByID(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	canDelete, err := h.providerStore.CanDelete(ac.Subject, ac.Teams, ac.IsGlobalAdmin, ac.IsTeamAdmin, existing.Name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if !canDelete {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you do not have permission to delete this provider"})
		return
	}

	if err := h.providerStore.Delete(r.Context(), id); err != nil {
		slog.Error("failed to delete provider", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusNoContent, nil)
}

func maskProvider(rec *store.ProviderRecord) *store.ProviderRecord {
	for k := range rec.Headers {
		if k == "api-key" || k == "authorization" {
			rec.Headers[k] = "***"
		}
	}
	if rec.APIKey != "" {
		rec.APIKey = "***"
	}
	return rec
}
