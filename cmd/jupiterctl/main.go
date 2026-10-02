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
	"github.com/iricardofernandes/jupiter/internal/authentication"
	"github.com/iricardofernandes/jupiter/internal/bank"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/internal/risk"
	"github.com/iricardofernandes/jupiter/internal/subscriptions"
	"github.com/iricardofernandes/jupiter/internal/vault"
)

const usage = `usage: jupiterctl <command>

commands:
  migrate                  apply every module's database migrations
  merchant create <name>   create a merchant and print its API keys, which are shown only once
  merchant set-tax-id <id> <cpf or cnpj>
                           record who the merchant's receivables belong to
  recipient verify <id> verified|rejected
                           record the outcome of a recipient's verification (KYC/KYB)
  recipient hold-payouts <id> | release-payouts <id>
                           hold a recipient's payouts, or lift the hold and send those held
  dispute decide <id> agreed|disagreed <operator>
                           decide a MED claim the merchant answered: return the money, or not
  reconcile report <YYYY-MM-DD> [live]
                           a day's reconciliation with every counterparty, in test or live mode
  reconcile resolve <break> <operator> <note>
                           close a break by hand, saying why
  reconcile confirm <break> <operator>
                           accept a probable match
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
	case len(args) == 4 && args[0] == "merchant" && args[1] == "set-tax-id":
		merchantID, err := merchant.MerchantPrefix.Parse(args[2])
		if err != nil {
			return err
		}
		return merchant.New(nil).SetTaxID(ctx, pool, merchantID, args[3])
	case args[0] == "recipient" || args[0] == "dispute" || args[0] == "reconcile":
		return operate(ctx, pool, args)
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
		ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, risk.Migrate, acquirer.Migrate, authentication.Migrate,
		subscriptions.Migrate, recipients.Migrate, receivables.Migrate, bank.Migrate, disputes.Migrate, reconciliation.Migrate,
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

// devCerts writes a CA and, from it: a server certificate for the vault valid on
// localhost and "vault", and the client certificates the API and the worker present to
// it; the Pix simulator's server certificate and the one it signs notifications with, and
// Jupiter's client certificate at the bank and server certificate for notifications.
func devCerts(dir string) error {
	pki, err := mtls.NewPKI("Jupiter development CA")
	if err != nil {
		return err
	}
	pairs := map[string]func() (mtls.Issued, error){
		"vault":        func() (mtls.Issued, error) { return pki.Server("localhost", "vault", "127.0.0.1") },
		"api":          func() (mtls.Issued, error) { return pki.Client(vault.APIIdentity) },
		"worker":       func() (mtls.Issued, error) { return pki.Client(vault.WorkerIdentity) },
		"sim-pix":      func() (mtls.Issued, error) { return pki.Server("localhost", "sim-pix", "127.0.0.1") },
		"sim-pix-hook": func() (mtls.Issued, error) { return pki.Client(pixBankIdentity) },
		"pix-client":   func() (mtls.Issued, error) { return pki.Client("spiffe://jupiter/pix") },
		"pix-webhooks": func() (mtls.Issued, error) { return pki.Server("localhost", "127.0.0.1") },
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the operator names the directory
		return err
	}
	files := map[string][]byte{"ca.pem": pki.CAPEM()}
	for name, issue := range pairs {
		issued, err := issue()
		if err != nil {
			return err
		}
		files[name+".pem"], files[name+"-key.pem"] = issued.CertPEM, issued.KeyPEM
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil { //nolint:gosec // the operator names the directory
			return err
		}
	}
	fmt.Println("wrote ca.pem and the key pairs vault, api, worker, sim-pix, sim-pix-hook, pix-client and pix-webhooks to", dir)
	return nil
}

// pixBankIdentity is the identity the Pix simulator signs its notifications with.
const pixBankIdentity = "spiffe://sim-pix/webhook"

func verifyRecipient(ctx context.Context, pool *pgxpool.Pool, recipientID string, status recipients.Status) error {
	rid, err := recipients.Prefix.Parse(recipientID)
	if err != nil {
		return err
	}
	s := recipients.New(recipients.Config{Merchants: merchant.New(nil)})
	rec, err := s.ByID(ctx, pool, rid.String())
	if err != nil {
		return err
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := s.Verify(ctx, tx, rec.Owner, rid, status)
		return err
	})
}

// holdPayouts holds a recipient's payouts, or lifts the hold and lets those held go.
func holdPayouts(ctx context.Context, pool *pgxpool.Pool, recipientID string, held bool) error {
	rid, err := recipients.Prefix.Parse(recipientID)
	if err != nil {
		return err
	}
	s := recipients.New(recipients.Config{Merchants: merchant.New(nil)})
	rec, err := s.ByID(ctx, pool, rid.String())
	if err != nil {
		return err
	}
	if !held && rec.Status != recipients.Verified {
		return fmt.Errorf("recipient %s is %s: its payouts go only once it is verified", rid, rec.Status)
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := s.HoldPayouts(ctx, tx, rid.String(), held); err != nil || held {
			return err
		}
		owner := payments.Owner{Merchant: rec.Owner.Merchant, Livemode: rec.Owner.Livemode}
		released, failed, err := payments.New(payments.Config{}).ReleaseHeldPayouts(ctx, tx, owner, rid.String(), rec.Default, rec.PayoutDestination())
		if err == nil {
			fmt.Printf("released %d payouts to %s; %d made for another destination failed\n", released, describe(rec.Destination), failed)
		}
		return err
	})
}

func describe(d recipients.Destination) string {
	if d.Method == "pix" {
		return "Pix key " + d.PixKey
	}
	return fmt.Sprintf("bank %s branch %s account %s", d.ISPB, d.Branch, d.Account)
}

// decideDispute records an operator's decision on a MED claim; the worker tells the bank.
func decideDispute(ctx context.Context, pool *pgxpool.Pool, disputeID string, agree bool, operator string) error {
	parsed, err := disputes.DisputePrefix.Parse(disputeID)
	if err != nil {
		return err
	}
	return disputes.New(disputes.Config{Pool: pool, Payments: payments.New(payments.Config{})}).Decide(ctx, pool, parsed, agree, operator)
}

// reconcileCommand reports a day's reconciliation, or resolves a break by hand.
func reconcileCommand(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	s := reconciliation.New(reconciliation.Config{Pool: pool})
	switch {
	case args[0] == "report" && (len(args) == 2 || (len(args) == 3 && args[2] == "live")):
		day, err := time.Parse(time.DateOnly, args[1])
		if err != nil {
			return err
		}
		report, err := s.Report(ctx, pool, len(args) == 3, day, "")
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	case args[0] == "resolve" && len(args) == 4:
		breakID, err := reconciliation.BreakPrefix.Parse(args[1])
		if err != nil {
			return err
		}
		return s.Resolve(ctx, breakID, args[3], args[2])
	case args[0] == "confirm" && len(args) == 3:
		breakID, err := reconciliation.BreakPrefix.Parse(args[1])
		if err != nil {
			return err
		}
		return s.Confirm(ctx, breakID, args[2])
	}
	return errUsage
}

// operate runs the commands on recipients, disputes and reconciliation.
func operate(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	switch {
	case len(args) == 4 && args[0] == "recipient" && args[1] == "verify":
		return verifyRecipient(ctx, pool, args[2], recipients.Status(args[3]))
	case len(args) == 3 && args[0] == "recipient" && (args[1] == "hold-payouts" || args[1] == "release-payouts"):
		return holdPayouts(ctx, pool, args[2], args[1] == "hold-payouts")
	case len(args) == 5 && args[0] == "dispute" && args[1] == "decide" && (args[3] == "agreed" || args[3] == "disagreed"):
		return decideDispute(ctx, pool, args[2], args[3] == "agreed", args[4])
	case len(args) >= 3 && args[0] == "reconcile":
		return reconcileCommand(ctx, pool, args[1:])
	}
	return errUsage
}
