package mcpclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

type modernCounts struct {
	requests, initializations, gets, deletes, calls atomic.Int32
	failCall                                        atomic.Bool
}

func modernServer(t *testing.T, counts *modernCounts, changes bool) (*httptest.Server, *mcpserver.MCPServer) {
	t.Helper()
	core := mcpserver.NewMCPServer("modern-only", "1.0.0", mcpserver.WithToolCapabilities(changes))
	tool := mcp.NewTool("echo", mcp.WithString("message", mcp.Required(), func(schema map[string]any) { schema["x-mcp-header"] = "Message" }))
	core.AddTool(tool, func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		counts.calls.Add(1)
		return mcp.NewToolResultStructured(map[string]any{"echo": r.GetArguments()["message"]}, "ok"), nil
	})
	handler := mcpserver.NewStreamableHTTPServer(core, mcpserver.WithStreamableHTTPProtocolVersions(mcp.ProtocolVersion20260728))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			counts.gets.Add(1)
			http.Error(w, "no GET", 405)
			return
		case "DELETE":
			counts.deletes.Add(1)
			http.Error(w, "no sessions", 405)
			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string                     `json:"name"`
				Arguments map[string]any             `json:"arguments"`
				Meta      map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(body, &message))
		if message.Method == "initialize" {
			counts.initializations.Add(1)
			http.Error(w, "no initialize", 400)
			return
		}
		counts.requests.Add(1)
		require.Equal(t, "2026-07-28", r.Header.Get("MCP-Protocol-Version"))
		require.Equal(t, message.Method, r.Header.Get("Mcp-Method"))
		require.JSONEq(t, `"2026-07-28"`, string(message.Params.Meta["io.modelcontextprotocol/protocolVersion"]))
		require.NotEmpty(t, message.Params.Meta["io.modelcontextprotocol/clientCapabilities"])
		require.Empty(t, r.Header.Get("Mcp-Session-Id"))
		if message.Method == "tools/call" {
			require.Equal(t, message.Params.Name, r.Header.Get("Mcp-Name"))
			if message.Params.Name == "echo" {
				value, _ := message.Params.Arguments["message"].(string)
				header := r.Header.Get("Mcp-Param-Message")
				if strings.HasPrefix(header, "=?base64?") {
					b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(header, "=?base64?"), "?="))
					require.NoError(t, err)
					header = string(b)
				}
				require.Equal(t, value, header)
			}
			if counts.failCall.Swap(false) {
				w.WriteHeader(404)
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, core
}

func TestModernPoolAndEphemeralNegotiation(t *testing.T) {
	var counts modernCounts
	ts, _ := modernServer(t, &counts, false)
	pool := NewPool()
	defer pool.Close()
	cfg := ServerConfig{Name: "modern", URL: ts.URL, Transport: TransportStreamableHTTP, Headers: map[string]string{"Authorization": "Bearer configured"}}
	require.NoError(t, pool.ConnectDynamic(t.Context(), cfg))
	require.Equal(t, "2026-07-28", pool.GetServerStatus("modern").ProtocolVersion)
	result, err := pool.CallTool(t.Context(), "modern", "echo", map[string]any{"message": "Hello 世界"})
	require.NoError(t, err)
	require.NotNil(t, result.StructuredContent)
	e, err := ConnectEphemeral(t.Context(), cfg)
	require.NoError(t, err)
	_, err = e.CallTool(t.Context(), "echo", map[string]any{"message": "ephemeral"})
	require.NoError(t, err)
	require.NoError(t, pool.RegisterEphemeral(e))
	_, err = pool.CallTool(t.Context(), "modern", "echo", map[string]any{"message": "registered"})
	require.NoError(t, err)
	pool.UnregisterEphemeral("modern")
	pool.Close()
	require.Zero(t, counts.initializations.Load())
	require.Zero(t, counts.gets.Load())
	require.Zero(t, counts.deletes.Load())
	require.Equal(t, int32(3), counts.calls.Load())
}

func TestModern404DoesNotReinitializeOrPoisonConnection(t *testing.T) {
	var counts modernCounts
	ts, _ := modernServer(t, &counts, false)
	pool := NewPool()
	defer pool.Close()
	require.NoError(t, pool.ConnectDynamic(t.Context(), ServerConfig{Name: "modern", URL: ts.URL, Transport: TransportStreamableHTTP}))
	_, err := pool.CallTool(t.Context(), "modern", "missing", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
	counts.failCall.Store(true)
	_, err = pool.CallTool(t.Context(), "modern", "echo", map[string]any{"message": "fails once"})
	require.Error(t, err)
	_, err = pool.CallTool(t.Context(), "modern", "echo", map[string]any{"message": "still usable"})
	require.NoError(t, err)
	require.Zero(t, counts.initializations.Load())
	require.Equal(t, int32(1), counts.calls.Load())
}

func TestModernDiscoveryErrorsNeverDowngrade(t *testing.T) {
	for _, code := range []int{-32020, -32021, -32022, -32601} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var inits atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string          `json:"method"`
					ID     json.RawMessage `json:"id"`
				}
				json.NewDecoder(r.Body).Decode(&req)
				if req.Method == "initialize" {
					inits.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				status := 400
				if code == -32601 {
					status = 404
				}
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": "modern protocol failure", "data": map[string]any{"supported": []string{"2099-01-01"}}}})
			}))
			defer s.Close()
			_, err := ConnectEphemeral(t.Context(), ServerConfig{Name: "modern", URL: s.URL, Transport: TransportStreamableHTTP})
			require.Error(t, err)
			require.Zero(t, inits.Load())
		})
	}
}

func TestLegacySSETransportAndOmittedTransportStillWork(t *testing.T) {
	for _, kind := range []string{TransportSSE, ""} {
		t.Run("transport="+kind, func(t *testing.T) {
			core := mcpserver.NewMCPServer("legacy-sse", "1.0.0")
			core.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("legacy"), nil
			})
			sse := mcpserver.NewSSEServer(core)
			s := httptest.NewServer(sse)
			defer s.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			e, err := ConnectEphemeral(ctx, ServerConfig{Name: "sse", URL: s.URL + "/sse", Transport: kind})
			require.NoError(t, err)
			defer e.Close()
			cancel() // Connection lifetime must outlive the setup request.
			require.Equal(t, "2025-11-25", e.client.ProtocolVersion())
			result, err := e.CallTool(t.Context(), "echo", nil)
			require.NoError(t, err)
			require.Equal(t, "legacy", result.Content[0].(mcp.TextContent).Text)
		})
	}
}

func TestProtocolHeadersCannotBeOverriddenByStoredHeaders(t *testing.T) {
	var counts modernCounts
	s, _ := modernServer(t, &counts, false)
	e, err := ConnectEphemeral(t.Context(), ServerConfig{Name: "modern", URL: s.URL, Transport: TransportStreamableHTTP, Headers: map[string]string{"MCP-Protocol-Version": "2025-11-25", "Mcp-Method": "initialize", "Mcp-Name": "wrong", "Mcp-Session-Id": "obsolete", "Mcp-Param-Message": "wrong", "Content-Type": "text/plain"}})
	require.NoError(t, err)
	defer e.Close()
	_, err = e.CallTool(t.Context(), "echo", map[string]any{"message": "correct"})
	require.NoError(t, err)
	require.Zero(t, counts.initializations.Load())
}

func TestRawInputSchemaForWorkerPreservesKeywords(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","$defs":{"id":{"type":"string"}},"properties":{"id":{"$ref":"#/$defs/id"}},"additionalProperties":false}`)
	tool := DiscoveredTool{Tool: mcp.Tool{Name: "test", RawInputSchema: raw}}
	require.JSONEq(t, string(raw), string(tool.InputSchemaJSON()))
}

func TestModernToolCancellationStopsUpstreamWork(t *testing.T) {
	var counts modernCounts
	s, core := modernServer(t, &counts, false)
	stopped := make(chan struct{})
	core.AddTool(mcp.NewTool("wait"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	e, err := ConnectEphemeral(t.Context(), ServerConfig{Name: "modern", URL: s.URL, Transport: TransportStreamableHTTP})
	require.NoError(t, err)
	defer e.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = e.CallTool(ctx, "wait", nil)
	require.Error(t, err)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream work was not cancelled")
	}
	require.Zero(t, counts.initializations.Load())
}

func TestModernToolSubscriptionsOutliveConnectContext(t *testing.T) {
	var counts modernCounts
	s, core := modernServer(t, &counts, true)
	pool := NewPool()
	defer pool.Close()
	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, pool.ConnectDynamic(ctx, ServerConfig{Name: "modern", URL: s.URL, Transport: TransportStreamableHTTP}))
	cancel()
	var changes atomic.Int32
	pool.OnToolsChanged(func() { changes.Add(1) })
	// Subscribe is asynchronous: repeat an idempotent schema update until the
	// stream is acknowledged and delivery proves the listener is alive.
	require.Eventually(t, func() bool {
		core.AddTool(mcp.NewTool("new_tool", mcp.WithDescription("new schema")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("new"), nil
		})
		_, ok := pool.GetTool("modern", "new_tool")
		return ok && changes.Load() > 0
	}, 3*time.Second, 20*time.Millisecond)
	pool.Close()
	require.Zero(t, counts.deletes.Load())
}
