package mcpclient

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

// newTestMCPServer starts an in-process Streamable HTTP MCP server with a
// single "echo" tool. When killToolCall is true, the next tools/call POST is
// answered with 404 to simulate a terminated session. If persistentKill is
// false the flag is reset after one 404 so a re-initialized client can
// succeed; if true every tools/call POST keeps failing.
func newTestMCPServer(t *testing.T, killToolCall *atomic.Bool, initializeCount *atomic.Int32, persistentKill bool) *httptest.Server {
	t.Helper()

	srv := mcpserver.NewMCPServer("test-mcp", "0.1.0")
	srv.AddTool(mcp.Tool{
		Name:        "echo",
		Description: "echoes input",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"message": map[string]any{"type": "string"},
			},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	handler := mcpserver.NewStreamableHTTPServer(srv)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if bytes.Contains(body, []byte(`"initialize"`)) {
				initializeCount.Add(1)
			}
			if killToolCall.Load() && bytes.Contains(body, []byte(`"tools/call"`)) {
				if !persistentKill {
					killToolCall.Store(false)
				}
				w.WriteHeader(http.StatusNotFound)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestPoolCallToolRecoversFromTerminatedSession(t *testing.T) {
	var killToolCall atomic.Bool
	var initializeCount atomic.Int32

	ts := newTestMCPServer(t, &killToolCall, &initializeCount, false)
	pool := NewPool()
	defer pool.Close()

	ctx := context.Background()
	require.NoError(t, pool.ConnectDynamic(ctx, ServerConfig{
		Name:      "test-server",
		URL:       ts.URL,
		Transport: TransportStreamableHTTP,
	}))

	_, err := pool.CallTool(ctx, "test-server", "echo", map[string]any{"message": "hi"})
	require.NoError(t, err)

	killToolCall.Store(true)

	_, err = pool.CallTool(ctx, "test-server", "echo", map[string]any{"message": "hi"})
	require.NoError(t, err, "call should recover after session termination")
	require.GreaterOrEqual(t, initializeCount.Load(), int32(2), "session should have been re-initialized")
}

func TestPoolCallToolReturnsErrorWhenRecoveryFails(t *testing.T) {
	var killToolCall atomic.Bool
	var initializeCount atomic.Int32

	ts := newTestMCPServer(t, &killToolCall, &initializeCount, true)
	pool := NewPool()
	defer pool.Close()

	ctx := context.Background()
	require.NoError(t, pool.ConnectDynamic(ctx, ServerConfig{
		Name:      "test-server",
		URL:       ts.URL,
		Transport: TransportStreamableHTTP,
	}))

	_, err := pool.CallTool(ctx, "test-server", "echo", map[string]any{"message": "hi"})
	require.NoError(t, err)

	killToolCall.Store(true)

	_, err = pool.CallTool(ctx, "test-server", "echo", map[string]any{"message": "hi"})
	require.Error(t, err, "persistently terminated session should surface an error")
}

func TestEphemeralCallToolRecoversFromTerminatedSession(t *testing.T) {
	var killToolCall atomic.Bool
	var initializeCount atomic.Int32

	ts := newTestMCPServer(t, &killToolCall, &initializeCount, false)
	ctx := context.Background()

	econn, err := ConnectEphemeral(ctx, ServerConfig{
		Name:      "test-server",
		URL:       ts.URL,
		Transport: TransportStreamableHTTP,
	})
	require.NoError(t, err)
	defer econn.Close()

	_, err = econn.CallTool(ctx, "echo", map[string]any{"message": "hi"})
	require.NoError(t, err)

	killToolCall.Store(true)

	_, err = econn.CallTool(ctx, "echo", map[string]any{"message": "hi"})
	require.NoError(t, err, "call should recover after session termination")
	require.GreaterOrEqual(t, initializeCount.Load(), int32(2), "session should have been re-initialized")
}
