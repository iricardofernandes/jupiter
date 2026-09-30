// Package service runs a Jupiter process: it reads configuration from the environment,
// logs structured JSON, serves health endpoints and shuts down gracefully on SIGINT or
// SIGTERM. Every binary under cmd/ starts through Main.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	envHTTPAddr = "JUPITER_HTTP_ADDR"
	envLogLevel = "JUPITER_LOG_LEVEL"

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Config is what every process needs before it can do anything else.
type Config struct {
	Name     string
	HTTPAddr string
	LogLevel slog.Level
}

// ConfigFromEnv reads the configuration for the process called name, falling back to
// defaultAddr when JUPITER_HTTP_ADDR is unset. Invalid values are errors, so a
// misconfigured process fails at startup rather than running with a guess.
func ConfigFromEnv(name, defaultAddr string, getenv func(string) string) (Config, error) {
	cfg := Config{Name: name, HTTPAddr: defaultAddr, LogLevel: slog.LevelInfo}
	if addr := getenv(envHTTPAddr); addr != "" {
		cfg.HTTPAddr = addr
	}
	if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
		return Config{}, fmt.Errorf("%s=%q: %w", envHTTPAddr, cfg.HTTPAddr, err)
	}
	if level := getenv(envLogLevel); level != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return Config{}, fmt.Errorf("%s=%q: %w", envLogLevel, level, err)
		}
	}
	return cfg, nil
}

// Main runs the process called name until it receives SIGINT or SIGTERM, and exits with
// a non-zero status if it fails.
func Main(name, defaultAddr string) {
	cfg, err := ConfigFromEnv(name, defaultAddr, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: invalid configuration: %v\n", name, err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", cfg.Name)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := Run(ctx, cfg, nil, logger); err != nil {
		logger.ErrorContext(ctx, "service stopped with an error", "error", err)
		stop()
		os.Exit(1) //nolint:gocritic // stop is called explicitly above
	}
}

// Run listens on cfg.HTTPAddr and serves handler until ctx is done.
func Run(ctx context.Context, cfg Config, handler http.Handler, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.HTTPAddr, err)
	}
	return Serve(ctx, ln, handler, logger)
}

// Serve answers health checks, and passes every other request to handler (which may be
// nil), on ln until ctx is done. It then stops accepting connections and drains
// in-flight requests for up to the shutdown timeout, returning nil after a clean
// shutdown.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Handler:           routes(handler),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		// Requests inherit ctx's values but not its cancellation: the shutdown signal
		// stops new connections, and Shutdown then lets in-flight requests finish.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	served := make(chan error, 1)
	go func() { served <- server.Serve(ln) }()
	logger.InfoContext(ctx, "serving", "addr", ln.Addr().String())

	select {
	case err := <-served:
		return fmt.Errorf("serving on %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}

	logger.InfoContext(ctx, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving on %s: %w", ln.Addr(), err)
	}
	return nil
}

func routes(handler http.Handler) http.Handler {
	mux := http.NewServeMux()
	if handler != nil {
		mux.Handle("/", handler)
	}
	ok := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
	// Liveness: the process is running. Readiness: it can take traffic. They answer
	// the same until the process has dependencies to check.
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	return mux
}
