package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Bounds the first ping, so an unreachable database fails startup instead of hanging.
const connectTimeout = 10 * time.Second

// Timeouts bound what one statement, one wait for a lock and one transaction left idle
// may take, so that a slow query or a forgotten transaction cannot hold the pool's
// connections and the locks behind them. A setting the database URL names wins.
type Timeouts struct {
	Statement, Lock, IdleInTransaction time.Duration
}

// ServeTimeouts suit a process that answers requests; WorkTimeouts, one that runs the
// background's batches and reports.
var (
	ServeTimeouts = Timeouts{Statement: 15 * time.Second, Lock: 5 * time.Second, IdleInTransaction: time.Minute}
	WorkTimeouts  = Timeouts{Statement: 10 * time.Minute, Lock: 30 * time.Second, IdleInTransaction: 10 * time.Minute}
)

// Connect opens a pool. Its size is the URL's pool_max_conns, by default the larger of
// four and the number of CPUs.
func Connect(ctx context.Context, url string, timeouts ...Timeouts) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx masks the password in the error it returns.
		return nil, fmt.Errorf("parsing database URL: %w", err)
	}
	for _, t := range timeouts {
		params := cfg.ConnConfig.RuntimeParams
		for name, d := range map[string]time.Duration{
			"statement_timeout": t.Statement, "lock_timeout": t.Lock, "idle_in_transaction_session_timeout": t.IdleInTransaction,
		} {
			if _, set := params[name]; !set && d > 0 {
				params[name] = strconv.FormatInt(d.Milliseconds(), 10)
			}
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to %s:%d/%s: %w",
			cfg.ConnConfig.Host, cfg.ConnConfig.Port, cfg.ConnConfig.Database, err)
	}
	return pool, nil
}
