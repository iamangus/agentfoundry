package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func negotiate(ctx context.Context, c *client.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Initialize is the SDK's dual-era negotiation API: it probes server/discover
	// first, and only sends initialize/initialized for a legacy server.
	version := mcp.LATEST_PROTOCOL_VERSION
	if _, legacySSE := c.GetTransport().(*mcptransport.SSE); legacySSE {
		// Deprecated HTTP+SSE is not a modern binding. Do not negotiate a
		// modern core over it even when talking to a dual-era SDK server.
		version = mcp.LATEST_LEGACY_PROTOCOL_VERSION
	}
	_, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: version,
		ClientInfo:      mcp.Implementation{Name: "agentfoundry", Version: "1.0.0"},
		Capabilities:    mcp.ClientCapabilities{},
	}})
	return err
}

func openConnection(ctx context.Context, srv ServerConfig) (*connection, error) {
	kind := srv.Transport
	if kind == "" {
		kind = TransportSSE
	} // Preserve stored/attached legacy configurations.
	lifetime, cancel := context.WithCancel(context.Background())
	conn := &connection{config: srv, lifetime: lifetime, cancel: cancel, refreshRequests: make(chan struct{}, 1)}
	headers := make(map[string]string, len(srv.Headers))
	for key, value := range srv.Headers {
		switch strings.ToLower(key) {
		case "mcp-protocol-version", "mcp-method", "mcp-name", "mcp-session-id", "last-event-id", "accept", "content-type":
			continue
		}
		if strings.HasPrefix(strings.ToLower(key), "mcp-param-") {
			continue
		}
		headers[key] = value
	}
	var err error
	switch kind {
	case TransportSSE:
		conn.client, err = client.NewSSEMCPClient(srv.URL, client.WithHeaders(headers))
	case TransportStreamableHTTP:
		base := http.DefaultTransport.(*http.Transport).Clone()
		httpClient := &http.Client{Transport: &modernHTTP{base: base}}
		conn.closeHTTP = base.CloseIdleConnections
		conn.client, err = client.NewStreamableHttpClient(srv.URL,
			mcptransport.WithHTTPHeaders(headers), mcptransport.WithHTTPBasicClient(httpClient))
	default:
		cancel()
		return nil, fmt.Errorf("unknown transport %q for server %s (use 'sse' or 'streamable-http')", kind, srv.Name)
	}
	if err != nil {
		cancel()
		if conn.closeHTTP != nil {
			conn.closeHTTP()
		}
		return nil, err
	}
	// SSE Start's context owns its long-lived GET stream. Detach that lifetime
	// from the caller after setup, while still bounding and cancelling setup.
	stopCaller := context.AfterFunc(ctx, conn.cancel)
	defer stopCaller()
	startTimer := time.AfterFunc(30*time.Second, conn.cancel)
	err = conn.client.Start(conn.lifetime)
	startTimer.Stop()
	if err != nil {
		conn.close()
		return nil, fmt.Errorf("start MCP transport: %w", err)
	}
	if err := negotiate(ctx, conn.client); err != nil {
		conn.close()
		return nil, fmt.Errorf("negotiate MCP protocol: %w", err)
	}
	toolsCtx, toolsCancel := context.WithTimeout(ctx, 15*time.Second)
	defer toolsCancel()
	tools, err := conn.client.ListTools(toolsCtx, mcp.ListToolsRequest{})
	if err != nil {
		conn.close()
		return nil, fmt.Errorf("discover MCP tools: %w", err)
	}
	conn.tools = tools.Tools
	go func() {
		for {
			select {
			case <-conn.lifetime.Done():
				return
			case <-conn.refreshRequests:
				conn.refreshTools()
			}
		}
	}()
	conn.client.OnNotification(func(notification mcp.JSONRPCNotification) {
		if notification.Method == mcp.MethodNotificationToolsListChanged || notification.Method == mcp.MethodNotificationSubscriptionsAcknowledged {
			// Acknowledgement also closes the list/subscribe race on startup.
			// Don't block the stream while making another request.
			select {
			case conn.refreshRequests <- struct{}{}:
			default:
			}
		}
	})
	return conn, nil
}

// Modern HTTP errors must not enter mcp-go v1.3.0's legacy 404/session recovery
// path or downgrade to initialize after an unambiguous modern discovery error.
type modernHTTP struct {
	base             http.RoundTripper
	mu               sync.Mutex
	discoveryError   string
	discoveredModern bool
}

func (t *modernHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	method := r.Header.Get("Mcp-Method")
	t.mu.Lock()
	discoveryError := t.discoveryError
	discoveredModern := t.discoveredModern
	t.mu.Unlock()
	if method == "" && discoveredModern && r.GetBody != nil {
		body, err := r.GetBody()
		if err == nil {
			var message struct {
				Method string `json:"method"`
			}
			json.NewDecoder(body).Decode(&message)
			body.Close()
			method = message.Method
		}
	}
	if method == "initialize" && discoveredModern {
		return nil, fmt.Errorf("modern MCP discovery failed; refusing legacy downgrade: %s", discoveryError)
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil || !mcp.IsModernProtocol(r.Header.Get("MCP-Protocol-Version")) || resp.StatusCode < 400 {
		return resp, err
	}
	// Error bodies are bounded; successful JSON and SSE responses pass through.
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
	resp.Body.Close()
	if readErr != nil || len(b) > 64<<10 {
		return nil, fmt.Errorf("invalid or oversized MCP HTTP error response")
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	var message struct {
		JSONRPC string `json:"jsonrpc"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	valid := json.Unmarshal(b, &message) == nil && message.JSONRPC == "2.0" && message.Error != nil
	if method == "server/discover" && valid && (message.Error.Code == -32020 || message.Error.Code == -32021 || message.Error.Code == -32022 || (resp.StatusCode == 404 && message.Error.Code == -32601)) {
		t.mu.Lock()
		t.discoveryError = message.Error.Message
		t.discoveredModern = true
		t.mu.Unlock()
	}
	if resp.StatusCode == http.StatusNotFound {
		if !valid {
			// During discovery an old HTTP endpoint may return a bare 404;
			// leave it intact so the SDK can try the legacy handshake.
			if method == "server/discover" {
				return resp, nil
			}
			return nil, fmt.Errorf("MCP endpoint returned HTTP 404 without a JSON-RPC error")
		}
		// Let the SDK parse the actual RPC error instead of discarding its body
		// and marking the (nonexistent) modern session terminated.
		resp.StatusCode, resp.Status = http.StatusOK, "200 OK"
		resp.Header.Set("Content-Type", "application/json")
	}
	return resp, nil
}

func (conn *connection) startListener() {
	capabilities := conn.client.GetServerCapabilities()
	if !mcp.IsModernProtocol(conn.client.ProtocolVersion()) || capabilities.Tools == nil || !capabilities.Tools.ListChanged {
		return
	}
	ctx, cancel := context.WithCancel(conn.lifetime)
	conn.stopListener = cancel
	go func() {
		for ctx.Err() == nil {
			err := conn.client.Listen(ctx, mcp.SubscriptionFilter{ToolsListChanged: true})
			if ctx.Err() != nil {
				return
			}
			slog.Warn("MCP tool subscription closed; reconnecting", "server", conn.config.Name, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

func (conn *connection) close() {
	conn.closeOnce.Do(func() {
		conn.cancel()
		if conn.stopListener != nil {
			conn.stopListener()
		}
		conn.client.Close()
		if conn.closeHTTP != nil {
			conn.closeHTTP()
		}
	})
}

func (conn *connection) getTools() []mcp.Tool {
	conn.toolsMu.RLock()
	defer conn.toolsMu.RUnlock()
	return append([]mcp.Tool(nil), conn.tools...)
}

func (conn *connection) updateTools(tools []mcp.Tool) bool {
	conn.toolsMu.Lock()
	defer conn.toolsMu.Unlock()
	changed := toolsChanged(conn.tools, tools)
	conn.tools = tools
	return changed
}

func (conn *connection) refreshTools() {
	ctx, cancel := context.WithTimeout(conn.lifetime, 15*time.Second)
	defer cancel()
	conn.requestMu.RLock()
	tools, err := conn.client.ListTools(ctx, mcp.ListToolsRequest{})
	conn.requestMu.RUnlock()
	if err != nil {
		if conn.lifetime.Err() != nil {
			return
		}
		slog.Warn("MCP tool refresh failed", "server", conn.config.Name, "error", err)
		return
	}
	if conn.updateTools(tools.Tools) {
		conn.notify()
	}
}

func (conn *connection) notify() {
	conn.notifyMu.RLock()
	fn := conn.onChange
	conn.notifyMu.RUnlock()
	if fn != nil {
		fn()
	}
}

func (conn *connection) onToolsChanged(fn func()) {
	conn.notifyMu.Lock()
	conn.onChange = fn
	conn.notifyMu.Unlock()
}
