package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// DiscoveredTool represents a tool discovered from an external MCP server.
type DiscoveredTool struct {
	// ServerName is the name of the MCP server this tool belongs to.
	ServerName string
	// Tool is the MCP tool metadata.
	Tool mcp.Tool
}

// QualifiedName returns the namespaced name: "server.tool".
func (dt *DiscoveredTool) QualifiedName() string {
	return dt.ServerName + "." + dt.Tool.Name
}

// InputSchemaJSON returns the tool's input schema as json.RawMessage.
func (dt *DiscoveredTool) InputSchemaJSON() json.RawMessage {
	data, err := json.Marshal(dt.Tool.InputSchema)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return data
}

// Transport type constants.
const (
	TransportSSE            = "sse"
	TransportStreamableHTTP = "streamable-http"
)

// ServerConfig holds the configuration for connecting to an external MCP server.
type ServerConfig struct {
	Name      string            `yaml:"name" json:"name"`
	URL       string            `yaml:"url" json:"url"`
	Transport string            `yaml:"transport" json:"transport"` // "sse" (default) or "streamable-http"
	Headers   map[string]string `yaml:"headers" json:"headers"`
}

// connection holds a live MCP client connection and its discovered tools.
type connection struct {
	client *client.Client
	config ServerConfig
	tools  []mcp.Tool

	// reinitMu serializes session re-initialization attempts so concurrent
	// callers that hit a terminated session don't stampede the server with
	// redundant initialize handshakes.
	reinitMu sync.Mutex
}

// Pool manages connections to external MCP servers and provides
// tool discovery and proxied tool invocation.
type Pool struct {
	mu        sync.RWMutex
	conns     map[string]*connection // server name -> persistent connection
	ephemeral map[string]*connection // server name -> ephemeral connection (shadows conns)

	// desired holds the ServerConfigs we intend to keep connected. Servers are
	// registered here by Connect/ConnectDynamic even if the initial connection
	// fails, so the reconciler can retry them until they come up.
	desired map[string]ServerConfig

	// connectMu serializes connectOne attempts so the reconciler and the
	// connect methods never race on the same server.
	connectMu sync.Mutex

	startOnce sync.Once
	stopCh    chan struct{}

	// onChange is called whenever the tool list changes (from any server).
	onChange func()
}

// NewPool creates a new MCP client pool.
func NewPool() *Pool {
	return &Pool{
		conns:     make(map[string]*connection),
		ephemeral: make(map[string]*connection),
		desired:   make(map[string]ServerConfig),
		stopCh:    make(chan struct{}),
	}
}

// OnToolsChanged registers a callback that fires when any server's tool list changes.
func (p *Pool) OnToolsChanged(fn func()) {
	p.onChange = fn
}

// Connect establishes connections to all configured MCP servers,
// initializes sessions, and discovers tools. All servers are recorded as
// desired so the reconciler keeps them connected across restarts.
func (p *Pool) Connect(ctx context.Context, servers []ServerConfig) error {
	for _, srv := range servers {
		p.addDesired(srv)
		if err := p.connectOne(ctx, srv); err != nil {
			slog.Error("failed to connect to MCP server", "name", srv.Name, "url", srv.URL, "error", err)
			// Continue connecting to other servers; don't fail hard.
			continue
		}
	}
	p.startReconciler()
	return nil
}

// connectOne connects to a single MCP server.
func (p *Pool) connectOne(ctx context.Context, srv ServerConfig) error {
	transport := srv.Transport
	if transport == "" {
		transport = TransportSSE
	}
	slog.Info("connecting to MCP server", "name", srv.Name, "url", srv.URL, "transport", transport)

	var c *client.Client
	var err error

	switch transport {
	case TransportSSE:
		var opts []mcptransport.ClientOption
		if len(srv.Headers) > 0 {
			opts = append(opts, client.WithHeaders(srv.Headers))
		}
		c, err = client.NewSSEMCPClient(srv.URL, opts...)
	case TransportStreamableHTTP:
		var opts []mcptransport.StreamableHTTPCOption
		if len(srv.Headers) > 0 {
			opts = append(opts, mcptransport.WithHTTPHeaders(srv.Headers))
		}
		c, err = client.NewStreamableHttpClient(srv.URL, opts...)
	default:
		return fmt.Errorf("unknown transport %q for server %s (use 'sse' or 'streamable-http')", transport, srv.Name)
	}
	if err != nil {
		return fmt.Errorf("create %s client for %s: %w", transport, srv.Name, err)
	}

	conn := &connection{
		client: c,
		config: srv,
	}

	// Register notification handler before Start so we don't miss any.
	serverName := srv.Name
	c.OnNotification(func(notification mcp.JSONRPCNotification) {
		if notification.Method == mcp.MethodNotificationToolsListChanged {
			slog.Info("tool list changed notification received", "server", serverName)
			p.refreshTools(serverName)
		}
	})

	// Start the transport.
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := c.Start(startCtx); err != nil {
		c.Close()
		return fmt.Errorf("start %s client for %s: %w", transport, srv.Name, err)
	}

	// Initialize the MCP session.
	initCtx, initCancel := context.WithTimeout(ctx, 15*time.Second)
	defer initCancel()
	_, err = c.Initialize(initCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "agentfoundry",
				Version: "0.1.0",
			},
			Capabilities: mcp.ClientCapabilities{},
		},
	})
	if err != nil {
		c.Close()
		return fmt.Errorf("initialize MCP session for %s: %w", srv.Name, err)
	}

	// Discover tools.
	toolsCtx, toolsCancel := context.WithTimeout(ctx, 15*time.Second)
	defer toolsCancel()
	toolsResult, err := c.ListTools(toolsCtx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return fmt.Errorf("list tools from %s: %w", srv.Name, err)
	}

	conn.tools = toolsResult.Tools

	p.mu.Lock()
	p.conns[srv.Name] = conn
	p.mu.Unlock()

	toolNames := make([]string, len(conn.tools))
	for i, t := range conn.tools {
		toolNames[i] = t.Name
	}
	slog.Info("connected to MCP server", "name", srv.Name, "tools", toolNames)

	return nil
}

// addDesired records a server as one we want to keep connected.
func (p *Pool) addDesired(srv ServerConfig) {
	p.mu.Lock()
	p.desired[srv.Name] = srv
	p.mu.Unlock()
}

// removeDesired drops a server from the desired set.
func (p *Pool) removeDesired(name string) {
	p.mu.Lock()
	delete(p.desired, name)
	p.mu.Unlock()
}

// startReconciler launches the background loop that keeps desired servers
// connected and re-discovers their tools.
func (p *Pool) startReconciler() {
	p.startOnce.Do(func() {
		go p.reconcileLoop()
	})
}

// reconcileLoop periodically checks that all desired servers are connected.
// A server that failed at startup or whose connection died (e.g. the pod
// restarted) is reconnected here, and its tools re-discovered, so newly
// available MCP servers show up in ListAllTools without waiting for a manual
// refresh. Without this, a transient failure at boot permanently hides that
// server's tools from the worker until the orchestrator restarts.
func (p *Pool) reconcileLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.reconcile()
		}
	}
}

// reconcile attempts to connect any desired server that isn't connected.
func (p *Pool) reconcile() {
	p.mu.RLock()
	targets := make([]ServerConfig, 0, len(p.desired))
	for _, srv := range p.desired {
		targets = append(targets, srv)
	}
	p.mu.RUnlock()

	for _, srv := range targets {
		p.mu.RLock()
		_, hasConn := p.conns[srv.Name]
		_, hasEph := p.ephemeral[srv.Name]
		p.mu.RUnlock()
		if hasConn || hasEph {
			continue
		}

		p.connectMu.Lock()
		err := p.connectOne(context.Background(), srv)
		p.connectMu.Unlock()
		if err != nil {
			slog.Warn("reconciler failed to connect to MCP server", "name", srv.Name, "error", err)
		} else if p.onChange != nil {
			p.onChange()
		}
	}
}

// refreshTools re-fetches the tool list from a specific server.
func (p *Pool) refreshTools(serverName string) {
	p.mu.RLock()
	conn, ok := p.conns[serverName]
	p.mu.RUnlock()
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	toolsResult, err := conn.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		slog.Error("failed to refresh tools", "server", serverName, "error", err)
		return
	}

	p.mu.Lock()
	conn.tools = toolsResult.Tools
	p.mu.Unlock()

	toolNames := make([]string, len(toolsResult.Tools))
	for i, t := range toolsResult.Tools {
		toolNames[i] = t.Name
	}
	slog.Info("refreshed tools from server", "server", serverName, "tools", toolNames)

	if p.onChange != nil {
		p.onChange()
	}
}

func (p *Pool) getConnection(name string) (*connection, bool) {
	if conn, ok := p.ephemeral[name]; ok {
		return conn, true
	}
	conn, ok := p.conns[name]
	return conn, ok
}

func (p *Pool) allConnections() map[string]*connection {
	merged := make(map[string]*connection, len(p.conns)+len(p.ephemeral))
	for k, v := range p.conns {
		merged[k] = v
	}
	for k, v := range p.ephemeral {
		merged[k] = v
	}
	return merged
}

// ListAllTools returns all discovered tools across all connected servers.
// Ephemeral servers shadow persistent servers with the same name.
func (p *Pool) ListAllTools() []DiscoveredTool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var all []DiscoveredTool
	for name, conn := range p.allConnections() {
		for _, t := range conn.tools {
			all = append(all, DiscoveredTool{
				ServerName: name,
				Tool:       t,
			})
		}
	}
	return all
}

// ListServerTools returns the tools from a specific server.
// Checks ephemeral first, then persistent.
func (p *Pool) ListServerTools(serverName string) ([]DiscoveredTool, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	conn, ok := p.getConnection(serverName)
	if !ok {
		return nil, false
	}

	tools := make([]DiscoveredTool, len(conn.tools))
	for i, t := range conn.tools {
		tools[i] = DiscoveredTool{
			ServerName: serverName,
			Tool:       t,
		}
	}
	return tools, true
}

// GetTool looks up a tool by its qualified name ("server.tool").
// Checks ephemeral first, then persistent.
func (p *Pool) GetTool(serverName, toolName string) (*DiscoveredTool, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	conn, ok := p.getConnection(serverName)
	if !ok {
		return nil, false
	}

	for _, t := range conn.tools {
		if t.Name == toolName {
			return &DiscoveredTool{
				ServerName: serverName,
				Tool:       t,
			}, true
		}
	}
	return nil, false
}

// CallTool invokes a tool on the appropriate external MCP server.
// Checks ephemeral first, then persistent.
func (p *Pool) CallTool(ctx context.Context, serverName, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
	p.mu.RLock()
	conn, ok := p.getConnection(serverName)
	p.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown MCP server: %s", serverName)
	}

	result, err := conn.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: arguments,
		},
	})
	if err != nil && errors.Is(err, mcptransport.ErrSessionTerminated) {
		result, err = p.recoverFromTerminatedSession(ctx, conn, serverName, toolName, arguments, err)
	}
	if err != nil {
		return nil, fmt.Errorf("call tool %s.%s: %w", serverName, toolName, err)
	}

	return result, nil
}

// recoverFromTerminatedSession re-initializes a connection whose MCP session
// was terminated server-side (e.g. the server restarted) and retries the tool
// call once. Re-initialization happens in place so persistent and ephemeral
// connections that share the client stay consistent.
func (p *Pool) recoverFromTerminatedSession(ctx context.Context, conn *connection, serverName, toolName string, arguments map[string]any, origErr error) (*mcp.CallToolResult, error) {
	slog.Warn("MCP session terminated, re-initializing", "server", serverName, "tool", toolName, "error", origErr)

	conn.reinitMu.Lock()
	defer conn.reinitMu.Unlock()

	newTools, rerr := reinitSession(ctx, conn.client)
	if rerr != nil {
		slog.Error("failed to re-initialize MCP session", "server", serverName, "error", rerr)
		return nil, rerr
	}

	p.mu.Lock()
	changed := toolsChanged(conn.tools, newTools)
	conn.tools = newTools
	p.mu.Unlock()
	if changed && p.onChange != nil {
		p.onChange()
	}

	slog.Info("MCP session re-initialized, retrying tool call", "server", serverName, "tool", toolName)
	return conn.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: arguments,
		},
	})
}

// reinitSession re-establishes a terminated MCP session on an existing client
// and returns the freshly discovered tool list. mcp-go clears its session ID
// when the server reports a session as terminated, so a fresh initialize
// handshake mints a new session on the server.
func reinitSession(ctx context.Context, cl *client.Client) ([]mcp.Tool, error) {
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	_, err := cl.Initialize(initCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "agentfoundry",
				Version: "0.1.0",
			},
			Capabilities: mcp.ClientCapabilities{},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("re-initialize MCP session: %w", err)
	}

	toolsCtx, toolsCancel := context.WithTimeout(ctx, 15*time.Second)
	defer toolsCancel()

	toolsResult, err := cl.ListTools(toolsCtx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("re-discover tools: %w", err)
	}
	return toolsResult.Tools, nil
}

// toolsChanged reports whether two tool lists differ by name.
func toolsChanged(a, b []mcp.Tool) bool {
	if len(a) != len(b) {
		return true
	}
	names := make(map[string]bool, len(a))
	for _, t := range a {
		names[t.Name] = true
	}
	for _, t := range b {
		if !names[t.Name] {
			return true
		}
	}
	return false
}

// ListServerNames returns the names of all connected servers (persistent + ephemeral).
func (p *Pool) ListServerNames() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	merged := p.allConnections()
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	return names
}

// EphemeralConn is a short-lived connection to a single MCP server, intended
// for use within a single agent run. It is not registered in the global Pool
// and must be closed by the caller when the run completes.
type EphemeralConn struct {
	config ServerConfig
	client *client.Client
	tools  []mcp.Tool
}

// ConnectEphemeral connects to a single MCP server outside of the global pool
// and returns an EphemeralConn. The caller is responsible for calling Close
// when the connection is no longer needed.
func ConnectEphemeral(ctx context.Context, srv ServerConfig) (*EphemeralConn, error) {
	transport := srv.Transport
	if transport == "" {
		transport = TransportSSE
	}

	var c *client.Client
	var err error

	switch transport {
	case TransportSSE:
		var opts []mcptransport.ClientOption
		if len(srv.Headers) > 0 {
			opts = append(opts, client.WithHeaders(srv.Headers))
		}
		c, err = client.NewSSEMCPClient(srv.URL, opts...)
	case TransportStreamableHTTP:
		var opts []mcptransport.StreamableHTTPCOption
		if len(srv.Headers) > 0 {
			opts = append(opts, mcptransport.WithHTTPHeaders(srv.Headers))
		}
		c, err = client.NewStreamableHttpClient(srv.URL, opts...)
	default:
		return nil, fmt.Errorf("unknown transport %q for server %s (use 'sse' or 'streamable-http')", transport, srv.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("create %s client for %s: %w", transport, srv.Name, err)
	}

	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := c.Start(startCtx); err != nil {
		c.Close()
		return nil, fmt.Errorf("start %s client for %s: %w", transport, srv.Name, err)
	}

	_, err = c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "agentfoundry",
				Version: "0.1.0",
			},
			Capabilities: mcp.ClientCapabilities{},
		},
	})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize MCP session for %s: %w", srv.Name, err)
	}

	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("list tools from %s: %w", srv.Name, err)
	}

	toolNames := make([]string, len(toolsResult.Tools))
	for i, t := range toolsResult.Tools {
		toolNames[i] = t.Name
	}
	slog.Info("ephemeral MCP connection established", "name", srv.Name, "tools", toolNames)

	return &EphemeralConn{
		config: srv,
		client: c,
		tools:  toolsResult.Tools,
	}, nil
}

// ServerName returns the name this server was registered under.
func (e *EphemeralConn) ServerName() string {
	return e.config.Name
}

// ListTools returns all tools discovered from this ephemeral server.
func (e *EphemeralConn) ListTools() []DiscoveredTool {
	tools := make([]DiscoveredTool, len(e.tools))
	for i, t := range e.tools {
		tools[i] = DiscoveredTool{
			ServerName: e.config.Name,
			Tool:       t,
		}
	}
	return tools
}

// CallTool invokes a tool on this ephemeral server.
func (e *EphemeralConn) CallTool(ctx context.Context, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
	result, err := e.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: arguments,
		},
	})
	if err != nil && errors.Is(err, mcptransport.ErrSessionTerminated) {
		slog.Warn("ephemeral MCP session terminated, re-initializing", "server", e.config.Name, "tool", toolName, "error", err)
		newTools, rerr := reinitSession(ctx, e.client)
		if rerr != nil {
			return nil, rerr
		}
		e.tools = newTools
		result, err = e.client.CallTool(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name:      toolName,
				Arguments: arguments,
			},
		})
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Close shuts down the ephemeral MCP connection.
func (e *EphemeralConn) Close() {
	slog.Info("closing ephemeral MCP connection", "server", e.config.Name)
	e.client.Close()
}

// RegisterEphemeral adds an EphemeralConn to the pool under its server name.
// Tools from the ephemeral connection become visible via ListAllTools and
// CallTool. Call UnregisterEphemeral to remove it.
func (p *Pool) RegisterEphemeral(e *EphemeralConn) {
	conn := &connection{
		client: e.client,
		config: e.config,
		tools:  e.tools,
	}
	p.mu.Lock()
	p.ephemeral[e.config.Name] = conn
	p.mu.Unlock()
	slog.Info("registered ephemeral MCP server in pool", "name", e.config.Name, "tools", len(e.tools))
}

// UnregisterEphemeral removes a server from the pool by name and closes its
// underlying connection. If the server was not registered this is a no-op.
func (p *Pool) UnregisterEphemeral(name string) {
	p.mu.Lock()
	conn, ok := p.ephemeral[name]
	if !ok {
		p.mu.Unlock()
		return
	}
	delete(p.ephemeral, name)
	p.mu.Unlock()
	conn.client.Close()
	slog.Info("unregistered ephemeral MCP server from pool", "name", name)
}

// ConnectDynamic connects to an MCP server at runtime and adds it to the pool.
// Returns an error if the connection fails. The server is recorded as desired
// so the reconciler reconnects it if it goes down or if this initial attempt
// fails.
func (p *Pool) ConnectDynamic(ctx context.Context, srv ServerConfig) error {
	p.addDesired(srv)
	p.connectMu.Lock()
	err := p.connectOne(ctx, srv)
	p.connectMu.Unlock()
	p.startReconciler()
	return err
}

// DisconnectDynamic removes an MCP server from the pool by name and closes its
// connection. If the server is not in the pool this is a no-op.
func (p *Pool) DisconnectDynamic(name string) {
	p.removeDesired(name)
	p.mu.Lock()
	conn, ok := p.conns[name]
	if !ok {
		p.mu.Unlock()
		return
	}
	delete(p.conns, name)
	p.mu.Unlock()

	conn.client.Close()
	slog.Info("disconnected dynamic MCP server", "name", name)
}

// ServerStatus describes whether a server is connected and what tools it has.
type ServerStatus struct {
	Connected bool
	Tools     []mcp.Tool
}

// GetServerStatus returns the connection status and discovered tools for a
// server by name. Checks persistent connections only (not ephemeral).
func (p *Pool) GetServerStatus(name string) ServerStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	conn, ok := p.conns[name]
	if !ok {
		return ServerStatus{}
	}

	tools := make([]mcp.Tool, len(conn.tools))
	copy(tools, conn.tools)
	return ServerStatus{Connected: true, Tools: tools}
}

// RefreshServer re-discovers tools from a specific server by name.
func (p *Pool) RefreshServer(name string) {
	p.refreshTools(name)
}

// Close shuts down all MCP client connections.
func (p *Pool) Close() {
	select {
	case <-p.stopCh:
	default:
		close(p.stopCh)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for name, conn := range p.ephemeral {
		slog.Info("closing ephemeral MCP connection", "server", name)
		conn.client.Close()
	}
	for name, conn := range p.conns {
		slog.Info("closing MCP client connection", "server", name)
		conn.client.Close()
	}
	p.ephemeral = make(map[string]*connection)
	p.conns = make(map[string]*connection)
	p.desired = make(map[string]ServerConfig)
}
