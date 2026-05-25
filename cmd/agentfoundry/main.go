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

	"github.com/angoo/agentfoundry/internal/api"
	"github.com/angoo/agentfoundry/internal/auth"
	"github.com/angoo/agentfoundry/internal/config"
	"github.com/angoo/agentfoundry/internal/db"
	mcpserver "github.com/angoo/agentfoundry/internal/mcp"
	"github.com/angoo/agentfoundry/internal/mcpclient"
	"github.com/angoo/agentfoundry/internal/registry"
	"github.com/angoo/agentfoundry/internal/run"
	"github.com/angoo/agentfoundry/internal/session"
	"github.com/angoo/agentfoundry/internal/store"
	"github.com/angoo/agentfoundry/internal/stream"
	"github.com/angoo/agentfoundry/internal/temporal"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.LoadSystem("agentfoundry.yaml")
	if err != nil {
		slog.Warn("no agentfoundry.yaml found, using defaults", "error", err)
		cfg = config.DefaultSystem()
	}
	slog.Info("loaded system config",
		"listen", cfg.Listen,
		"temporal_host", cfg.Temporal.HostPort,
	)

	authCfg := auth.LoadConfig()
	if authCfg.InternalAPIKey == "" && cfg.InternalAPIKey != "" {
		authCfg.InternalAPIKey = cfg.InternalAPIKey
	}

	ctx := context.Background()

	dbURL := os.Getenv("AUTH_DB_URL")
	if dbURL == "" {
		slog.Error("AUTH_DB_URL is required (used for agent definitions and auth storage)")
		os.Exit(1)
	}

	dbPool, err := db.NewPool(ctx, dbURL)
	if err != nil {
		slog.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer dbPool.Close()

	if err := dbPool.Migrate(ctx); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	var (
		jwt      *auth.JWTValidator
		groups   *auth.GroupCache
		keyStore *auth.APIKeyStore
		mcpStore *auth.MCPServerStore
		authMW   *auth.Middleware
	)

	if authCfg.Enabled() {
		if authCfg.KeycloakAdmin.ClientID != "" && authCfg.KeycloakAdmin.ClientSecret != "" {
			groups = auth.NewGroupCache(
				authCfg.Issuer,
				authCfg.KeycloakURL,
				authCfg.KeycloakRealm,
				authCfg.KeycloakAdmin.ClientID,
				authCfg.KeycloakAdmin.ClientSecret,
			)
			slog.Info("keycloak group cache initialized")
		} else {
			slog.Warn("keycloak admin credentials not configured, API key auth will not resolve user groups")
		}

		jwt, err = auth.NewJWTValidator(ctx, authCfg)
		if err != nil {
			slog.Error("failed to initialize JWT validator", "error", err)
			os.Exit(1)
		}
		slog.Info("JWT validator initialized", "issuer", authCfg.Issuer)

		keyStore = auth.NewAPIKeyStore(dbPool.Pool)
		mcpStore = auth.NewMCPServerStore(dbPool.Pool)

		authMW = auth.NewMiddleware(jwt, keyStore, groups, authCfg)
		slog.Info("auth middleware enabled")
	} else {
		slog.Info("auth disabled (AUTH_ISSUER not set), running in open access mode")
		authMW = auth.NewMiddleware(nil, nil, nil, authCfg)
	}

	reg := registry.New()

	dbStore := store.NewDBStore(dbPool.Pool, reg)
	if err := dbStore.LoadAll(ctx); err != nil {
		slog.Error("failed to load agent definitions from database", "error", err)
		os.Exit(1)
	}
	definitionStore := dbStore

	pool := mcpclient.NewPool()

	if mcpStore != nil {
		dynamicServers, err := mcpStore.ListAll(ctx)
		if err != nil {
			slog.Error("failed to load dynamic MCP servers from database", "error", err)
		} else {
			for _, srv := range dynamicServers {
				cfg := mcpclient.ServerConfig{
					Name:      srv.Name,
					URL:       srv.URL,
					Transport: srv.Transport,
					Headers:   srv.Headers,
				}
				if err := pool.ConnectDynamic(ctx, cfg); err != nil {
					slog.Error("failed to reconnect dynamic MCP server", "name", srv.Name, "error", err)
				}
			}
		}
	}

	temporalClient, err := temporal.NewClient(cfg.Temporal.HostPort, cfg.Temporal.Namespace, cfg.Temporal.APIKey)
	if err != nil {
		slog.Error("failed to connect to temporal server", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	mcpManager := mcpserver.NewManager(reg, pool, temporalClient)
	mcpManager.RegisterRoutes(mux)

	pool.OnToolsChanged(func() {
		mcpManager.RefreshAll()
	})

	streams := stream.NewManager()
	sessions := session.New()
	runs := run.New()

	apiHandler := api.NewHandler(reg, pool, definitionStore, temporalClient, streams, sessions, keyStore, mcpStore, runs, &cfg.LLM)
	apiHandler.RegisterRoutes(mux)

	var handler http.Handler = mux
	if authMW != nil {
		handler = authMW.Handler("/health", "/servers/")(mux)
	}

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("agentfoundry daemon starting", "addr", cfg.Listen)
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
	slog.Info("agentfoundry stopped")
}
