package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/store"
)

func profileVisible(p *store.InferenceProfile, ac *auth.AuthContext) bool {
	return ac != nil && (ac.IsGlobalAdmin || p.Scope == "global" || p.Scope == "team" && ac.IsMemberOfTeam(p.Team) || p.Scope == "user" && p.CreatedBy == ac.Subject)
}

func profileEditable(p *store.InferenceProfile, ac *auth.AuthContext) bool {
	return ac != nil && (ac.IsGlobalAdmin || p.Scope == "team" && ac.CanManageTeamAgent(p.Team) || p.Scope == "user" && p.CreatedBy == ac.Subject)
}

func (h *Handler) listInferenceProfiles(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	profiles, err := h.providerStore.ListProfiles(r.Context(), ac.Subject, ac.Teams, ac.IsGlobalAdmin)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to list inference profiles"})
		return
	}
	writeJSON(w, 200, profiles)
}

func (h *Handler) saveInferenceProfile(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	var p store.InferenceProfile
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&p); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	p.ID = r.PathValue("id")
	p.CreatedBy = ac.Subject
	if p.ID != "" {
		existing, err := h.providerStore.GetProfile(r.Context(), p.ID)
		if err != nil || !profileVisible(existing, ac) {
			writeJSON(w, 404, map[string]string{"error": "profile not found"})
			return
		}
		if !profileEditable(existing, ac) {
			writeJSON(w, 403, map[string]string{"error": "cannot edit this profile"})
			return
		}
		p.CreatedBy = existing.CreatedBy
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Scope == "" {
		p.Scope = "user"
	}
	if p.Scope != "team" {
		p.Team = ""
	}
	if p.Scope != "user" && p.Scope != "team" && p.Scope != "global" {
		writeJSON(w, 400, map[string]string{"error": "invalid scope"})
		return
	}
	if p.Scope == "global" && !ac.IsGlobalAdmin || p.Scope == "team" && (p.Team == "" || !ac.CanManageTeamAgent(p.Team)) {
		writeJSON(w, 403, map[string]string{"error": "not allowed to manage profiles in this scope"})
		return
	}
	if err := h.validateInferenceProfile(r, &p); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	saved, err := h.providerStore.SaveProfile(r.Context(), &p)
	if err != nil {
		slog.Error("save inference profile", "error", err)
		writeJSON(w, 500, map[string]string{"error": "failed to save inference profile"})
		return
	}
	status := http.StatusOK
	if p.ID == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, saved)
}

func (h *Handler) validateInferenceProfile(r *http.Request, p *store.InferenceProfile) error {
	if p.Name == "" || p.ProviderID == "" {
		return fmt.Errorf("name and primary provider are required")
	}
	ac := auth.FromContext(r)
	providers, err := h.providerStore.List(r.Context(), ac.Subject, ac.Teams, ac.IsGlobalAdmin)
	if err != nil {
		return fmt.Errorf("could not validate providers")
	}
	resolve := func(id, model string) (string, error) {
		for _, provider := range providers {
			if provider.ID != id {
				continue
			}
			if !backupScopeCompatible(p.Scope, p.Team, provider.Scope, provider.Team) || provider.Scope == "user" && provider.CreatedBy != p.CreatedBy {
				return "", fmt.Errorf("provider must be available to the profile's scope and owner")
			}
			model = strings.TrimSpace(model)
			if model == "" {
				model = provider.DefaultModel
			}
			if model == "" {
				return "", fmt.Errorf("choose a model or configure the provider's default model")
			}
			return model, nil
		}
		return "", fmt.Errorf("provider not found or access denied")
	}
	p.Model, err = resolve(p.ProviderID, p.Model)
	if err != nil {
		return err
	}
	if p.BackupProviderID == "" {
		p.BackupModel = ""
		return nil
	}
	p.BackupModel, err = resolve(p.BackupProviderID, p.BackupModel)
	if err != nil {
		return err
	}
	if p.ProviderID == p.BackupProviderID && p.Model == p.BackupModel {
		return fmt.Errorf("backup must use a different provider or model from the primary")
	}
	return nil
}

func (h *Handler) deleteInferenceProfile(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	p, err := h.providerStore.GetProfile(r.Context(), r.PathValue("id"))
	if err != nil || !profileVisible(p, ac) {
		writeJSON(w, 404, map[string]string{"error": "profile not found"})
		return
	}
	if !profileEditable(p, ac) {
		writeJSON(w, 403, map[string]string{"error": "cannot delete this profile"})
		return
	}
	if err := h.providerStore.DeleteProfile(r.Context(), p.ID); err != nil {
		writeJSON(w, 500, map[string]string{"error": "failed to delete profile"})
		return
	}
	writeJSON(w, 204, nil)
}
