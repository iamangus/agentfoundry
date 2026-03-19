package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/angoo/agentfile/internal/agent"
	"github.com/angoo/agentfile/internal/config"
	"github.com/angoo/agentfile/internal/llm"
	"github.com/angoo/agentfile/internal/mcpclient"
	"github.com/angoo/agentfile/internal/registry"
)

// DefinitionStore persists and retrieves agent definitions.
type DefinitionStore interface {
	SaveDefinition(def *config.Definition) error
	DeleteDefinition(name string) error
	GetDefinition(name string) *config.Definition
	ListDefinitions() []*config.Definition
	GetRawDefinition(name string) ([]byte, error)
	SaveRawDefinition(name string, data []byte) error
}

// Handler serves the REST API.
type Handler struct {
	store        DefinitionStore
	reg          *registry.Registry
	pool         *mcpclient.Pool
	agentRuntime *agent.Runtime
	runs         *RunManager
}

// NewHandler creates a new API handler.
func NewHandler(reg *registry.Registry, pool *mcpclient.Pool, store DefinitionStore, agentRuntime *agent.Runtime) *Handler {
	return &Handler{
		store:        store,
		reg:          reg,
		pool:         pool,
		agentRuntime: agentRuntime,
		runs:         NewRunManager(),
	}
}

// RegisterRoutes registers the API routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents", h.listAgents)
	mux.HandleFunc("GET /api/v1/agents/{name}", h.getAgent)
	mux.HandleFunc("GET /api/v1/agents/{name}/raw", h.getRawAgent)
	mux.HandleFunc("POST /api/v1/agents", h.createAgent)
	mux.HandleFunc("PUT /api/v1/agents/{name}", h.updateAgentRaw)
	mux.HandleFunc("DELETE /api/v1/agents/{name}", h.deleteAgent)
	mux.HandleFunc("POST /api/v1/agents/{name}/run", h.runAgent)
	mux.HandleFunc("GET /api/v1/runs/{id}", h.getRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", h.cancelRun)
	mux.HandleFunc("GET /api/v1/tools", h.listTools)
	mux.HandleFunc("GET /api/v1/status", h.getStatus)

	slog.Info("API routes registered", "prefix", "/api/v1")
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.store.ListDefinitions())
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	def := h.store.GetDefinition(name)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, def)
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	var def config.Definition
	if err := json.NewDecoder(r.Body).Decode(&def); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	if err := def.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := h.store.SaveDefinition(&def); err != nil {
		slog.Error("failed to save agent", "name", def.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save"})
		return
	}

	writeJSON(w, http.StatusCreated, def)
}

func (h *Handler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	h.reg.Remove(name)
	if err := h.store.DeleteDefinition(name); err != nil {
		slog.Error("failed to delete agent", "name", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) listTools(w http.ResponseWriter, r *http.Request) {
	allTools := h.pool.ListAllTools()

	type toolInfo struct {
		QualifiedName string `json:"qualified_name"`
		Server        string `json:"server"`
		Name          string `json:"name"`
		Description   string `json:"description"`
	}

	tools := make([]toolInfo, len(allTools))
	for i, dt := range allTools {
		tools[i] = toolInfo{
			QualifiedName: dt.QualifiedName(),
			Server:        dt.ServerName,
			Name:          dt.Tool.Name,
			Description:   dt.Tool.Description,
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

// runAgentRequest is the JSON body for POST /api/v1/agents/{name}/run.
type runAgentRequest struct {
	Message    string                   `json:"message"`
	History    []llm.Message            `json:"history,omitempty"`
	MCPServers []mcpclient.ServerConfig `json:"mcp_servers,omitempty"`
}

// runAgentResponse is the JSON body returned for a successfully queued run.
type runAgentResponse struct {
	RunID string `json:"run_id"`
}

func (h *Handler) runAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	def, ok := h.reg.GetAgentDef(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found: " + name})
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

	// Generate a unique run ID and create a cancellable context for this run.
	runID := uuid.New().String()
	ctx, cancel := context.WithCancel(context.Background())

	// Register the run before starting the goroutine so it is immediately
	// visible to any polling callers.
	h.runs.Create(runID, name, cancel)

	// Connect any ephemeral MCP servers provided in the request.
	// They are closed once the run goroutine exits, regardless of outcome.
	ephemeral := make([]*mcpclient.EphemeralConn, 0, len(req.MCPServers))
	for _, srv := range req.MCPServers {
		conn, err := mcpclient.ConnectEphemeral(r.Context(), srv)
		if err != nil {
			slog.Error("failed to connect ephemeral MCP server", "name", srv.Name, "url", srv.URL, "error", err)
			cancel()
			for _, c := range ephemeral {
				c.Close()
			}
			// Remove the run we already registered since we're failing early.
			h.runs.SetFailed(runID, "failed to connect MCP server "+srv.Name+": "+err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error": "failed to connect MCP server " + srv.Name + ": " + err.Error(),
			})
			return
		}
		ephemeral = append(ephemeral, conn)
	}

	// Snapshot inputs before handing off to the goroutine.
	defSnap := def
	msgSnap := req.Message
	historySnap := req.History

	go func() {
		defer func() {
			for _, c := range ephemeral {
				c.Close()
			}
			cancel() // ensure context resources are always released
		}()

		h.runs.SetRunning(runID)

		result, _, err := h.agentRuntime.RunWithReporter(ctx, defSnap, msgSnap, nil, historySnap, ephemeral...)
		if err != nil {
			if ctx.Err() != nil {
				// Context was canceled externally (e.g. via POST /runs/{id}/cancel).
				// RunManager.Cancel has already set the status; nothing more to do.
				slog.Info("agent run canceled", "run_id", runID, "agent", name)
				return
			}
			slog.Error("agent run failed", "run_id", runID, "agent", name, "error", err)
			h.runs.SetFailed(runID, err.Error())
			return
		}

		slog.Info("agent run completed", "run_id", runID, "agent", name)
		h.runs.SetCompleted(runID, result)
	}()

	writeJSON(w, http.StatusAccepted, runAgentResponse{RunID: runID})
}

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info := h.runs.Get(id)
	if info == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found: " + id})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *Handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.runs.Cancel(id) {
		info := h.runs.Get(id)
		if info == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found: " + id})
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "run is already in terminal state: " + string(info.Status),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
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

func (h *Handler) updateAgentRaw(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
		return
	}
	if err := h.store.SaveRawDefinition(name, data); err != nil {
		slog.Error("failed to save raw agent", "name", name, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
