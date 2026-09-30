// Package postgrestest starts a disposable PostgreSQL for integration tests.
package postgrestest

import (
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Image is the PostgreSQL image tests run against. Keep it in step with compose.yaml.
const Image = "postgres:18.6-alpine"

// URL starts a PostgreSQL container that is removed when the test ends, and returns a
// connection URL for it.
func URL(t testing.TB) string {
	t.Helper()
	ctx := t.Context()
	ctr, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("jupiter"),
		tcpostgres.WithUsername("jupiter"),
		tcpostgres.WithPassword("jupiter"),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("starting %s: %v", Image, err)
	}
	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return url
}
