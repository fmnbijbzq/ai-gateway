package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ai-gateway/internal/admin"
	"ai-gateway/internal/balancer"
	"ai-gateway/internal/config"
	"ai-gateway/internal/health"
	"ai-gateway/internal/proxy"
	"ai-gateway/internal/upstream"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	logger.Info("config loaded",
		"listen", cfg.Server.Listen,
		"upstreams", len(cfg.Upstreams),
	)

	// Initialize upstreams
	upstreams := make([]*upstream.Upstream, 0, len(cfg.Upstreams))
	for _, ucfg := range cfg.Upstreams {
		u := upstream.New(ucfg, cfg.CircuitBreaker, cfg.HealthCheck)
		upstreams = append(upstreams, u)
		logger.Info("upstream registered",
			"name", ucfg.Name,
			"base_url", ucfg.BaseURL,
			"weight", ucfg.Weight,
			"models", ucfg.Models,
		)
	}

	// Initialize components
	bal := balancer.New(upstreams)
	checker := health.New(upstreams, cfg.HealthCheck.Interval, cfg.HealthCheck.Timeout, logger)
	prx := proxy.New(bal, cfg.Server.MaxRetries, cfg.Server.WriteTimeout, logger)
	adm := admin.New(upstreams, logger)

	// Auth middleware for /v1/* endpoints
	authMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if cfg.Server.APIKey != "" {
				key := r.Header.Get("X-Api-Key")
				if key == "" {
					if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
						key = strings.TrimPrefix(auth, "Bearer ")
					}
				}
				if key != cfg.Server.APIKey {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
					return
				}
			}
			next(w, r)
		}
	}

	// Set up routes
	mux := http.NewServeMux()

	// Health endpoint (no auth)
	mux.HandleFunc("/health", checker.Handler())

	// Admin endpoints (no auth)
	adm.Register(mux)

	// API proxy endpoints (with auth)
	mux.HandleFunc("/v1/models", authMiddleware(prx.ModelsHandler()))
	mux.HandleFunc("/v1/messages", authMiddleware(prx.MessagesHandler()))
	mux.HandleFunc("/v1/", authMiddleware(prx.Handler()))

	// Root handler
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"service":"ai-gateway","status":"running"}`))
			return
		}
		http.NotFound(w, r)
	})

	server := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      mux,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Start health checker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go checker.Start(ctx)

	// Start server
	go func() {
		logger.Info("server starting", "addr", cfg.Server.Listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("shutting down", "signal", sig.String())

	cancel() // stop health checker

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown error", "error", err)
	}
	logger.Info("server stopped")
}
