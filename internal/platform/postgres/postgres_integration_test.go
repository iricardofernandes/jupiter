//go:build integration

package postgres_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
)

var server *postgrestest.Server

func TestMain(m *testing.M) { os.Exit(postgrestest.Main(m, &server)) }

func TestConnect(t *testing.T) {
	pool := server.Pool(t)
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

func TestMigrateIsIdempotentAndUsesTheModuleSchema(t *testing.T) {
	pool := server.Pool(t)
	migrations := fstest.MapFS{
		"00001_widgets.sql": {Data: []byte("-- +goose Up\nCREATE TABLE widgets.widgets (id int);\n-- +goose Down\nDROP TABLE widgets.widgets;\n")},
	}
	for range 2 {
		if err := postgres.Migrate(t.Context(), pool, "widgets", migrations); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	var version int64
	if err := pool.QueryRow(t.Context(), "SELECT max(version_id) FROM widgets.goose_db_version").Scan(&version); err != nil {
		t.Fatalf("reading version: %v", err)
	}
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}
}

func TestInTxRetriesADeadlock(t *testing.T) {
	pool := server.Pool(t)
	exec(t, pool, "CREATE TABLE counters (id int PRIMARY KEY, n int NOT NULL)", "INSERT INTO counters VALUES (1, 0), (2, 0)")

	// Two transactions lock the rows in opposite orders; PostgreSQL aborts one, and InTx
	// must run it again rather than surface the deadlock.
	ready := make(chan struct{}, 2)
	proceed := make(chan struct{})
	run := func(first, second int) error {
		attempt := 0
		return postgres.InTx(t.Context(), pool, func(tx pgx.Tx) error {
			attempt++
			if _, err := tx.Exec(t.Context(), "UPDATE counters SET n = n + 1 WHERE id = $1", first); err != nil {
				return err
			}
			if attempt == 1 {
				ready <- struct{}{}
				<-proceed
			}
			_, err := tx.Exec(t.Context(), "UPDATE counters SET n = n + 1 WHERE id = $1", second)
			return err
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- run(1, 2) }()
	go func() { errs <- run(2, 1) }()
	<-ready
	<-ready
	close(proceed)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("InTx: %v", err)
		}
	}
	var total int
	if err := pool.QueryRow(t.Context(), "SELECT sum(n) FROM counters").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("sum = %d, want 4: each transaction applied exactly once", total)
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, statements ...string) {
	t.Helper()
	for _, sql := range statements {
		if _, err := pool.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}
