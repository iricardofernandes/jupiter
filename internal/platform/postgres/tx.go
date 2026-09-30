package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxTxAttempts = 3

// InTx runs fn in a read-committed transaction and commits it. A transaction that
// PostgreSQL aborts to break a deadlock or a serialization conflict is retried from the
// start, so fn must not have effects outside the database.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return InTxWith(ctx, pool, pgx.TxOptions{}, fn)
}

// InTxWith is InTx with explicit transaction options.
func InTxWith(ctx context.Context, pool *pgxpool.Pool, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	var err error
	for range maxTxAttempts {
		err = pgx.BeginTxFunc(ctx, pool, opts, fn)
		if !IsRetryable(err) {
			return err
		}
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxTxAttempts, err)
}

// IsRetryable reports whether err is a deadlock or serialization failure.
func IsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}

// ErrorCode returns the SQLSTATE of err, or "" if it is not a PostgreSQL error.
func ErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ConstraintName returns the constraint a PostgreSQL error names, or "".
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
