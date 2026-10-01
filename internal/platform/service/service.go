package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

const (
	envHTTPAddr    = "JUPITER_HTTP_ADDR"
	envLogLevel    = "JUPITER_LOG_LEVEL"
	envDatabaseURL = "JUPITER_DATABASE_URL"

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 10 * time.Second
	readyTimeout      = 2 * time.Second
)

type Config struct {
	Name        string
	HTTPAddr    string
	LogLevel    slog.Level
	DatabaseURL string
}

type App struct {
	Handler http.Handler
	// TLS, if set, serves HTTPS with this configuration, health checks included.
	TLS        *tls.Config
	Ready      func(context.Context) error
	Background []func(context.Context) error
	Close      func()
}

type Build func(ctx context.Context, cfg Config, logger *slog.Logger) (App, error)

func ConfigFromEnv(name, defaultAddr string, getenv func(string) string) (Config, error) {
	cfg := Config{Name: name, HTTPAddr: defaultAddr, LogLevel: slog.LevelInfo, DatabaseURL: getenv(envDatabaseURL)}
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

func Main(name, defaultAddr string, build Build) {
	cfg, err := ConfigFromEnv(name, defaultAddr, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: invalid configuration: %v\n", name, err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", cfg.Name)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, build, logger); err != nil {
		logger.ErrorContext(ctx, "service stopped with an error", "error", err)
		stop()
		os.Exit(1) //nolint:gocritic // stop is called explicitly above
	}
}

func run(ctx context.Context, cfg Config, build Build, logger *slog.Logger) error {
	shutdown, err := telemetry.Start(ctx, cfg.Name, os.Getenv)
	if err != nil {
		return fmt.Errorf("starting telemetry: %w", err)
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	app, err := build(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("starting %s: %w", cfg.Name, err)
	}
	if app.Close != nil {
		defer app.Close()
	}
	return Run(ctx, cfg, app, logger)
}

func Run(ctx context.Context, cfg Config, app App, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.HTTPAddr, err)
	}
	return Serve(ctx, ln, app, logger)
}

// Serve runs the app's background tasks beside the HTTP server. The first task to fail
// stops the whole process, so a broken worker restarts instead of limping on.
func Serve(ctx context.Context, ln net.Listener, app App, logger *slog.Logger) error {
	if app.TLS != nil {
		ln = tls.NewListener(ln, app.TLS)
	}
	group, ctx := errgroup.WithContext(ctx)
	for _, task := range app.Background {
		group.Go(func() error { return task(ctx) })
	}
	group.Go(func() error { return serveHTTP(ctx, ln, app, logger) })
	return group.Wait()
}

func serveHTTP(ctx context.Context, ln net.Listener, app App, logger *slog.Logger) error {
	server := &http.Server{
		Handler:           routes(app),
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

func routes(app App) http.Handler {
	mux := http.NewServeMux()
	if app.Handler != nil {
		mux.Handle("/", app.Handler)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if app.Ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
			defer cancel()
			if err := app.Ready(ctx); err != nil {
				writeText(w, http.StatusServiceUnavailable, "not ready")
				return
			}
		}
		writeText(w, http.StatusOK, "ok")
	})
	return mux
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}

// Every returns a background task that runs fn every interval until ctx is done. A
// failed run is logged and retried on the next tick rather than stopping the process.
func Every(logger *slog.Logger, name string, interval time.Duration, fn func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		state := tasks.add(name, interval)
		for {
			began := time.Now()
			err := fn(ctx)
			state.ran(ctx, began, err)
			if err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "background task failed", "task", name, "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}
}
