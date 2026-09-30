//go:build integration

package postgres_test

import (
	"strings"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
)

func TestConnect(t *testing.T) {
	pool, err := postgres.Connect(t.Context(), postgrestest.URL(t))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	var version string
	if err := pool.QueryRow(t.Context(), "SHOW server_version").Scan(&version); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.HasPrefix(version, "18.") {
		t.Fatalf("server_version = %q, want PostgreSQL 18", version)
	}
}

func TestConnectFailsFastOnAnUnreachableDatabase(t *testing.T) {
	// Port 1 on localhost refuses connections immediately.
	_, err := postgres.Connect(t.Context(), "postgres://jupiter:secret@127.0.0.1:1/jupiter?connect_timeout=2")
	if err == nil {
		t.Fatal("Connect succeeded against an unreachable database")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks the password: %v", err)
	}
}
