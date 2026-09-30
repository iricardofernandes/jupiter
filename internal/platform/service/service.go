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

type Config struct {
	Name     string
	HTTPAddr string
	LogLevel slog.Level
}

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

func Run(ctx context.Context, cfg Config, handler http.Handler, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.HTTPAddr, err)
	}
	return Serve(ctx, ln, handler, logger)
}

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
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	return mux
}
