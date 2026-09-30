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

// DefaultTemplate names the template Main migrates; Templates adds others, such as the
// vault's, which lives in a database of its own.
const DefaultTemplate = "jupiter"

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
	return Templates(m, server, map[string][]MigrateFunc{DefaultTemplate: migrations})
}

// Templates is Main for tests that need databases of more than one kind: each named
// template is migrated with its own migrations.
func Templates(m *testing.M, server **Server, templates map[string][]MigrateFunc) int {
	ctx := context.Background()
	s, err := start(ctx, templates)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgrestest:", err)
		return 1
	}
	*server = s
	defer func() { _ = testcontainers.TerminateContainer(s.container) }()
	return m.Run()
}

func start(ctx context.Context, templates map[string][]MigrateFunc) (*Server, error) {
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
	for name, migrations := range templates {
		if err := s.prepareTemplate(ctx, templateDatabase(name), migrations); err != nil {
			_ = testcontainers.TerminateContainer(ctr)
			return nil, err
		}
	}
	return s, nil
}

func templateDatabase(name string) string {
	return name + "_template"
}

func (s *Server) prepareTemplate(ctx context.Context, database string, migrations []MigrateFunc) error {
	if err := s.admin(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		return err
	}
	pool, err := postgres.Connect(ctx, s.urlFor(database))
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
	return s.URLFrom(t, DefaultTemplate)
}

func (s *Server) URLFrom(t testing.TB, template string) string {
	t.Helper()
	url, _, err := s.DatabaseFrom(t.Context(), template)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

// Database creates a new, migrated database and returns its URL and a function that
// drops it, for callers such as property tests that need a database per iteration.
func (s *Server) Database(ctx context.Context) (string, func(context.Context) error, error) {
	return s.DatabaseFrom(ctx, DefaultTemplate)
}

func (s *Server) DatabaseFrom(ctx context.Context, template string) (string, func(context.Context) error, error) {
	name := fmt.Sprintf("test_%d", s.databases.Add(1))
	if err := s.cloneTemplate(ctx, name, templateDatabase(template)); err != nil {
		return "", nil, err
	}
	drop := func(ctx context.Context) error {
		return s.admin(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	}
	return s.urlFor(name), drop, nil
}

// cloneTemplate retries while the template's last sessions are still closing, which
// PostgreSQL reports as "source database is being accessed by other users".
func (s *Server) cloneTemplate(ctx context.Context, name, template string) error {
	const attempts = 20
	var err error
	for range attempts {
		err = s.admin(ctx, "CREATE DATABASE "+name+" TEMPLATE "+pgx.Identifier{template}.Sanitize())
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
	return s.PoolFrom(t, DefaultTemplate)
}

func (s *Server) PoolFrom(t testing.TB, template string) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.Connect(t.Context(), s.URLFrom(t, template))
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
