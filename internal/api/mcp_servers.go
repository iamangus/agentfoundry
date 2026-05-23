package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/mcpclient"
)

type createMCPServerRequest struct {
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Transport string            `json:"transport"`
	Headers   map[string]string `json:"headers,omitempty"`
	Scope     string            `json:"scope"`
	Team      string            `json:"team,omitempty"`
}

type toolInfo struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Scope        string `json:"scope"`
	ScopeSource  string `json:"scope_source"`
}

type mcpServerResponse struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Transport string            `json:"transport"`
	Headers   map[string]string `json:"headers,omitempty"`
	Scope     string            `json:"scope"`
	Team      string            `json:"team,omitempty"`
	CreatedBy string            `json:"created_by"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
	Connected bool              `json:"connected"`
	Tools     []toolInfo        `json:"tools"`
}

type setToolScopeRequest struct {
	Scope string `json:"scope"`
	Team  string `json:"team,omitempty"`
}

func (h *Handler) registerMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP server management not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req createMCPServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	if req.Name == "" || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and url are required"})
		return
	}

	if req.Transport == "" {
		req.Transport = mcpclient.TransportSSE
	}

	if req.Scope == "" {
		req.Scope = "user"
	}

	if req.Headers == nil {
		req.Headers = map[string]string{}
	}

	switch config.Scope(req.Scope) {
	case config.ScopeGlobal:
		if !ac.IsGlobalAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "global admin required"})
			return
		}
		req.Team = ""
	case config.ScopeTeam:
		if req.Team == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "team is required for team scope"})
			return
		}
		if !ac.IsMemberOfTeam(req.Team) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a member of team " + req.Team})
			return
		}
	case config.ScopeUser:
		req.Team = ""
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scope"})
		return
	}

	rec, err := h.mcpStore.Create(r.Context(), req.Name, req.URL, req.Transport, req.Scope, req.Team, ac.Subject, req.Headers)
	if err != nil {
		if err == auth.ErrMCPServerNameTaken {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "server with this name already exists"})
			return
		}
		slog.Error("failed to create mcp server", "name", req.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create server"})
		return
	}

	srvCfg := mcpclient.ServerConfig{
		Name:      rec.Name,
		URL:       rec.URL,
		Transport: rec.Transport,
		Headers:   rec.Headers,
	}
	if err := h.pool.ConnectDynamic(r.Context(), srvCfg); err != nil {
		slog.Warn("mcp server registered but failed to connect", "name", rec.Name, "error", err)
	}

	resp := toMCPServerResponse(*rec, h.pool, h.mcpStore)
	writeJSON(w, http.StatusCreated, resp)
}

func (h *Handler) listMCPServers(w http.ResponseWriter, r *http.Request) {
	if h.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP server management not available"})
		return
	}

	ac := auth.FromContext(r)
	var subject string
	var teams []string
	var isGlobalAdmin bool
	if ac != nil {
		subject = ac.Subject
		teams = ac.Teams
		isGlobalAdmin = ac.IsGlobalAdmin
	}

	records, err := h.mcpStore.List(r.Context(), subject, teams, isGlobalAdmin)
	if err != nil {
		slog.Error("failed to list mcp servers", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list servers"})
		return
	}

	resp := make([]mcpServerResponse, len(records))
	for i, rec := range records {
		resp[i] = toMCPServerResponse(rec, h.pool, h.mcpStore)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) getMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP server management not available"})
		return
	}

	ac := auth.FromContext(r)
	serverName := r.PathValue("name")

	rec, err := h.mcpStore.GetByName(r.Context(), serverName)
	if err != nil {
		if err == auth.ErrMCPServerNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			return
		}
		slog.Error("failed to get mcp server", "name", serverName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get server"})
		return
	}

	if ac != nil && !visibleToTool(rec.Scope, rec.Team, rec.CreatedBy, ac.Subject, ac.Teams, ac.IsGlobalAdmin) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
		return
	}

	resp := toMCPServerResponse(*rec, h.pool, h.mcpStore)
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) updateMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP server management not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	serverName := r.PathValue("name")

	var req createMCPServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	ok, existing, err := h.mcpStore.CanEdit(ac.Subject, ac.Teams, ac.IsGlobalAdmin, ac.IsTeamAdmin, serverName)
	if err != nil {
		if err == auth.ErrMCPServerNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			return
		}
		slog.Error("failed to check edit permission", "name", serverName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check permissions"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access denied"})
		return
	}

	if req.Name == "" || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and url are required"})
		return
	}
	if req.Transport == "" {
		req.Transport = mcpclient.TransportSSE
	}
	if req.Scope == "" {
		req.Scope = existing.Scope
	}
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}

	switch config.Scope(req.Scope) {
	case config.ScopeGlobal:
		if !ac.IsGlobalAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "global admin required"})
			return
		}
		req.Team = ""
	case config.ScopeTeam:
		if req.Team == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "team is required for team scope"})
			return
		}
		if !ac.IsMemberOfTeam(req.Team) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a member of team " + req.Team})
			return
		}
	case config.ScopeUser:
		req.Team = ""
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scope"})
		return
	}

	h.pool.DisconnectDynamic(serverName)

	rec, err := h.mcpStore.Update(r.Context(), serverName, req.URL, req.Transport, req.Scope, req.Team, req.Headers)
	if err != nil {
		slog.Error("failed to update mcp server", "name", serverName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update server"})
		return
	}

	srvCfg := mcpclient.ServerConfig{
		Name:      rec.Name,
		URL:       rec.URL,
		Transport: rec.Transport,
		Headers:   rec.Headers,
	}
	if err := h.pool.ConnectDynamic(r.Context(), srvCfg); err != nil {
		slog.Warn("mcp server updated but failed to connect", "name", rec.Name, "error", err)
	}

	resp := toMCPServerResponse(*rec, h.pool, h.mcpStore)
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) deleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP server management not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	serverName := r.PathValue("name")

	ok, _, err := h.mcpStore.CanEdit(ac.Subject, ac.Teams, ac.IsGlobalAdmin, ac.IsTeamAdmin, serverName)
	if err != nil {
		if err == auth.ErrMCPServerNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			return
		}
		slog.Error("failed to check edit permission", "name", serverName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check permissions"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access denied"})
		return
	}

	h.pool.DisconnectDynamic(serverName)

	if err := h.mcpStore.Delete(r.Context(), serverName); err != nil {
		slog.Error("failed to delete mcp server", "name", serverName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete server"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) setToolScope(w http.ResponseWriter, r *http.Request) {
	if h.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP server management not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	serverName := r.PathValue("name")
	toolName := r.PathValue("tool")

	ok, rec, err := h.mcpStore.CanEdit(ac.Subject, ac.Teams, ac.IsGlobalAdmin, ac.IsTeamAdmin, serverName)
	if err != nil {
		if err == auth.ErrMCPServerNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			return
		}
		slog.Error("failed to check edit permission", "name", serverName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check permissions"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access denied"})
		return
	}

	var req setToolScopeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	switch config.Scope(req.Scope) {
	case config.ScopeGlobal:
		if !ac.IsGlobalAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "global admin required"})
			return
		}
		req.Team = ""
	case config.ScopeTeam:
		if req.Team == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "team is required for team scope"})
			return
		}
		if !ac.IsMemberOfTeam(req.Team) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a member of team " + req.Team})
			return
		}
	case config.ScopeUser:
		req.Team = ""
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scope"})
		return
	}

	if err := h.mcpStore.SetToolScope(r.Context(), rec.ID, toolName, req.Scope, req.Team, ac.Subject); err != nil {
		slog.Error("failed to set tool scope", "server", serverName, "tool", toolName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to set tool scope"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "tool scope updated"})
}

func (h *Handler) refreshMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.mcpStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MCP server management not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	serverName := r.PathValue("name")

	_, err := h.mcpStore.GetByName(r.Context(), serverName)
	if err != nil {
		if err == auth.ErrMCPServerNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "mcp server not found"})
			return
		}
		slog.Error("failed to get mcp server", "name", serverName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get server"})
		return
	}

	h.pool.RefreshServer(serverName)

	writeJSON(w, http.StatusOK, map[string]string{"status": "refresh triggered"})
}

func toMCPServerResponse(rec auth.MCPServerRecord, pool *mcpclient.Pool, store *auth.MCPServerStore) mcpServerResponse {
	import_context := context.Background()

	status := pool.GetServerStatus(rec.Name)

	tools := make([]toolInfo, 0)
	for _, t := range status.Tools {
		scope := rec.Scope
		scopeSource := "server"

		if store != nil && import_context != nil {
			toolScopes, err := store.GetToolScopes(import_context, rec.ID)
			if err == nil {
				if ts, ok := toolScopes[t.Name]; ok {
					scope = ts.Scope
					scopeSource = "tool"
				}
			}
		}

		tools = append(tools, toolInfo{
			Name:        t.Name,
			Description: t.Description,
			Scope:       scope,
			ScopeSource: scopeSource,
		})
	}

	return mcpServerResponse{
		ID:        rec.ID,
		Name:      rec.Name,
		URL:       rec.URL,
		Transport: rec.Transport,
		Headers:   rec.Headers,
		Scope:     rec.Scope,
		Team:      rec.Team,
		CreatedBy: rec.CreatedBy,
		CreatedAt: rec.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt: rec.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		Connected: status.Connected,
		Tools:     tools,
	}
}

func visibleToTool(scope, team, createdBy, subject string, teams []string, isGlobalAdmin bool) bool {
	switch scope {
	case "global":
		return true
	case "team":
		if isGlobalAdmin {
			return true
		}
		for _, t := range teams {
			if t == team {
				return true
			}
		}
		return false
	case "user", "":
		return subject != "" && createdBy == subject
	default:
		return false
	}
}
