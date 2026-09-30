package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/platform/service"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, err := service.ConfigFromEnv("api", ":8080", env(nil))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Name != "api" || cfg.HTTPAddr != ":8080" || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("got %+v, want api on :8080 at info", cfg)
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	cfg, err := service.ConfigFromEnv("api", ":8080", env(map[string]string{
		"JUPITER_HTTP_ADDR": "127.0.0.1:9000",
		"JUPITER_LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9000" || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("got %+v", cfg)
	}
}

func TestConfigFromEnvRejectsInvalidValues(t *testing.T) {
	tests := []map[string]string{
		{"JUPITER_LOG_LEVEL": "loud"},
		{"JUPITER_HTTP_ADDR": "no-port"},
	}
	for _, values := range tests {
		if _, err := service.ConfigFromEnv("api", ":8080", env(values)); err == nil {
			t.Errorf("ConfigFromEnv(%v) succeeded, want an error", values)
		}
	}
}

func TestServeAnswersHealthChecksAndShutsDown(t *testing.T) {
	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- service.Serve(ctx, ln, nil, slog.New(slog.DiscardHandler)) }()

	base := "http://" + ln.Addr().String()
	for _, path := range []string{"/healthz", "/readyz"} {
		status, body := get(t, base+path)
		if status != http.StatusOK || strings.TrimSpace(body) != "ok" {
			t.Errorf("GET %s = %d %q, want 200 ok", path, status, body)
		}
	}
	if status, _ := get(t, base+"/nope"); status != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", status)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

func TestServeRoutesToTheHandlerBesideHealthChecks(t *testing.T) {
	ln := listen(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("api"))
	})
	go func() { _ = service.Serve(t.Context(), ln, handler, slog.New(slog.DiscardHandler)) }()

	base := "http://" + ln.Addr().String()
	if status, body := get(t, base+"/v1/anything"); status != http.StatusOK || body != "api" {
		t.Errorf("GET /v1/anything = %d %q, want the handler's response", status, body)
	}
	if status, body := get(t, base+"/healthz"); status != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Errorf("GET /healthz = %d %q, want the health check", status, body)
	}
}

func TestShutdownDrainsInFlightRequests(t *testing.T) {
	ln := listen(t)
	entered, release := make(chan struct{}), make(chan struct{})
	contextErr := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		contextErr <- r.Context().Err()
		_, _ = w.Write([]byte("finished"))
	})
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- service.Serve(ctx, ln, handler, slog.New(slog.DiscardHandler)) }()

	type response struct {
		status int
		body   string
		err    error
	}
	responses := make(chan response, 1)
	go func() {
		status, body, err := fetch(t.Context(), "http://"+ln.Addr().String()+"/slow")
		responses <- response{status, body, err}
	}()

	<-entered
	cancel()
	time.Sleep(100 * time.Millisecond)
	close(release)

	if err := <-contextErr; err != nil {
		t.Errorf("request context was cancelled by the shutdown signal: %v", err)
	}
	if r := <-responses; r.err != nil || r.status != http.StatusOK || r.body != "finished" {
		t.Errorf("in-flight request got %d %q, %v; want 200 finished", r.status, r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
}

func TestServeReportsListenerFailure(t *testing.T) {
	ln := listen(t)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	err := service.Serve(t.Context(), ln, nil, slog.New(slog.DiscardHandler))
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve on a closed listener = %v, want an error", err)
	}
}

func TestRunStopsCleanlyWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg := service.Config{Name: "test", HTTPAddr: "127.0.0.1:0", LogLevel: slog.LevelInfo}
	if err := service.Run(ctx, cfg, nil, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("Run = %v, want nil after cancellation", err)
	}
}

func TestRunFailsWhenTheAddressIsTaken(t *testing.T) {
	taken := listen(t)
	defer func() { _ = taken.Close() }()
	cfg := service.Config{Name: "test", HTTPAddr: taken.Addr().String(), LogLevel: slog.LevelInfo}
	if err := service.Run(t.Context(), cfg, nil, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("Run succeeded on an address already in use")
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	status, body, err := fetch(t.Context(), url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return status, body
}

// fetch is get for goroutines other than the test's, which must not call t.Fatal.
func fetch(ctx context.Context, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}
