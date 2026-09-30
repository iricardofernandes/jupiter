package postgrestest

import (
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Keep in step with compose.yaml.
const Image = "postgres:18.6-alpine"

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
