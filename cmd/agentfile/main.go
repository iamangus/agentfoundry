package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/angoo/agentfile/internal/agent"
	"github.com/angoo/agentfile/internal/api"
	"github.com/angoo/agentfile/internal/config"
	"github.com/angoo/agentfile/internal/llm"
	mcpserver "github.com/angoo/agentfile/internal/mcp"
	"github.com/angoo/agentfile/internal/mcpclient"
	"github.com/angoo/agentfile/internal/registry"
	"github.com/angoo/agentfile/internal/web"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Load system config
	cfg, err := config.LoadSystem("agentfile.yaml")
	if err != nil {
		slog.Warn("no agentfile.yaml found, using defaults", "error", err)
		cfg = config.DefaultSystem()
	}
	slog.Info("loaded system config",
		"listen", cfg.Listen,
		"definitions_dir", cfg.DefinitionsDir,
		"mcp_servers", len(cfg.MCPServers),
	)

	// Create registry (stores agent definitions)
	reg := registry.New()

	// Create MCP client pool (connects to external MCP servers, discovers tools)
	pool := mcpclient.NewPool()

	// Connect to configured external MCP servers
	ctx := context.Background()
	if len(cfg.MCPServers) > 0 {
		if err := pool.Connect(ctx, cfg.MCPServers); err != nil {
			slog.Error("failed to connect to MCP servers", "error", err)
		}
	} else {
		slog.Info("no external MCP servers configured")
	}

	// Create LLM client (works with any OpenAI-compatible API)
	llmClient := llm.NewClient(llm.ClientConfig{
		BaseURL:      cfg.LLM.BaseURL,
		APIKey:       cfg.LLM.APIKey,
		DefaultModel: cfg.LLM.DefaultModel,
		Headers:      cfg.LLM.Headers,
	})

	// Create agent runtime
	agentRuntime := agent.NewRuntime(reg, pool, llmClient)

	// Load agent definitions from filesystem
	loader := config.NewLoader(cfg.DefinitionsDir, reg)
	if err := loader.LoadAll(); err != nil {
		slog.Error("failed to load definitions", "error", err)
		os.Exit(1)
	}

	// Start filesystem watcher for hot-reloading agent definitions
	if err := loader.Watch(); err != nil {
		slog.Warn("failed to start filesystem watcher", "error", err)
	}
	defer loader.Close()

	// Set up HTTP mux
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	// MCP server manager — scoped MCP SSE servers per agent + default
	mcpManager := mcpserver.NewManager(reg, pool, agentRuntime)
	mcpManager.RegisterRoutes(mux)

	// When external MCP server tools change, invalidate cached MCP servers
	pool.OnToolsChanged(func() {
		mcpManager.RefreshAll()
	})

	// REST API for agents and status
	apiHandler := api.NewHandler(reg, pool, loader, agentRuntime)
	apiHandler.RegisterRoutes(mux)

	// Web UI (chat + agents pages)
	webHandler, err := web.NewHandler(loader, agentRuntime, pool)
	if err != nil {
		slog.Error("failed to create web UI handler", "error", err)
		os.Exit(1)
	}
	webHandler.RegisterRoutes(mux)

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
	}

	// Graceful shutdown
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("agentfile daemon starting", "addr", cfg.Listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-sigCtx.Done()
	slog.Info("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mcpManager.Shutdown(shutdownCtx)
	pool.Close()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	slog.Info("agentfile stopped")
}
