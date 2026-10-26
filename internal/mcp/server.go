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
	reg            *registry.Registry
	pool           *mcpclient.Pool
	temporal       *temporal.Client
	mu             sync.RWMutex
	servers        map[string]*server.StreamableHTTPServer
	cores          map[string]*server.MCPServer
	allowedOrigins []string
	retired        []*server.StreamableHTTPServer
	ctx            context.Context
	cancel         context.CancelFunc
}

type Option func(*Manager)

func WithAllowedOrigins(origins ...string) Option {
	return func(m *Manager) {
		for _, origin := range origins {
			if origin = strings.TrimSpace(origin); origin != "" {
				m.allowedOrigins = append(m.allowedOrigins, origin)
			}
		}
	}
}

func NewManager(reg *registry.Registry, pool *mcpclient.Pool, temporalClient *temporal.Client, options ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		reg:      reg,
		pool:     pool,
		temporal: temporalClient,
		servers:  make(map[string]*server.StreamableHTTPServer),
		cores:    make(map[string]*server.MCPServer),
		ctx:      ctx, cancel: cancel,
	}
	for _, option := range options {
		option(m)
	}
	return m
}

func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/servers/{name}", m.handle)
	slog.Info("MCP routes registered", "pattern", "/servers/{name}")
}

func (m *Manager) handle(w http.ResponseWriter, r *http.Request) {
	if m.ctx.Err() != nil {
		http.Error(w, "MCP server shutting down", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	defer cancel()
	r = r.WithContext(ctx)
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		allowed := false
		if len(origins) == 1 {
			for _, origin := range m.allowedOrigins {
				if origins[0] == origin {
					allowed = true
				}
			}
		}
		if !allowed {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	}
	if mcp.IsModernProtocol(r.Header.Get("MCP-Protocol-Version")) {
		r = r.Clone(r.Context())
		r.Header.Del("Last-Event-ID") // obsolete; ignore rather than reject
	}
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
		server.WithRecovery(),
	)

	allTools := m.pool.ListAllTools()
	for _, dt := range allTools {
		mcpServer.AddTools(m.discoveredTool(&dt))
	}
	m.cores["default"] = mcpServer

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
		server.WithRecovery(),
	)

	toolCount := 0
	for _, ref := range agentDef.Tools {
		if serverName, toolName, ok := parseRef(ref); ok {
			dt, found := m.pool.GetTool(serverName, toolName)
			if found {
				mcpServer.AddTools(m.discoveredTool(dt))
				toolCount++
				continue
			}
			slog.Warn("agent MCP server: skipping unknown MCP tool",
				"agent", name, "ref", ref)
			continue
		}

		if refAgentDef, ok := m.reg.GetAgentDef(ref); ok {
			mcpServer.AddTools(m.agentTool(refAgentDef))
			toolCount++
			continue
		}

		slog.Warn("agent MCP server: skipping unresolvable ref", "agent", name, "ref", ref)
	}

	m.cores[name] = mcpServer
	srv := server.NewStreamableHTTPServer(mcpServer)

	slog.Info("created agent MCP server", "agent", name, "tools", toolCount)
	return srv
}

func (m *Manager) discoveredTool(dt *mcpclient.DiscoveredTool) server.ServerTool {
	qualifiedName := dt.QualifiedName()

	// Preserve raw schemas, outputSchema, annotations, icons and x-mcp-header
	// declarations when exposing an upstream tool under a qualified name.
	mcpTool := dt.Tool
	mcpTool.Name = qualifiedName
	// Tasks are not proxied by this synchronous tool gateway.
	mcpTool.Execution = nil

	pool := m.pool
	serverName := dt.ServerName
	toolName := dt.Tool.Name

	return server.ServerTool{Tool: mcpTool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result, err := pool.CallTool(ctx, serverName, toolName, req.GetArguments())
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return result, nil
	}}
}

func (m *Manager) agentTool(def *config.Definition) server.ServerTool {
	mcpTool := mcp.NewTool(def.Name,
		mcp.WithDescription(def.Description),
		mcp.WithString("message", mcp.Description("The message/request to send to this agent"), mcp.Required()),
	)

	tc := m.temporal
	agentDef := def

	return server.ServerTool{Tool: mcpTool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		message, ok := req.GetArguments()["message"].(string)
		if !ok {
			return mcp.NewToolResultError("message argument is required and must be a string"), nil
		}

		result, err := tc.ExecuteWorkflowSync(ctx, temporal.RunAgentParams{
			AgentID:   agentDef.AgentID,
			AgentName: agentDef.Name,
			Message:   message,
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("agent error: %v", err)), nil
		}

		return mcp.NewToolResultText(result.Response), nil
	}}
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
	for name, core := range m.cores {
		var tools []server.ServerTool
		if name == "default" {
			for _, dt := range m.pool.ListAllTools() {
				tools = append(tools, m.discoveredTool(&dt))
			}
		} else if def, ok := m.reg.GetAgentDef(name); ok {
			for _, ref := range def.Tools {
				if serverName, toolName, ok := parseRef(ref); ok {
					if dt, found := m.pool.GetTool(serverName, toolName); found {
						tools = append(tools, m.discoveredTool(dt))
					}
				} else if child, found := m.reg.GetAgentDef(ref); found {
					tools = append(tools, m.agentTool(child))
				}
			}
		} else {
			// Existing streams see the removal; future requests get 404.
			core.SetTools()
			if srv := m.servers[name]; srv != nil {
				m.retired = append(m.retired, srv)
			}
			delete(m.servers, name)
			delete(m.cores, name)
			continue
		}
		// Preserve transport instances and active subscriptions. SetTools emits
		// list_changed for both legacy sessions and modern listen streams.
		core.SetTools(tools...)
	}
	slog.Info("MCP server tools refreshed")
}

func (m *Manager) Shutdown(ctx context.Context) {
	m.cancel() // Includes modern subscriptions, which carry no legacy session.
	m.mu.Lock()
	servers := m.servers
	retired := m.retired
	m.servers = make(map[string]*server.StreamableHTTPServer)
	m.cores = make(map[string]*server.MCPServer)
	m.retired = nil
	m.mu.Unlock()
	for name, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("error shutting down MCP server", "name", name, "error", err)
		}
	}
	for _, srv := range retired {
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("error shutting down retired MCP server", "error", err)
		}
	}
}
