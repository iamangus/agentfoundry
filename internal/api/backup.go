package api

import (
	"fmt"
	"net/http"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/config"
)

func (h *Handler) validateBackupProvider(r *http.Request, def *config.Definition) error {
	if def.BackupProviderID == "" {
		return nil
	}
	if h.providerStore == nil {
		return fmt.Errorf("backup provider is unavailable")
	}
	ac := auth.FromContext(r)
	if ac == nil {
		return fmt.Errorf("authentication required")
	}
	providers, err := h.providerStore.List(r.Context(), ac.Subject, ac.Teams, ac.IsGlobalAdmin)
	if err != nil {
		return fmt.Errorf("could not validate backup provider")
	}
	for _, p := range providers {
		if p.ID != def.BackupProviderID {
			continue
		}
		if !backupScopeCompatible(def.Scope, def.Team, p.Scope, p.Team) {
			return fmt.Errorf("backup provider must be available to the agent's scope")
		}
		model := def.BackupModel
		if model == "" {
			model = p.DefaultModel
		}
		if model == "" {
			return fmt.Errorf("choose a backup model or configure the provider's default model")
		}
		primaryModel := def.Model
		if def.ProviderID == p.ID && primaryModel == "" {
			primaryModel = p.DefaultModel
		}
		if def.ProviderID == p.ID && primaryModel == model {
			return fmt.Errorf("backup must use a different provider or model from the primary")
		}
		return nil
	}
	return fmt.Errorf("backup provider not found or access denied")
}

func backupScopeCompatible(agentScope, agentTeam, providerScope, providerTeam string) bool {
	if providerScope == "global" {
		return true
	}
	if agentScope == "global" {
		return false
	}
	if agentScope == "team" {
		return providerScope == "team" && agentTeam == providerTeam
	}
	return true
}
