package postgres

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

// Migrate applies a module's migrations to its own schema. Each module keeps its own
// version table inside that schema, so modules migrate independently of one another.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schema string, migrations fs.FS) error {
	if err := createSchema(ctx, pool, schema); err != nil {
		return err
	}
	store, err := database.NewStore(goose.DialectPostgres, schema+".goose_db_version")
	if err != nil {
		return fmt.Errorf("goose store: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("goose locker: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider("", db, migrations,
		goose.WithStore(store),
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("goose provider for %s: %w", schema, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrating %s: %w", schema, err)
	}
	return nil
}

// createSchema runs before goose takes its lock, so two processes can race on it; IF NOT
// EXISTS does not stop the loser from hitting pg_namespace's unique index.
func createSchema(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	sql := "CREATE SCHEMA IF NOT EXISTS " + pgx.Identifier{schema}.Sanitize()
	_, err := pool.Exec(ctx, sql)
	if ErrorCode(err) == "23505" {
		_, err = pool.Exec(ctx, sql)
	}
	if err != nil {
		return fmt.Errorf("creating schema %s: %w", schema, err)
	}
	return nil
}
