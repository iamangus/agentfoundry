package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/mcpclient"
	"github.com/angoo/agentfoundry/internal/registry"
	"github.com/angoo/agentfoundry/internal/temporal"
)

type Manager struct {
	reg      *registry.Registry
	pool     *mcpclient.Pool
	temporal *temporal.Client
	mu       sync.RWMutex
	servers  map[string]*server.StreamableHTTPServer
}

func NewManager(reg *registry.Registry, pool *mcpclient.Pool, temporalClient *temporal.Client) *Manager {
	return &Manager{
		reg:      reg,
		pool:     pool,
		temporal: temporalClient,
		servers:  make(map[string]*server.StreamableHTTPServer),
	}
}

func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/servers/{name}", m.handle)
	slog.Info("MCP routes registered", "pattern", "/servers/{name}")
}

func (m *Manager) handle(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	srv := m.getOrCreateServer(name)
	if srv == nil {
		http.Error(w, fmt.Sprintf("unknown server: %s", name), http.StatusNotFound)
		return
	}
	srv.ServeHTTP(w, r)
}

func (m *Manager) getOrCreateServer(name string) *server.StreamableHTTPServer {
	m.mu.RLock()
	if srv, ok := m.servers[name]; ok {
		m.mu.RUnlock()
		return srv
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if srv, ok := m.servers[name]; ok {
		return srv
	}

	var srv *server.StreamableHTTPServer
	if name == "default" {
		srv = m.createDefaultServer()
	} else {
		srv = m.createAgentServer(name)
	}

	if srv != nil {
		m.servers[name] = srv
	}
	return srv
}

func (m *Manager) createDefaultServer() *server.StreamableHTTPServer {
	mcpServer := server.NewMCPServer("agentfoundry-default", "1.0.0",
		server.WithToolCapabilities(true),
	)

	allTools := m.pool.ListAllTools()
	for _, dt := range allTools {
		m.addDiscoveredTool(mcpServer, &dt)
	}

	srv := server.NewStreamableHTTPServer(mcpServer)

	slog.Info("created default MCP server", "tools", len(allTools))
	return srv
}

func (m *Manager) createAgentServer(name string) *server.StreamableHTTPServer {
	agentDef, ok := m.reg.GetAgentDef(name)
	if !ok {
		slog.Warn("cannot create MCP server for unknown agent", "name", name)
		return nil
	}

	mcpServer := server.NewMCPServer("agentfoundry-"+name, "1.0.0",
		server.WithToolCapabilities(true),
	)

	toolCount := 0
	for _, ref := range agentDef.Tools {
		if serverName, toolName, ok := parseRef(ref); ok {
			dt, found := m.pool.GetTool(serverName, toolName)
			if found {
				m.addDiscoveredTool(mcpServer, dt)
				toolCount++
				continue
			}
			slog.Warn("agent MCP server: skipping unknown MCP tool",
				"agent", name, "ref", ref)
			continue
		}

		if refAgentDef, ok := m.reg.GetAgentDef(ref); ok {
			m.addAgentAsTool(mcpServer, refAgentDef)
			toolCount++
			continue
		}

		slog.Warn("agent MCP server: skipping unresolvable ref", "agent", name, "ref", ref)
	}

	srv := server.NewStreamableHTTPServer(mcpServer)

	slog.Info("created agent MCP server", "agent", name, "tools", toolCount)
	return srv
}

func (m *Manager) addDiscoveredTool(mcpServer *server.MCPServer, dt *mcpclient.DiscoveredTool) {
	qualifiedName := dt.QualifiedName()

	mcpTool := mcp.Tool{
		Name:        qualifiedName,
		Description: dt.Tool.Description,
		InputSchema: dt.Tool.InputSchema,
	}

	pool := m.pool
	serverName := dt.ServerName
	toolName := dt.Tool.Name

	mcpServer.AddTool(mcpTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result, err := pool.CallTool(ctx, serverName, toolName, req.GetArguments())
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return result, nil
	})
}

func (m *Manager) addAgentAsTool(mcpServer *server.MCPServer, def *config.Definition) {
	mcpTool := mcp.NewTool(def.Name,
		mcp.WithDescription(def.Description),
		mcp.WithString("message", mcp.Description("The message/request to send to this agent"), mcp.Required()),
	)

	tc := m.temporal
	agentDef := def

	mcpServer.AddTool(mcpTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		message, ok := req.GetArguments()["message"].(string)
		if !ok {
			return mcp.NewToolResultError("message argument is required and must be a string"), nil
		}

		result, err := tc.ExecuteWorkflowSync(ctx, temporal.RunAgentParams{
			AgentName: agentDef.Name,
			Message:   message,
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("agent error: %v", err)), nil
		}

		return mcp.NewToolResultText(result.Response), nil
	})
}

func parseRef(ref string) (serverName, toolName string, ok bool) {
	idx := strings.Index(ref, ".")
	if idx < 0 {
		return "", "", false
	}
	return ref[:idx], ref[idx+1:], true
}

func (m *Manager) RefreshAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servers = make(map[string]*server.StreamableHTTPServer)
	slog.Info("all MCP servers invalidated")
}

func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, srv := range m.servers {
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("error shutting down MCP server", "name", name, "error", err)
		}
	}
}
