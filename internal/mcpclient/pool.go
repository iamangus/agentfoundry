package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
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
	if len(dt.Tool.RawInputSchema) > 0 {
		return append(json.RawMessage(nil), dt.Tool.RawInputSchema...)
	}
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
	client          *client.Client
	config          ServerConfig
	tools           []mcp.Tool
	refs            int
	toolsMu         sync.RWMutex
	requestMu       sync.RWMutex
	stopListener    context.CancelFunc
	closeHTTP       func()
	closeOnce       sync.Once
	notifyMu        sync.RWMutex
	onChange        func()
	lifetime        context.Context
	cancel          context.CancelFunc
	refreshRequests chan struct{}

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
	stopOnce  sync.Once
	ctx       context.Context
	cancel    context.CancelFunc

	// onChange is called whenever the tool list changes (from any server).
	onChange func()
}

// NewPool creates a new MCP client pool.
func NewPool() *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{
		conns:     make(map[string]*connection),
		ephemeral: make(map[string]*connection),
		desired:   make(map[string]ServerConfig),
		stopCh:    make(chan struct{}),
		ctx:       ctx, cancel: cancel,
	}
}

// OnToolsChanged registers a callback that fires when any server's tool list changes.
func (p *Pool) OnToolsChanged(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = fn
}

func (p *Pool) notify() {
	if p.ctx.Err() != nil {
		return
	}
	p.mu.RLock()
	fn := p.onChange
	p.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Connect establishes connections to all configured MCP servers,
// initializes sessions, and discovers tools. All servers are recorded as
// desired so the reconciler keeps them connected across restarts.
func (p *Pool) Connect(ctx context.Context, servers []ServerConfig) error {
	for _, srv := range servers {
		p.addDesired(srv)
		p.connectMu.Lock()
		err := p.connectOne(ctx, srv)
		p.connectMu.Unlock()
		if err != nil {
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
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	defer cancel()
	conn, err := openConnection(ctx, srv)
	if err != nil {
		return fmt.Errorf("connect MCP server %s: %w", srv.Name, err)
	}
	conn.onToolsChanged(p.notify)

	p.mu.Lock()
	if p.ctx.Err() != nil {
		p.mu.Unlock()
		conn.close()
		return fmt.Errorf("MCP pool is closed")
	}
	old := p.conns[srv.Name]
	conn.startListener()
	p.conns[srv.Name] = conn
	p.mu.Unlock()
	if old != nil {
		old.close()
	}

	tools := conn.getTools()
	toolNames := make([]string, len(tools))
	for i, t := range tools {
		toolNames[i] = t.Name
	}
	conn.requestMu.RLock()
	version := conn.client.ProtocolVersion()
	conn.requestMu.RUnlock()
	slog.Info("connected to MCP server", "name", srv.Name, "protocol", version, "tools", toolNames)

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
		err := p.connectOne(p.ctx, srv)
		p.connectMu.Unlock()
		if err != nil {
			slog.Warn("reconciler failed to connect to MCP server", "name", srv.Name, "error", err)
		} else {
			p.notify()
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

	conn.refreshTools()
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
		for _, t := range conn.getTools() {
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

	discovered := conn.getTools()
	tools := make([]DiscoveredTool, len(discovered))
	for i, t := range discovered {
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

	for _, t := range conn.getTools() {
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

	conn.requestMu.RLock()
	result, err := conn.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: arguments,
		},
	})
	modern := mcp.IsModernProtocol(conn.client.ProtocolVersion())
	conn.requestMu.RUnlock()
	if err != nil && !modern && errors.Is(err, mcptransport.ErrSessionTerminated) {
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
	conn.requestMu.Lock()
	defer conn.requestMu.Unlock()

	newTools, rerr := reinitSession(ctx, conn.client)
	if rerr != nil {
		slog.Error("failed to re-initialize MCP session", "server", serverName, "error", rerr)
		return nil, rerr
	}

	if conn.updateTools(newTools) {
		p.notify()
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

	err := negotiate(initCtx, cl)
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

// toolsChanged includes schema and metadata changes, not just names.
func toolsChanged(a, b []mcp.Tool) bool {
	return !reflect.DeepEqual(a, b)
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
	*connection
}

// ConnectEphemeral connects to a single MCP server outside of the global pool
// and returns an EphemeralConn. The caller is responsible for calling Close
// when the connection is no longer needed.
func ConnectEphemeral(ctx context.Context, srv ServerConfig) (*EphemeralConn, error) {
	conn, err := openConnection(ctx, srv)
	if err != nil {
		return nil, err
	}
	conn.startListener()
	slog.Info("ephemeral MCP connection established", "name", srv.Name, "protocol", conn.client.ProtocolVersion(), "tools", len(conn.getTools()))
	return &EphemeralConn{connection: conn}, nil
}

// ServerName returns the name this server was registered under.
func (e *EphemeralConn) ServerName() string {
	return e.config.Name
}

// ListTools returns all tools discovered from this ephemeral server.
func (e *EphemeralConn) ListTools() []DiscoveredTool {
	discovered := e.getTools()
	tools := make([]DiscoveredTool, len(discovered))
	for i, t := range discovered {
		tools[i] = DiscoveredTool{
			ServerName: e.config.Name,
			Tool:       t,
		}
	}
	return tools
}

// CallTool invokes a tool on this ephemeral server.
func (e *EphemeralConn) CallTool(ctx context.Context, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
	e.requestMu.RLock()
	result, err := e.client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: arguments,
		},
	})
	modern := mcp.IsModernProtocol(e.client.ProtocolVersion())
	e.requestMu.RUnlock()
	if err != nil && !modern && errors.Is(err, mcptransport.ErrSessionTerminated) {
		e.reinitMu.Lock()
		defer e.reinitMu.Unlock()
		e.requestMu.Lock()
		defer e.requestMu.Unlock()
		slog.Warn("ephemeral MCP session terminated, re-initializing", "server", e.config.Name, "tool", toolName, "error", err)
		newTools, rerr := reinitSession(ctx, e.client)
		if rerr != nil {
			return nil, rerr
		}
		if e.updateTools(newTools) {
			e.notify()
		}
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
	e.close()
}

// RegisterEphemeral adds an EphemeralConn to the pool under its server name.
// Tools from the ephemeral connection become visible via ListAllTools and
// CallTool. Call UnregisterEphemeral to remove it.
func (p *Pool) RegisterEphemeral(e *EphemeralConn) error {
	conn := e.connection
	p.mu.Lock()
	if existing := p.ephemeral[e.config.Name]; existing != nil {
		if !reflect.DeepEqual(existing.config, e.config) {
			p.mu.Unlock()
			return fmt.Errorf("MCP server %q already attached with different configuration", e.config.Name)
		}
		existing.refs++
		p.mu.Unlock()
		if existing != conn {
			e.Close()
		}
		return nil
	}
	conn.refs = 1
	conn.onToolsChanged(p.notify)
	p.ephemeral[e.config.Name] = conn
	p.mu.Unlock()
	p.notify()
	slog.Info("registered ephemeral MCP server in pool", "name", e.config.Name, "tools", len(e.getTools()))
	return nil
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
	if conn.refs > 1 {
		conn.refs--
		p.mu.Unlock()
		return
	}
	delete(p.ephemeral, name)
	p.mu.Unlock()
	conn.close()
	slog.Info("unregistered ephemeral MCP server from pool", "name", name)
	p.notify()
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
	if err == nil {
		p.notify()
	}
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

	conn.close()
	slog.Info("disconnected dynamic MCP server", "name", name)
	p.notify()
}

// ServerStatus describes whether a server is connected and what tools it has.
type ServerStatus struct {
	Connected       bool
	Tools           []mcp.Tool
	ProtocolVersion string
}

// GetServerStatus returns the connection status and discovered tools for a
// server by name. Checks persistent connections only (not ephemeral).
func (p *Pool) GetServerStatus(name string) ServerStatus {
	p.mu.RLock()
	conn, ok := p.conns[name]
	p.mu.RUnlock()
	if !ok {
		return ServerStatus{}
	}

	conn.requestMu.RLock()
	version := conn.client.ProtocolVersion()
	conn.requestMu.RUnlock()
	return ServerStatus{Connected: true, Tools: conn.getTools(), ProtocolVersion: version}
}

// RefreshServer re-discovers tools from a specific server by name.
func (p *Pool) RefreshServer(name string) {
	p.refreshTools(name)
}

// Close shuts down all MCP client connections.
func (p *Pool) Close() {
	p.stopOnce.Do(func() { p.cancel(); close(p.stopCh) })

	p.mu.Lock()
	connections := make([]*connection, 0, len(p.conns)+len(p.ephemeral))
	for _, conn := range p.conns {
		connections = append(connections, conn)
	}
	for _, conn := range p.ephemeral {
		connections = append(connections, conn)
	}
	p.ephemeral = make(map[string]*connection)
	p.conns = make(map[string]*connection)
	p.desired = make(map[string]ServerConfig)
	p.mu.Unlock()
	for _, conn := range connections {
		slog.Info("closing MCP connection", "server", conn.config.Name)
		conn.close()
	}
}
