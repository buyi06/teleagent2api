package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"teleagent2api/internal/config"
	"teleagent2api/internal/handler"
	"teleagent2api/internal/middleware"
	"teleagent2api/internal/proxy"
)

func main() {
	cfg := config.Load()

	initLogger(cfg)

	if len(cfg.Credentials) == 0 {
		slog.Error("missing TeleAgent credentials", slog.String("missing", "TELEAGENT_TOKEN/TELEAGENT_INSTALL_ID/TELEAGENT_DEVICE_ID or config.credentials[]"))
		os.Exit(1)
	}

	if cfg.APIKey == "" {
		slog.Warn("API_KEY is empty — all requests will be accepted without authentication")
	}

	slog.Info("configuration loaded", slog.String("summary", cfg.SafeSummary()))

	upProxy, err := proxy.NewUpstreamProxy(cfg)
	if err != nil {
		slog.Error("failed to initialize upstream proxy", slog.String("error", err.Error()))
		os.Exit(1)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cfg.Timeout
	httpClient := &http.Client{
		Transport: transport,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handler.Health())

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/v1/models", handler.Models(cfg))
	apiMux.HandleFunc("/v1/chat/completions", handler.ChatCompletions(upProxy, httpClient, cfg))

	// Mount api endpoints under auth middleware
	mux.Handle("/v1/", middleware.Auth(cfg.APIKey)(apiMux))

	// Legacy endpoints
	mux.Handle("/models", middleware.Auth(cfg.APIKey)(http.HandlerFunc(handler.Models(cfg))))
	mux.Handle("/chat/completions", middleware.Auth(cfg.APIKey)(http.HandlerFunc(handler.ChatCompletions(upProxy, httpClient, cfg))))

	// Stack: RequestID → MaxBodySize → AccessLog → mux
	var h http.Handler = mux
	h = middleware.AccessLog(h)
	h = middleware.MaxBodySize(10 << 20)(h) // 10 MB
	h = middleware.RequestID(h)

	server := &http.Server{
		Addr:        cfg.Listen,
		Handler:     h,
		ReadTimeout: cfg.Timeout / 4,
		// Do not use a total WriteTimeout for streaming completions. Long
		// healthy streams are guarded by upstream header timeout and per-chunk
		// idle timeout instead.
		WriteTimeout: 0,
		IdleTimeout:  cfg.Timeout,
	}

	slog.Info("starting server",
		slog.String("listen", cfg.Listen),
		slog.String("upstream", cfg.BaseURL),
	)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down gracefully...")
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout/4)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", slog.String("error", err.Error()))
		}
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("server error", slog.String("error", err.Error()))
		os.Exit(1)
	}
	slog.Info("server stopped")
}

func initLogger(cfg config.Config) {
	opts := &slog.HandlerOptions{Level: cfg.SlogLevel()}

	var handler slog.Handler
	switch strings.ToLower(cfg.LogFormat) {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	default:
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}
