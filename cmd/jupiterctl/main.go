package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

const usage = `usage: jupiterctl <command>

commands:
  migrate                  apply every module's database migrations
  ledger check             verify ledger invariants; exits 1 if any is violated
  ledger repair <account>  reset a drifted account's cached balance from its entries

JUPITER_DATABASE_URL selects the database.`

var errViolations = errors.New("ledger invariants violated")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "jupiterctl:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(ctx context.Context, args []string) error {
	url := os.Getenv("JUPITER_DATABASE_URL")
	if url == "" || len(args) == 0 {
		return errUsage
	}
	pool, err := postgres.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch {
	case len(args) == 1 && args[0] == "migrate":
		return ledger.Migrate(ctx, pool)
	case len(args) == 2 && args[0] == "ledger" && args[1] == "check":
		return check(ctx, pool)
	case len(args) == 3 && args[0] == "ledger" && args[1] == "repair":
		account, err := ledger.AccountPrefix.Parse(args[2])
		if err != nil {
			return err
		}
		return ledger.New().Repair(ctx, pool, account)
	default:
		return errUsage
	}
}

func check(ctx context.Context, pool *pgxpool.Pool) error {
	report, err := ledger.New().Check(ctx, pool, ledger.CheckOptions{
		ClearingGrace: 24 * time.Hour,
		ExpiryGrace:   5 * time.Minute,
		MarkDrift:     true,
	})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if len(report.Violations) > 0 {
		return errViolations
	}
	return nil
}
