package postgrestest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// Keep in step with compose.yaml.
const Image = "postgres:18.6-alpine"

const templateName = "jupiter_template"

// Server is one PostgreSQL container shared by a test package. Each test gets its own
// database cloned from a migrated template, because ledger tables refuse the DELETE
// and TRUNCATE that sharing one database would need.
type Server struct {
	container *tcpostgres.PostgresContainer
	adminURL  string
	databases atomic.Int64
}

type MigrateFunc func(context.Context, *pgxpool.Pool) error

// Main starts a server, migrates its template, runs the package's tests and stops it.
// Call it from TestMain: os.Exit(postgrestest.Main(m, &server, ledger.Migrate)).
func Main(m *testing.M, server **Server, migrations ...MigrateFunc) int {
	ctx := context.Background()
	s, err := start(ctx, migrations)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgrestest:", err)
		return 1
	}
	*server = s
	defer func() { _ = testcontainers.TerminateContainer(s.container) }()
	return m.Run()
}

func start(ctx context.Context, migrations []MigrateFunc) (*Server, error) {
	ctr, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("jupiter"),
		tcpostgres.WithPassword("jupiter"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if ctr != nil {
			_ = testcontainers.TerminateContainer(ctr)
		}
		return nil, fmt.Errorf("starting %s: %w", Image, err)
	}
	adminURL, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return nil, fmt.Errorf("connection string: %w", err)
	}
	s := &Server{container: ctr, adminURL: adminURL}
	if err := s.prepareTemplate(ctx, migrations); err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return nil, err
	}
	return s, nil
}

func (s *Server) prepareTemplate(ctx context.Context, migrations []MigrateFunc) error {
	if err := s.admin(ctx, "CREATE DATABASE "+templateName); err != nil {
		return err
	}
	pool, err := postgres.Connect(ctx, s.urlFor(templateName))
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, migrate := range migrations {
		if err := migrate(ctx, pool); err != nil {
			return fmt.Errorf("migrating template: %w", err)
		}
	}
	return nil
}

// URL returns the connection URL of a new, migrated database for t.
func (s *Server) URL(t testing.TB) string {
	t.Helper()
	url, _, err := s.Database(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return url
}

// Database creates a new, migrated database and returns its URL and a function that
// drops it, for callers such as property tests that need a database per iteration.
func (s *Server) Database(ctx context.Context) (string, func(context.Context) error, error) {
	name := fmt.Sprintf("test_%d", s.databases.Add(1))
	if err := s.cloneTemplate(ctx, name); err != nil {
		return "", nil, err
	}
	drop := func(ctx context.Context) error {
		return s.admin(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	}
	return s.urlFor(name), drop, nil
}

// cloneTemplate retries while the template's last sessions are still closing, which
// PostgreSQL reports as "source database is being accessed by other users".
func (s *Server) cloneTemplate(ctx context.Context, name string) error {
	const attempts = 20
	var err error
	for range attempts {
		err = s.admin(ctx, "CREATE DATABASE "+name+" TEMPLATE "+templateName)
		if postgres.ErrorCode(err) != "55006" {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// Pool returns a pool on a new, migrated database for t, closed when t ends.
func (s *Server) Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.Connect(t.Context(), s.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func (s *Server) admin(ctx context.Context, sql string) error {
	conn, err := pgx.Connect(ctx, s.adminURL)
	if err != nil {
		return fmt.Errorf("connecting as admin: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%s: %w", sql, err)
	}
	return nil
}

func (s *Server) urlFor(database string) string {
	u, err := url.Parse(s.adminURL)
	if err != nil {
		panic(fmt.Sprintf("postgrestest: invalid admin URL: %v", err))
	}
	u.Path = "/" + database
	return u.String()
}
