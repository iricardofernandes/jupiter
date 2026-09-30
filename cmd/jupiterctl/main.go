package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/vault"
)

const usage = `usage: jupiterctl <command>

commands:
  migrate                  apply every module's database migrations
  merchant create <name>   create a merchant and print its API keys, which are shown only once
  ledger check             verify ledger invariants; exits 1 if any is violated
  ledger repair <account>  reset a drifted account's cached balance from its entries
  dev-certs <dir>          write a development CA and the vault's, API's and worker's mTLS certificates

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
	if len(args) == 2 && args[0] == "dev-certs" {
		return devCerts(args[1])
	}
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
		return migrate(ctx, pool)
	case len(args) == 3 && args[0] == "merchant" && args[1] == "create":
		return createMerchant(ctx, pool, args[2])
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

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	for _, m := range []func(context.Context, *pgxpool.Pool) error{
		ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, acquirer.Migrate,
	} {
		if err := m(ctx, pool); err != nil {
			return err
		}
	}
	return nil
}

func createMerchant(ctx context.Context, pool *pgxpool.Pool, name string) error {
	var m merchant.Merchant
	var keys []merchant.IssuedKey
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		m, keys, err = merchant.New(nil).Create(ctx, tx, name, api.CurrentVersion)
		return err
	})
	if err != nil {
		return err
	}
	out := map[string]any{"merchant": m.ID.String(), "name": m.Name, "api_version": m.APIVersion}
	for _, k := range keys {
		out[k.Value[:7]] = k.Value
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

// devCerts writes a CA, a server certificate for the vault valid on localhost and
// "vault", and the client certificates the API and the worker present.
func devCerts(dir string) error {
	pki, err := mtls.NewPKI("Jupiter development CA")
	if err != nil {
		return err
	}
	server, err := pki.Server("localhost", "vault", "127.0.0.1")
	if err != nil {
		return err
	}
	client, err := pki.Client(vault.APIIdentity)
	if err != nil {
		return err
	}
	worker, err := pki.Client(vault.WorkerIdentity)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the operator names the directory
		return err
	}
	for name, data := range map[string][]byte{
		"ca.pem": pki.CAPEM(), "vault.pem": server.CertPEM, "vault-key.pem": server.KeyPEM,
		"api.pem": client.CertPEM, "api-key.pem": client.KeyPEM,
		"worker.pem": worker.CertPEM, "worker-key.pem": worker.KeyPEM,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil { //nolint:gosec // the operator names the directory
			return err
		}
	}
	fmt.Println("wrote ca.pem and the key pairs vault, api and worker to", dir)
	return nil
}
