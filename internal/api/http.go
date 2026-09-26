package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/llm"
	"github.com/angoo/agentfoundry/internal/mcpclient"
	"github.com/angoo/agentfoundry/internal/registry"
	"github.com/angoo/agentfoundry/internal/run"
	"github.com/angoo/agentfoundry/internal/session"
	"github.com/angoo/agentfoundry/internal/store"
	"github.com/angoo/agentfoundry/internal/stream"
	"github.com/angoo/agentfoundry/internal/temporal"
)

type DefinitionStore interface {
	SaveDefinition(def *config.Definition) error
	DeleteDefinition(name string) error
	GetDefinition(name string) *config.Definition
	GetDefinitionByID(agentID string) *config.Definition
	ListDefinitions() []*config.Definition
	GetRawDefinition(name string) ([]byte, error)
}

type VersionedStore interface {
	DefinitionStore
	ListVersions(ctx context.Context, name string) ([]config.AgentVersion, error)
	GetVersion(ctx context.Context, name, versionID string) ([]byte, *config.Definition, error)
	Rollback(ctx context.Context, name, versionID string) error
}

type Handler struct {
	store          DefinitionStore
	reg            *registry.Registry
	pool           *mcpclient.Pool
	temporal       *temporal.Client
	streams        *stream.Manager
	sessions       *session.Store
	keyStore       *auth.APIKeyStore
	mcpStore       *auth.MCPServerStore
	runs           *run.Store
	providerStore  *store.ProviderStore
	internalAPIKey string
}

func NewHandler(reg *registry.Registry, pool *mcpclient.Pool, store DefinitionStore, temporalClient *temporal.Client, streams *stream.Manager, sessions *session.Store, keyStore *auth.APIKeyStore, mcpStore *auth.MCPServerStore, runs *run.Store, providerStore *store.ProviderStore, internalAPIKey string) *Handler {
	return &Handler{
		store:          store,
		reg:            reg,
		pool:           pool,
		temporal:       temporalClient,
		streams:        streams,
		sessions:       sessions,
		keyStore:       keyStore,
		mcpStore:       mcpStore,
		runs:           runs,
		providerStore:  providerStore,
		internalAPIKey: internalAPIKey,
	}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents", h.listAgents)
	mux.HandleFunc("GET /api/v1/agent/{agentID}", h.getAgentByID)
	mux.HandleFunc("GET /api/v1/agents/{name}", h.getAgent)
	mux.HandleFunc("GET /api/v1/agents/{name}/raw", h.getRawAgent)
	mux.HandleFunc("GET /api/v1/agents/{name}/versions", h.listVersions)
	mux.HandleFunc("GET /api/v1/agents/{name}/version", h.getVersion)
	mux.HandleFunc("POST /api/v1/agents/{name}/rollback", h.rollbackVersion)
	mux.HandleFunc("POST /api/v1/agents", h.createAgent)
	mux.HandleFunc("PUT /api/v1/agents/{name}", h.updateAgent)
	mux.HandleFunc("DELETE /api/v1/agents/{name}", h.deleteAgent)
	mux.HandleFunc("POST /api/v1/agents/{name}/run", h.runAgent)
	mux.HandleFunc("POST /api/v1/agents/{agentID}/runs", h.startPersistentRun)
	mux.HandleFunc("GET /api/v1/runs", h.listRunsByTaskID)
	mux.HandleFunc("GET /api/v1/runs/{id}", h.getRunStatus)
	mux.HandleFunc("GET /api/v1/runs/{id}/inputs/{inputID}", h.getRunInputStatus)
	mux.HandleFunc("GET /api/v1/runs/{id}/trace", h.getRunTrace)
	mux.HandleFunc("POST /api/v1/runs/{id}/inputs", h.submitPersistentInput)
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", h.cancelRun)
	mux.HandleFunc("GET /api/v1/runs/{id}/events", h.runEvents)
	mux.HandleFunc("GET /api/v1/tools", h.listTools)
	mux.HandleFunc("GET /api/v1/status", h.getStatus)
	mux.HandleFunc("POST /api/internal/mcp/call", h.mcpProxyCall)
	mux.HandleFunc("POST /api/internal/streams/{id}/tokens", h.publishStreamToken)
	mux.HandleFunc("POST /api/internal/streams/{id}/events", h.publishStreamEvent)

	mux.HandleFunc("POST /api/v1/chat/sessions", h.createChatSession)
	mux.HandleFunc("GET /api/v1/chat/sessions", h.listChatSessions)
	mux.HandleFunc("GET /api/v1/chat/sessions/{id}", h.getChatSession)

	mux.HandleFunc("POST /api/v1/api-keys", h.createAPIKey)
	mux.HandleFunc("GET /api/v1/api-keys", h.listAPIKeys)
	mux.HandleFunc("DELETE /api/v1/api-keys/{id}", h.revokeAPIKey)

	mux.HandleFunc("POST /api/v1/mcp-servers", h.registerMCPServer)
	mux.HandleFunc("GET /api/v1/mcp-servers", h.listMCPServers)
	mux.HandleFunc("GET /api/v1/mcp-servers/{name}", h.getMCPServer)
	mux.HandleFunc("PUT /api/v1/mcp-servers/{id}", h.updateMCPServer)
	mux.HandleFunc("DELETE /api/v1/mcp-servers/{id}", h.deleteMCPServer)
	mux.HandleFunc("PUT /api/v1/mcp-servers/{id}/tools/{tool}", h.setToolScope)
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/refresh", h.refreshMCPServer)

	mux.HandleFunc("GET /api/v1/teams", h.listTeams)

	mux.HandleFunc("POST /api/v1/providers", h.createProvider)
	mux.HandleFunc("GET /api/v1/providers", h.listProviders)
	mux.HandleFunc("GET /api/v1/providers/{name}", h.getProvider)
	mux.HandleFunc("PUT /api/v1/providers/{id}", h.updateProvider)
	mux.HandleFunc("DELETE /api/v1/providers/{id}", h.deleteProvider)

	mux.HandleFunc("GET /api/v1/models/capabilities", h.getModelCapabilities)

	mux.HandleFunc("POST /api/v1/inference/agents/{agentID}/chat/completions", h.inferenceProxy)

	mux.HandleFunc("GET /api/v1/executions", h.listExecutions)
	mux.HandleFunc("GET /api/v1/executions/{workflowId}", h.getExecution)
	mux.HandleFunc("GET /api/v1/executions/{workflowId}/trace", h.getExecutionTrace)

	slog.Info("API routes registered", "prefix", "/api/v1")
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	allDefs := h.store.ListDefinitions()

	if ac == nil {
		var visible []*config.Definition
		for _, d := range allDefs {
			if d.VisibleTo("", nil, false) {
				visible = append(visible, d)
			}
		}
		writeJSON(w, http.StatusOK, visible)
		return
	}

	if !ac.IsGlobalAdmin {
		var visible []*config.Definition
		for _, d := range allDefs {
			if d.VisibleTo(ac.Subject, ac.Teams, ac.IsGlobalAdmin) {
				visible = append(visible, d)
			}
		}
		writeJSON(w, http.StatusOK, visible)
		return
	}
	writeJSON(w, http.StatusOK, allDefs)
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	name := r.PathValue("name")
	def := h.store.GetDefinition(name)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if ac != nil && !def.VisibleTo(ac.Subject, ac.Teams, ac.IsGlobalAdmin) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, def)
}

func (h *Handler) getAgentByID(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	agentID := r.PathValue("agentID")
	def := h.store.GetDefinitionByID(agentID)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if ac != nil && !def.VisibleTo(ac.Subject, ac.Teams, ac.IsGlobalAdmin) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, def)
}

func (h *Handler) validateHandoffTargets(def *config.Definition) error {
	if def.HandoffTo != "" {
		if h.store.GetDefinition(def.HandoffTo) == nil {
			return fmt.Errorf("handoff target %q not found", def.HandoffTo)
		}
	}
	for _, tname := range def.Handoffs {
		if h.store.GetDefinition(tname) == nil {
			return fmt.Errorf("handoff target %q not found", tname)
		}
	}
	return nil
}
func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var def config.Definition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	def.Kind = config.KindAgent
	def.CreatedBy = ac.Subject

	if def.Scope == "" {
		def.Scope = string(config.ScopeUser)
	}

	if config.Scope(def.Scope) != config.ScopeTeam {
		def.Team = ""
	}

	if err := def.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := h.validateHandoffTargets(&def); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	switch config.Scope(def.Scope) {
	case config.ScopeGlobal:
		if !ac.IsGlobalAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "global admin required"})
			return
		}
	case config.ScopeTeam:
		if !ac.IsMemberOfTeam(def.Team) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a member of team " + def.Team})
			return
		}
	case config.ScopeUser:
		// any authenticated user
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid scope"})
		return
	}

	if err := h.store.SaveDefinition(&def); err != nil {
		slog.Error("failed to save agent", "name", def.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save"})
		return
	}

	writeJSON(w, http.StatusCreated, def)
}

func (h *Handler) updateAgent(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	name := r.PathValue("name")
	existing := h.store.GetDefinition(name)
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	if !existing.CanEdit(ac.Subject, ac.Teams, ac.IsGlobalAdmin, ac.IsTeamAdmin) {
		slog.Warn("agent edit permission denied",
			"agent", name,
			"scope", existing.Scope,
			"team", existing.Team,
			"created_by", existing.CreatedBy,
			"subject", ac.Subject,
			"user_teams", ac.Teams,
			"is_global_admin", ac.IsGlobalAdmin,
			"is_team_admin", ac.IsTeamAdmin,
		)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access denied"})
		return
	}

	var def config.Definition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	def.Kind = config.KindAgent
	def.CreatedBy = existing.CreatedBy
	def.AgentID = existing.AgentID

	switch config.Scope(def.Scope) {
	case config.ScopeGlobal:
		if !ac.IsGlobalAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "global admin required"})
			return
		}
	case config.ScopeTeam:
		if !ac.IsMemberOfTeam(def.Team) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a member of team " + def.Team})
			return
		}
	case config.ScopeUser, "":
	}

	if def.Scope == "" {
		def.Scope = string(config.ScopeUser)
	}

	if existing.Scope == string(config.ScopeTeam) && def.Scope != string(config.ScopeTeam) {
		if existing.CreatedBy != ac.Subject && !ac.IsGlobalAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the creator can change a team agent to personal"})
			return
		}
	}

	if err := def.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := h.validateHandoffTargets(&def); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if config.Scope(def.Scope) != config.ScopeTeam {
		def.Team = ""
	}

	if def.Name != name {
		if err := h.store.DeleteDefinition(name); err != nil {
			slog.Error("failed to delete old agent on rename", "old_name", name, "error", err)
		}
	}

	if err := h.store.SaveDefinition(&def); err != nil {
		slog.Error("failed to update agent", "name", def.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save"})
		return
	}

	saved := h.store.GetDefinition(def.Name)
	if saved == nil {
		saved = &def
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *Handler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	name := r.PathValue("name")
	existing := h.store.GetDefinition(name)
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	if !existing.CanDelete(ac.Subject, ac.Teams, ac.IsGlobalAdmin, ac.IsTeamAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access denied"})
		return
	}

	h.reg.Remove(name)
	if err := h.store.DeleteDefinition(name); err != nil {
		slog.Error("failed to delete agent", "name", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) listVersions(w http.ResponseWriter, r *http.Request) {
	vs, ok := h.store.(VersionedStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "versioning not available"})
		return
	}

	name := r.PathValue("name")
	def := h.store.GetDefinition(name)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	versions, err := vs.ListVersions(r.Context(), name)
	if err != nil {
		slog.Error("failed to list versions", "agent", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list versions"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (h *Handler) getVersion(w http.ResponseWriter, r *http.Request) {
	vs, ok := h.store.(VersionedStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "versioning not available"})
		return
	}

	name := r.PathValue("name")
	versionID := r.URL.Query().Get("version_id")
	if versionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version_id query parameter required"})
		return
	}

	def := h.store.GetDefinition(name)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	raw, parsed, err := vs.GetVersion(r.Context(), name, versionID)
	if err != nil {
		slog.Error("failed to get version", "agent", name, "version", versionID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get version"})
		return
	}

	type versionResponse struct {
		YAML       string             `json:"yaml"`
		Definition *config.Definition `json:"definition"`
	}
	writeJSON(w, http.StatusOK, versionResponse{YAML: string(raw), Definition: parsed})
}

func (h *Handler) rollbackVersion(w http.ResponseWriter, r *http.Request) {
	vs, ok := h.store.(VersionedStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "versioning not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	name := r.PathValue("name")
	versionID := r.URL.Query().Get("version_id")
	if versionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "version_id query parameter required"})
		return
	}

	existing := h.store.GetDefinition(name)
	if existing == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	if !existing.CanEdit(ac.Subject, ac.Teams, ac.IsGlobalAdmin, ac.IsTeamAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access denied"})
		return
	}

	if err := vs.Rollback(r.Context(), name, versionID); err != nil {
		slog.Error("failed to rollback", "agent", name, "version", versionID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to rollback"})
		return
	}

	slog.Info("rolled back agent", "agent", name, "version", versionID, "user", ac.Subject)
	restored := h.store.GetDefinition(name)
	writeJSON(w, http.StatusOK, restored)
}

func (h *Handler) listTools(w http.ResponseWriter, r *http.Request) {
	allTools := h.pool.ListAllTools()

	serverOverrides := make(map[string]json.RawMessage)
	if h.mcpStore != nil {
		records, err := h.mcpStore.ListAll(r.Context())
		if err == nil {
			for _, rec := range records {
				if len(rec.ToolOverrides) > 0 {
					serverOverrides[rec.Name] = rec.ToolOverrides
				}
			}
		}
	}

	type toolInfo struct {
		QualifiedName string          `json:"qualified_name"`
		Server        string          `json:"server"`
		Name          string          `json:"name"`
		Description   string          `json:"description"`
		InputSchema   json.RawMessage `json:"input_schema"`
		ToolOverrides json.RawMessage `json:"tool_overrides,omitempty"`
	}

	tools := make([]toolInfo, len(allTools))
	for i, dt := range allTools {
		tools[i] = toolInfo{
			QualifiedName: dt.QualifiedName(),
			Server:        dt.ServerName,
			Name:          dt.Tool.Name,
			Description:   dt.Tool.Description,
			InputSchema:   dt.InputSchemaJSON(),
			ToolOverrides: serverOverrides[dt.ServerName],
		}
	}

	writeJSON(w, http.StatusOK, tools)
}

func (h *Handler) getStatus(w http.ResponseWriter, r *http.Request) {
	allTools := h.pool.ListAllTools()
	toolNames := make([]string, len(allTools))
	for i, dt := range allTools {
		toolNames[i] = dt.QualifiedName()
	}

	status := map[string]any{
		"agents":      h.reg.ListAgentNames(),
		"tools":       toolNames,
		"mcp_servers": h.pool.ListServerNames(),
	}
	writeJSON(w, http.StatusOK, status)
}

type runAgentRequest struct {
	Message        string                   `json:"message"`
	History        []llm.Message            `json:"history,omitempty"`
	SessionID      string                   `json:"session_id,omitempty"`
	TaskID         string                   `json:"task_id,omitempty"`
	MCPServers     []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
	ResponseSchema *config.StructuredOutput `json:"response_schema,omitempty"`
}

type runAgentResponse struct {
	RunID      string `json:"run_id"`
	WorkflowID string `json:"workflow_id"`
}

// startPersistentRun starts an opt-in signal-driven run. Unlike /run, it has
// no initial message; callers submit ordered messages to its inputs endpoint.
func (h *Handler) startPersistentRun(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var req struct {
		MCPServers     []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
		ResponseSchema *config.StructuredOutput `json:"response_schema,omitempty"`
		ClientKey      string                   `json:"client_key,omitempty"`
		History        []llm.Message            `json:"history,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	def := h.store.GetDefinitionByID(r.PathValue("agentID"))
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	if len(req.ClientKey) > 256 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client_key is too long"})
		return
	}
	if existing, ok := h.runs.FindActiveClientKey(ac.Subject, req.ClientKey); ok {
		if existing.AgentName != def.Name || !sameRunMCPServers(existing.MCPServers, req.MCPServers) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "client_key already belongs to a run with different configuration"})
			return
		}
		writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: existing.ID, WorkflowID: existing.WorkflowID})
		return
	}

	var ephemeralNames []string
	for _, srv := range req.MCPServers {
		econn, err := mcpclient.ConnectEphemeral(r.Context(), srv)
		if err != nil {
			for _, name := range ephemeralNames {
				h.pool.UnregisterEphemeral(name)
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "connect MCP server: " + err.Error()})
			return
		}
		if err := h.pool.RegisterEphemeral(econn); err != nil {
			econn.Close()
			for _, name := range ephemeralNames {
				h.pool.UnregisterEphemeral(name)
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		ephemeralNames = append(ephemeralNames, srv.Name)
	}

	attachedJSON, err := json.Marshal(req.MCPServers)
	if err != nil {
		for _, name := range ephemeralNames {
			h.pool.UnregisterEphemeral(name)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid MCP configuration"})
		return
	}
	newRun, created, err := h.runs.CreatePersistentWithKeyChecked(def.Name, ac.Subject, req.ClientKey, attachedJSON)
	if err != nil {
		for _, name := range ephemeralNames {
			h.pool.UnregisterEphemeral(name)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to record run"})
		return
	}
	if !created {
		for _, name := range ephemeralNames {
			h.pool.UnregisterEphemeral(name)
		}
		if newRun.AgentName != def.Name || !sameRunMCPServers(newRun.MCPServers, req.MCPServers) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "client_key already belongs to a run with different configuration"})
			return
		}
		writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: newRun.ID, WorkflowID: newRun.WorkflowID})
		return
	}
	h.runs.SetEphemeralNames(newRun.ID, ephemeralNames)
	h.streams.Create(newRun.ID)
	workflowID := temporal.WorkflowIDForRun(newRun.ID, true)
	if err := h.runs.SetWorkflowID(newRun.ID, workflowID); err != nil {
		for _, name := range ephemeralNames {
			h.pool.UnregisterEphemeral(name)
		}
		h.streams.Delete(newRun.ID)
		h.runs.Delete(newRun.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to record workflow identity"})
		return
	}
	_, _, err = h.temporal.StartPersistentWorkflow(r.Context(), newRun.ID, temporal.RunAgentParams{
		AgentID: def.AgentID, AgentName: def.Name, MCPServers: req.MCPServers,
		ResponseSchema: req.ResponseSchema, StreamID: newRun.ID, History: req.History,
		LLMConfig: h.buildLLMConfig(r.Context(), def), MemoryEnabled: def.MemoryEnabled,
		MemorySearchAgentID: def.MemorySearchAgentID, MemoryIngestAgentID: def.MemoryIngestAgentID,
		UserSubject: ac.Subject,
	})
	if err != nil {
		slog.Warn("persistent workflow start response uncertain; reconciling by workflow ID", "workflow", workflowID, "error", err)
	}
	go h.awaitPersistentRun(newRun.ID, workflowID)
	writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: newRun.ID, WorkflowID: workflowID})
}

func (h *Handler) awaitPersistentRun(runID, workflowID string) {
	defer func() {
		if ru, ok := h.runs.Get(runID); ok {
			for _, name := range ru.EphemeralNames {
				h.pool.UnregisterEphemeral(name)
			}
		}
	}()
	_, err := h.temporal.AwaitWorkflow(context.Background(), workflowID, true)
	ru, ok := h.runs.Get(runID)
	if !ok {
		return
	}
	if err != nil && ru.Status != run.StatusCanceled {
		if saveErr := h.runs.WaitForStatus(context.Background(), runID, run.StatusFailed, "", err.Error()); saveErr != nil {
			return
		}
		h.streams.PublishError(runID, "Error: "+err.Error())
		return
	}
	if ru.Status == run.StatusCanceled {
		return
	}
	if saveErr := h.runs.WaitForStatus(context.Background(), runID, run.StatusCompleted, ru.Response, ""); saveErr != nil {
		return
	}
}

func (h *Handler) submitPersistentInput(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ru, ok := h.runs.Get(r.PathValue("id"))
	if !ok || ru.Owner != ac.Subject {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	if !ru.Persistent {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run does not accept inputs"})
		return
	}
	var req temporal.PersistentInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Message == "" || req.InputID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message and input_id are required"})
		return
	}
	inputStatus, err := h.runs.InputStatus(ru.ID, req.InputID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to look up input"})
		return
	}
	if req.Metadata["source"] == "web" {
		if inputStatus == "processed" {
			writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: ru.ID, WorkflowID: ru.WorkflowID})
			return
		}
		if inputStatus == "failed" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "steering input failed", "code": "input_failed"})
			return
		}
		if ru.Status == run.StatusCanceled || ru.Status == run.StatusCompleted || ru.Status == run.StatusFailed {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "run is not active", "status": string(ru.Status)})
			return
		}
		accepted, err := h.temporal.SteerPersistentInput(r.Context(), ru.WorkflowID, req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if !accepted {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "active turn no longer accepts steering"})
			return
		}
		if err := h.runs.RecordInput(ru.ID, req.InputID); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to record steering input"})
			return
		}
		writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: ru.ID, WorkflowID: ru.WorkflowID})
		return
	}
	if ru.Status == run.StatusCanceled || ru.Status == run.StatusCompleted || ru.Status == run.StatusFailed {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run is not active", "status": string(ru.Status)})
		return
	}
	if inputStatus == "processed" || inputStatus == "failed" || req.InputID == ru.LastInputID {
		writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: ru.ID, WorkflowID: ru.WorkflowID})
		return
	}
	if ru.Status == run.StatusRunning {
		if req.Metadata["source"] == "" && req.InputID != ru.CurrentInputID {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "run is processing an input", "status": string(ru.Status)})
			return
		}
		if err := h.runs.RecordInput(ru.ID, req.InputID); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to record input"})
			return
		}
		if err := h.temporal.SignalPersistentInput(r.Context(), ru.WorkflowID, req); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: ru.ID, WorkflowID: ru.WorkflowID})
		return
	}
	if ru.Status == run.StatusWaiting || h.streams.Get(ru.ID) == nil {
		// A prior persistent turn closes its SSE stream with `done`; each new
		// input gets a fresh stream while retaining the same logical run ID.
		h.streams.Create(ru.ID)
	}
	if err := h.runs.BeginInput(ru.ID, req.InputID); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to record pending input"})
		return
	}
	if err := h.runs.RecordInput(ru.ID, req.InputID); err != nil {
		_ = h.runs.ResetInput(ru.ID, req.InputID, ru.Response, ru.Error)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to record input"})
		return
	}
	if err := h.temporal.SignalPersistentInput(r.Context(), ru.WorkflowID, req); err != nil {
		// Temporal may have accepted the signal before the response was lost.
		// Keep the current input identity so retries cannot start a second turn.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: ru.ID, WorkflowID: ru.WorkflowID})
}

func (h *Handler) runAgent(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("name")

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req runAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	var def *config.Definition
	var history []llm.Message
	var sessionID string

	if req.SessionID != "" {
		sess := h.sessions.Get(req.SessionID)
		if sess == nil || sess.Owner != ac.Subject {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		def = h.store.GetDefinitionByID(sess.AgentID)
		sessionID = req.SessionID
	} else {
		def = h.store.GetDefinitionByID(agentID)
		history = req.History
	}

	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found: " + agentID})
		return
	}
	if existing, ok := h.runs.FindDispatchKey(ac.Subject, req.TaskID); ok {
		if existing.AgentName != def.Name || existing.SessionID != sessionID {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "task_id already belongs to a different run"})
			return
		}
		writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: existing.ID, WorkflowID: existing.WorkflowID})
		return
	}
	if sessionID != "" {
		if sess := h.sessions.Get(sessionID); sess != nil && sess.ActiveRunID != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "session already has an active run"})
			return
		}
	}

	var ephemeralNames []string
	for _, srv := range req.MCPServers {
		econn, err := mcpclient.ConnectEphemeral(r.Context(), srv)
		if err != nil {
			for _, name := range ephemeralNames {
				h.pool.UnregisterEphemeral(name)
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "connect MCP server: " + err.Error()})
			return
		}
		if err := h.pool.RegisterEphemeral(econn); err != nil {
			econn.Close()
			for _, name := range ephemeralNames {
				h.pool.UnregisterEphemeral(name)
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		ephemeralNames = append(ephemeralNames, srv.Name)
	}

	newRun, created, err := h.runs.CreateByTaskChecked(def.Name, ac.Subject, sessionID, req.TaskID)
	if err != nil {
		for _, n := range ephemeralNames {
			h.pool.UnregisterEphemeral(n)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to record run"})
		return
	}
	if !created {
		for _, n := range ephemeralNames {
			h.pool.UnregisterEphemeral(n)
		}
		if newRun.AgentName != def.Name || newRun.SessionID != sessionID {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "task_id already belongs to a different run"})
			return
		}
		writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: newRun.ID, WorkflowID: newRun.WorkflowID})
		return
	}
	if err := h.recordRunMCPServers(newRun.ID, req.MCPServers); err != nil {
		for _, name := range ephemeralNames {
			h.pool.UnregisterEphemeral(name)
		}
		h.runs.Delete(newRun.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to record run tools"})
		return
	}
	h.streams.Create(newRun.ID)
	if sessionID != "" {
		previous, err := h.sessions.StartRun(sessionID, newRun.ID, session.Message{Role: "user", Content: req.Message, Time: time.Now()})
		if err != nil {
			h.streams.Delete(newRun.ID)
			h.runs.Delete(newRun.ID)
			for _, n := range ephemeralNames {
				h.pool.UnregisterEphemeral(n)
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		for _, m := range previous {
			history = append(history, llm.Message{Role: m.Role, Content: m.Content})
		}
	}

	workflowID := temporal.WorkflowIDForRun(newRun.ID, false)
	if err := h.runs.SetWorkflowID(newRun.ID, workflowID); err != nil {
		for _, n := range ephemeralNames {
			h.pool.UnregisterEphemeral(n)
		}
		h.streams.Delete(newRun.ID)
		h.runs.Delete(newRun.ID)
		if sessionID != "" {
			_ = h.sessions.CompleteRun(sessionID, newRun.ID, "Error: failed to record workflow identity")
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to record workflow identity"})
		return
	}
	_, _, err = h.temporal.StartWorkflow(r.Context(), temporal.RunAgentParams{
		AgentID:             def.AgentID,
		AgentName:           def.Name,
		Message:             req.Message,
		History:             history,
		MCPServers:          req.MCPServers,
		ResponseSchema:      req.ResponseSchema,
		StreamID:            newRun.ID,
		SessionID:           sessionID,
		LLMConfig:           h.buildLLMConfig(r.Context(), def),
		MemoryEnabled:       def.MemoryEnabled,
		MemorySearchAgentID: def.MemorySearchAgentID,
		MemoryIngestAgentID: def.MemoryIngestAgentID,
		UserSubject:         ac.Subject,
	})
	if err != nil {
		slog.Warn("workflow start response uncertain; reconciling by workflow ID", "workflow", workflowID, "error", err)
	}

	go func() {
		cleanup := func() {
			time.AfterFunc(30*time.Second, func() {
				h.streams.Delete(newRun.ID)
			})
		}

		agentResult, err := h.temporal.AwaitWorkflow(context.Background(), workflowID, false)
		for _, n := range ephemeralNames {
			h.pool.UnregisterEphemeral(n)
		}
		current, ok := h.runs.Get(newRun.ID)
		if !ok || current.Status == run.StatusCanceled {
			cleanup()
			return
		}
		if err != nil {
			slog.Error("agent run failed", "agent", def.Name, "run_id", newRun.ID, "error", err)
			if saveErr := h.runs.WaitForStatus(context.Background(), newRun.ID, run.StatusFailed, "", err.Error()); saveErr != nil {
				cleanup()
				return
			}
			if sessionID != "" {
				_ = h.sessions.CompleteRunUntil(context.Background(), sessionID, newRun.ID, "Error: "+err.Error())
			}
			h.streams.PublishError(newRun.ID, "Error: "+err.Error())
			cleanup()
			return
		}

		slog.Info("agent run completed", "agent", def.Name, "run_id", newRun.ID)
		if err := h.runs.WaitForStatus(context.Background(), newRun.ID, run.StatusCompleted, agentResult.Response, ""); err != nil {
			cleanup()
			return
		}
		if sessionID != "" {
			_ = h.sessions.CompleteRunUntil(context.Background(), sessionID, newRun.ID, agentResult.Response)
		}
		h.streams.PublishDone(newRun.ID, agentResult.Response)
		cleanup()
	}()

	writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: newRun.ID, WorkflowID: workflowID})
}

func (h *Handler) recordRunMCPServers(id string, servers []mcpclient.ServerConfig) error {
	data, err := json.Marshal(servers)
	if err != nil {
		return err
	}
	return h.runs.SetMCPServers(id, data)
}

func sameRunMCPServers(raw json.RawMessage, requested []mcpclient.ServerConfig) bool {
	var attached []mcpclient.ServerConfig
	if err := json.Unmarshal(raw, &attached); err != nil {
		return false
	}
	if len(attached) == 0 && len(requested) == 0 {
		return true
	}
	return reflect.DeepEqual(attached, requested)
}

func (h *Handler) getRunStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ru, ok := h.runs.Get(id)
	ac := auth.FromContext(r)
	if !ok || ac == nil || ru.Owner != ac.Subject {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	writeJSON(w, http.StatusOK, ru)
}

func (h *Handler) getRunInputStatus(w http.ResponseWriter, r *http.Request) {
	ru, ok := h.runs.Get(r.PathValue("id"))
	ac := auth.FromContext(r)
	if !ok || ac == nil || ru.Owner != ac.Subject {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	status, err := h.runs.InputStatus(ru.ID, r.PathValue("inputID"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "input status unavailable"})
		return
	}
	if status == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "input not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"input_id": r.PathValue("inputID"), "status": status})
}

// getRunTrace exposes only the Temporal execution associated with a run the
// caller owns; workflow IDs alone are not sufficient authorization.
func (h *Handler) getRunTrace(w http.ResponseWriter, r *http.Request) {
	ru, ok := h.runs.Get(r.PathValue("id"))
	ac := auth.FromContext(r)
	if !ok || ac == nil || ru.Owner != ac.Subject {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}
	if ru.WorkflowID == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run has no workflow"})
		return
	}
	trace, err := h.temporal.GetExecutionTrace(r.Context(), ru.WorkflowID, "")
	if err != nil {
		slog.Error("failed to get run trace", "run_id", ru.ID, "workflow_id", ru.WorkflowID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to get run trace: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

// listRunsByTaskID returns the run associated with a background task id, so
// eve can rediscover in-flight task runs after its own restart.
func (h *Handler) listRunsByTaskID(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id query parameter is required"})
		return
	}
	runs := h.runs.ListByTaskID(taskID)
	for _, ru := range runs {
		if ru.Owner == ac.Subject {
			writeJSON(w, http.StatusOK, ru)
			return
		}
	}
	if len(runs) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no run found for task"})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no run found for task"})
}

func (h *Handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ru, ok := h.runs.Get(id)
	ac := auth.FromContext(r)
	if !ok || ac == nil || ru.Owner != ac.Subject {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
		return
	}

	if ru.Status != run.StatusRunning && ru.Status != run.StatusWaiting {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run is not running", "status": string(ru.Status)})
		return
	}

	if ru.WorkflowID != "" {
		if err := h.temporal.CancelWorkflow(r.Context(), ru.WorkflowID); err != nil {
			slog.Error("failed to cancel temporal workflow", "run_id", id, "workflow_id", ru.WorkflowID, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to cancel workflow"})
			return
		}
	}

	if err := h.runs.UpdateStatus(id, run.StatusCanceled, "", "canceled by user"); err != nil {
		if errors.Is(err, run.ErrTerminal) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "run is already terminal"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "failed to record cancellation"})
		return
	}
	h.streams.PublishError(id, "canceled by user")
	slog.Info("run canceled", "run_id", id)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) getRawAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := h.store.GetRawDefinition(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (h *Handler) buildLLMConfig(ctx context.Context, def *config.Definition) *temporal.LLMConfigInput {
	if def == nil || def.ProviderID == "" {
		return nil
	}
	prov, err := h.providerStore.GetByID(ctx, def.ProviderID)
	if err != nil {
		slog.Warn("failed to resolve provider for agent", "agent_id", def.AgentID, "provider_id", def.ProviderID, "error", err)
		return nil
	}
	return &temporal.LLMConfigInput{
		SchemaValidation: prov.SchemaValidation,
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

type mcpCallRequest struct {
	Server    string         `json:"server"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

type mcpCallResponse struct {
	Content       string         `json:"content"`
	ContentBlocks []ContentBlock `json:"content_blocks,omitempty"`
	IsError       bool           `json:"is_error"`
}

func (h *Handler) mcpProxyCall(w http.ResponseWriter, r *http.Request) {
	var req mcpCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	result, err := h.pool.CallTool(r.Context(), req.Server, req.Tool, req.Arguments)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	content, blocks := extractMCPContent(result)
	writeJSON(w, http.StatusOK, mcpCallResponse{Content: content, ContentBlocks: blocks, IsError: result.IsError})
}

func extractMCPContent(result *mcp.CallToolResult) (string, []ContentBlock) {
	var parts []string
	var blocks []ContentBlock
	for _, c := range result.Content {
		switch b := c.(type) {
		case mcp.TextContent:
			parts = append(parts, b.Text)
			blocks = append(blocks, ContentBlock{Type: "text", Text: b.Text})
		case mcp.ImageContent:
			blocks = append(blocks, ContentBlock{Type: "image", Data: b.Data, MIMEType: b.MIMEType})
		}
	}
	return strings.Join(parts, "\n"), blocks
}

// --- Chat session endpoints ---

type createSessionRequest struct {
	AgentID string `json:"agent_id"`
}

func (h *Handler) createChatSession(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}

	def := h.store.GetDefinitionByID(req.AgentID)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found: " + req.AgentID})
		return
	}

	sess, err := h.sessions.CreateChecked(def.AgentID, def.Name, ac.Subject)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create session"})
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (h *Handler) listChatSessions(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, h.sessions.ListByOwner(ac.Subject))
}

func (h *Handler) getChatSession(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	id := r.PathValue("id")
	sess := h.sessions.Get(id)
	if sess == nil || sess.Owner != ac.Subject {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (h *Handler) runEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")

	ru, ok := h.runs.Get(runID)
	if !ok {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	ac := auth.FromContext(r)
	if ac == nil || ru.Owner != ac.Subject {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	runStream := h.streams.Get(runID)
	if runStream == nil {
		runStream = h.streams.Create(runID)
		switch ru.Status {
		case run.StatusCompleted, run.StatusWaiting:
			h.streams.PublishDone(runID, ru.Response)
		case run.StatusFailed, run.StatusCanceled:
			h.streams.PublishError(runID, ru.Error)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	var ch <-chan stream.Event
	var unsubscribe func()
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		if id, err := strconv.ParseInt(lastID, 10, 64); err == nil {
			ch, unsubscribe = runStream.SubscribeFrom(id)
		} else {
			ch, unsubscribe = runStream.Subscribe()
		}
	} else {
		ch, unsubscribe = runStream.Subscribe()
	}
	defer unsubscribe()

	buf := make([]byte, 0, 256)
	for {
		select {
		case evt, open := <-ch:
			if !open {
				return
			}
			buf = buf[:0]
			buf = append(buf, "id:"...)
			buf = strconv.AppendInt(buf, evt.ID, 10)
			buf = append(buf, "\nevent: "...)
			buf = append(buf, evt.Type...)
			buf = append(buf, "\ndata: "...)
			if strings.Contains(evt.Data, "\n") {
				buf = append(buf, strings.ReplaceAll(evt.Data, "\n", "\ndata: ")...)
			} else {
				buf = append(buf, evt.Data...)
			}
			buf = append(buf, "\n\n"...)
			w.Write(buf)
			flusher.Flush()
			if evt.Type == "done" || evt.Type == "error" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

type createAPIKeyRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type createAPIKeyResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	FullKey   string `json:"full_key"`
}

func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.keyStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "API key management not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req createAPIKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	keyID, prefix, fullKey, err := h.keyStore.Create(r.Context(), req.Name, ac.Subject, req.ExpiresAt)
	if err != nil {
		slog.Error("failed to create api key", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create key"})
		return
	}

	writeJSON(w, http.StatusCreated, createAPIKeyResponse{
		ID:        keyID,
		Name:      req.Name,
		KeyPrefix: prefix,
		FullKey:   fullKey,
	})
}

type apiKeyInfo struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	if h.keyStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "API key management not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	keys, err := h.keyStore.List(r.Context(), ac.Subject)
	if err != nil {
		slog.Error("failed to list api keys", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list keys"})
		return
	}

	result := make([]apiKeyInfo, len(keys))
	for i, k := range keys {
		result[i] = apiKeyInfo{
			ID:         k.ID,
			Name:       k.Name,
			KeyPrefix:  k.KeyPrefix,
			CreatedAt:  k.CreatedAt,
			LastUsedAt: k.LastUsedAt,
			ExpiresAt:  k.ExpiresAt,
		}
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.keyStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "API key management not available"})
		return
	}

	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	id := r.PathValue("id")
	if err := h.keyStore.Revoke(r.Context(), id); err != nil {
		if err == auth.ErrKeyNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "api key not found"})
			return
		}
		slog.Error("failed to revoke api key", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to revoke"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (h *Handler) listTeams(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	if ac == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": ac.Teams})
}

func (h *Handler) listExecutions(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)

	agent := r.URL.Query().Get("agent_name")
	query := fmt.Sprintf("WorkflowType = '%s'", temporal.WorkflowType)
	if status := r.URL.Query().Get("status"); status != "" {
		query += fmt.Sprintf(" AND ExecutionStatus = '%s'", status)
	}
	if agent != "" {
		if !h.agentVisible(ac, agent) {
			writeJSON(w, http.StatusOK, []temporal.ExecutionInfo{})
			return
		}
		query += fmt.Sprintf(" AND AgentName = '%s'", agent)
	}

	isGlobalAdmin := ac != nil && ac.IsGlobalAdmin
	if !isGlobalAdmin && agent == "" {
		visibleAgents := h.visibleAgentNames(ac)
		if len(visibleAgents) == 0 {
			writeJSON(w, http.StatusOK, []temporal.ExecutionInfo{})
			return
		}
		var parts []string
		for name := range visibleAgents {
			parts = append(parts, "AgentName = '"+name+"'")
		}
		query += " AND (" + strings.Join(parts, " OR ") + ")"
	}

	execs, err := h.temporal.ListWorkflows(r.Context(), query, 100)
	if err != nil {
		slog.Error("failed to list executions", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list executions: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, execs)
}

func (h *Handler) getExecution(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	workflowID := r.PathValue("workflowId")

	detail, err := h.temporal.GetWorkflowHistory(r.Context(), workflowID, "")
	if err != nil {
		slog.Error("failed to get execution history", "workflow_id", workflowID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get execution: " + err.Error()})
		return
	}

	if ac != nil && ac.IsGlobalAdmin {
		writeJSON(w, http.StatusOK, detail)
		return
	}

	def := h.store.GetDefinition(detail.AgentName)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	if ac == nil {
		if !def.VisibleTo("", nil, false) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
	} else {
		if !def.VisibleTo(ac.Subject, ac.Teams, ac.IsGlobalAdmin) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
	}

	writeJSON(w, http.StatusOK, detail)
}

// getExecutionTrace exposes a normalized, durable execution trace. It uses the
// same visibility rules as raw history but remains available after ephemeral
// run status records have been cleaned up.
func (h *Handler) getExecutionTrace(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r)
	workflowID := r.PathValue("workflowId")
	trace, err := h.temporal.GetExecutionTrace(r.Context(), workflowID, "")
	if err != nil {
		slog.Error("failed to get execution trace", "workflow_id", workflowID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get execution trace: " + err.Error()})
		return
	}
	if ac != nil && ac.IsGlobalAdmin {
		writeJSON(w, http.StatusOK, trace)
		return
	}
	def := h.store.GetDefinition(trace.AgentName)
	if def == nil || !def.VisibleTo(authSubject(ac), authTeams(ac), ac != nil && ac.IsGlobalAdmin) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

func authSubject(ac *auth.AuthContext) string {
	if ac == nil {
		return ""
	}
	return ac.Subject
}

func authTeams(ac *auth.AuthContext) []string {
	if ac == nil {
		return nil
	}
	return ac.Teams
}

func (h *Handler) agentVisible(ac *auth.AuthContext, agentName string) bool {
	def := h.store.GetDefinition(agentName)
	if def == nil {
		return false
	}
	if ac == nil {
		return def.VisibleTo("", nil, false)
	}
	return def.VisibleTo(ac.Subject, ac.Teams, ac.IsGlobalAdmin)
}

func (h *Handler) visibleAgentNames(ac *auth.AuthContext) map[string]bool {
	names := make(map[string]bool)
	for _, d := range h.store.ListDefinitions() {
		if ac == nil {
			if d.VisibleTo("", nil, false) {
				names[d.Name] = true
			}
		} else {
			if d.VisibleTo(ac.Subject, ac.Teams, ac.IsGlobalAdmin) {
				names[d.Name] = true
			}
		}
	}
	return names
}
