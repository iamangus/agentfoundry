package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/mcpclient"
	"github.com/angoo/agentfoundry/internal/registry"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

func managerFixture(t *testing.T) (*Manager, *mcpclient.Pool, *server.MCPServer, string) {
	t.Helper()
	upstream := server.NewMCPServer("upstream", "1.0.0", server.WithToolCapabilities(true))
	tool := mcp.NewTool("lookup", mcp.WithReadOnlyHintAnnotation(true), mcp.WithString("region", func(schema map[string]any) { schema["x-mcp-header"] = "Region" }))
	tool.RawOutputSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)
	upstream.AddTool(tool, func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultStructuredOnly(map[string]any{"answer": 42}), nil
	})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(upstream))
	t.Cleanup(ts.Close)
	pool := mcpclient.NewPool()
	t.Cleanup(pool.Close)
	require.NoError(t, pool.ConnectDynamic(t.Context(), mcpclient.ServerConfig{Name: "search", URL: ts.URL, Transport: mcpclient.TransportStreamableHTTP}))
	reg := registry.New()
	require.NoError(t, reg.RegisterAgent(&config.Definition{Name: "researcher", Tools: []string{"search.lookup"}}))
	m := NewManager(reg, pool, nil, WithAllowedOrigins("https://foundry.example.com"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		m.Shutdown(ctx)
	})
	pool.OnToolsChanged(m.RefreshAll)
	mux := http.NewServeMux()
	m.RegisterRoutes(mux)
	downstream := httptest.NewServer(mux)
	t.Cleanup(downstream.Close)
	return m, pool, upstream, downstream.URL
}

func downstreamClient(t *testing.T, url string, legacy bool) *client.Client {
	t.Helper()
	tr, err := transport.NewStreamableHTTP(url)
	require.NoError(t, err)
	var opts []client.ClientOption
	if legacy {
		opts = append(opts, client.WithLegacyProtocolOnly())
	}
	c := client.NewClient(tr, opts...)
	t.Cleanup(func() { c.Close() })
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "test", Version: "1.0"}}})
	require.NoError(t, err)
	return c
}

func TestModernAndLegacyGateway(t *testing.T) {
	_, _, _, base := managerFixture(t)
	for _, path := range []string{"default", "researcher"} {
		for _, legacy := range []bool{false, true} {
			c := downstreamClient(t, base+"/servers/"+path, legacy)
			if legacy {
				require.Equal(t, "2025-11-25", c.ProtocolVersion())
			} else {
				require.Equal(t, "2026-07-28", c.ProtocolVersion())
				require.Empty(t, c.GetSessionId())
			}
			tools, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
			require.NoError(t, err)
			require.Len(t, tools.Tools, 1)
			require.Equal(t, "search.lookup", tools.Tools[0].Name)
			require.NotEmpty(t, tools.Tools[0].OutputSchema.Type)
			require.NotNil(t, tools.Tools[0].Annotations.ReadOnlyHint)
			require.True(t, *tools.Tools[0].Annotations.ReadOnlyHint)
			require.Equal(t, "Region", tools.Tools[0].InputSchema.Properties["region"].(map[string]any)["x-mcp-header"])
			result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "search.lookup", Arguments: map[string]any{"region": "日本"}}})
			require.NoError(t, err)
			require.NotNil(t, result.StructuredContent)
			c.Close()
		}
	}
}

func modernRequest(method string) *http.Request {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "test", "method": method, "params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
	r := httptest.NewRequest("POST", "http://localhost/servers/default", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Mcp-Method", method)
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	return r
}

func TestModernDiscoveryMetadataAndOrigin(t *testing.T) {
	m, _, _, _ := managerFixture(t)
	mux := http.NewServeMux()
	m.RegisterRoutes(mux)
	r := modernRequest("server/discover")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"resultType":"complete"`)
	require.Contains(t, w.Body.String(), "2026-07-28")
	require.Empty(t, w.Header().Get("Mcp-Session-Id"))
	r = modernRequest("tools/list")
	r.Header.Set("Mcp-Method", "tools/call")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	require.Equal(t, 400, w.Code)
	require.Contains(t, w.Body.String(), "-32020")
	r = modernRequest("tools/list")
	r.Header.Set("Origin", "https://evil.example.com")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	require.Equal(t, 403, w.Code)
	r = modernRequest("tools/list")
	r.Header.Set("Origin", "https://foundry.example.com")
	r.Header.Set("Last-Event-ID", "obsolete")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	require.Equal(t, 200, w.Code)
	for _, method := range []string{"GET", "DELETE"} {
		r := modernRequest("tools/list")
		r.Method = method
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		require.Equal(t, 405, w.Code)
	}
	r = modernRequest("tools/list")
	r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":"test","method":"tools/list","params":{}}`))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	require.Equal(t, 400, w.Code)
}

func TestRefreshKeepsSubscriptionsAndReportsSchemaChanges(t *testing.T) {
	m, _, upstream, base := managerFixture(t)
	c := downstreamClient(t, base+"/servers/default", false)
	var changed atomic.Int32
	ack := make(chan struct{}, 1)
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationToolsListChanged {
			changed.Add(1)
		}
		if n.Method == mcp.MethodNotificationSubscriptionsAcknowledged {
			select {
			case ack <- struct{}{}:
			default:
			}
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop, err := c.ListenAsync(ctx, mcp.SubscriptionFilter{ToolsListChanged: true}, nil)
	require.NoError(t, err)
	defer stop()
	select {
	case <-ack:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription not acknowledged")
	}
	original := m.getOrCreateServer("default")
	require.Eventually(t, func() bool {
		upstream.AddTool(mcp.NewTool("lookup", mcp.WithDescription("updated schema"), mcp.WithInteger("count")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("updated"), nil
		})
		return changed.Load() > 0
	}, 3*time.Second, 20*time.Millisecond)
	require.Same(t, original, m.getOrCreateServer("default"))
	tools, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	require.Len(t, tools.Tools, 1)
	require.Equal(t, "updated schema", tools.Tools[0].Description)
}

func TestShutdownClosesModernListenStreams(t *testing.T) {
	m, _, _, base := managerFixture(t)
	c := downstreamClient(t, base+"/servers/default", false)
	ack := make(chan struct{}, 1)
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationSubscriptionsAcknowledged {
			select {
			case ack <- struct{}{}:
			default:
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- c.Listen(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}) }()
	select {
	case <-ack:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription not established")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Shutdown(ctx)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("modern listen survived shutdown")
	}
}
