package bank

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

// Banks connects to the bank in each mode that has one: live mode with JUPITER_BANK_URL,
// test mode with JUPITER_BANK_TEST_URL, each with its _TOKEN, _CODE (the bank's
// three-digit code), _NAME, _AGREEMENT and _BRANCH and _ACCOUNT as number-digit; Jupiter
// is JUPITER_TAX_ID and JUPITER_LEGAL_NAME.
func Banks(getenv func(string) string, pool *pgxpool.Pool, logger *slog.Logger) (live, test *Connector, err error) {
	for _, m := range []struct {
		prefix   string
		livemode bool
		into     **Connector
	}{{"JUPITER_BANK_", true, &live}, {"JUPITER_BANK_TEST_", false, &test}} {
		if getenv(m.prefix+"URL") == "" {
			continue
		}
		if *m.into, err = fromEnv(getenv, m.prefix, m.livemode, pool, logger); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", m.prefix+"URL", err)
		}
	}
	if live != nil && test != nil && (live.cfg.Token == test.cfg.Token || live.cfg.Agreement == test.cfg.Agreement) {
		return nil, nil, errors.New("bank: live and test mode must use different tokens and agreements")
	}
	return live, test, nil
}

func fromEnv(getenv func(string) string, prefix string, livemode bool, pool *pgxpool.Pool, logger *slog.Logger) (*Connector, error) {
	code := getenv(prefix + "CODE")
	if len(code) != 3 || strings.Trim(code, "0123456789") != "" {
		return nil, errors.New("_CODE must be the bank's three-digit code")
	}
	branch, branchDV, ok1 := strings.Cut(getenv(prefix+"BRANCH"), "-")
	number, numberDV, ok2 := strings.Cut(getenv(prefix+"ACCOUNT"), "-")
	if !ok1 || !ok2 || len(branch) > 5 || len(number) > 12 || len(branchDV) != 1 || len(numberDV) != 1 {
		return nil, errors.New("_BRANCH and _ACCOUNT must be number-digit, of up to 5 and 12 digits")
	}
	if !taxid.Valid(getenv("JUPITER_TAX_ID")) || getenv("JUPITER_LEGAL_NAME") == "" {
		return nil, errors.New("JUPITER_TAX_ID and JUPITER_LEGAL_NAME must be Jupiter's when a bank is configured")
	}
	return New(Config{
		BaseURL: getenv(prefix + "URL"), Token: getenv(prefix + "TOKEN"), Livemode: livemode,
		Profile: cnab240.Febraban{BankCode: code, BankName: getenv(prefix + "NAME")},
		Account: cnab240.Account{
			Branch: fmt.Sprintf("%05s", branch), BranchDV: branchDV, Number: fmt.Sprintf("%012s", number), NumberDV: numberDV,
		},
		Agreement: getenv(prefix + "AGREEMENT"), TaxID: getenv("JUPITER_TAX_ID"), Name: getenv("JUPITER_LEGAL_NAME"),
		Pool: pool, Logger: logger,
	})
}

// Configure sets the payments rails of the connectors that exist: boletos and transfers.
func Configure(cfg *payments.Config, live, test *Connector) {
	if live != nil {
		cfg.LiveBoleto, cfg.LiveTransfers = live, live
	}
	if test != nil {
		cfg.TestBoleto, cfg.TestTransfers = test, test
	}
}
