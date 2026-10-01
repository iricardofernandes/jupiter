package payments

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
)

const (
	roleMerchantBalance   = "merchant_balance"
	roleNetworkReceivable = "network_receivable"
	rolePixSettlement     = "pix_settlement"
	rolePixUnmatched      = "pix_unmatched"
	roleBankSettlement    = "bank_settlement"
)

// accounts are the two ledger accounts a card payment moves between: what the card
// network owes Jupiter, and what Jupiter owes the merchant.
type accounts struct {
	merchantBalance   id.ID
	networkReceivable id.ID
}

// ledgerAccounts finds, or creates on first use, the accounts of an owner and currency.
// Creation is serialized per scope by an advisory lock; the common case, where the
// accounts exist, takes no lock at all.
func (s *Service) ledgerAccounts(ctx context.Context, tx pgx.Tx, owner Owner, currency money.Currency) (accounts, error) {
	q := db.New(tx)
	merchantID, err := s.scopedAccount(ctx, tx, q, owner.Merchant.String(), owner.Livemode, currency, roleMerchantBalance,
		ledger.AccountSpec{Book: ledger.ClientFunds, Code: "merchant_balance", Currency: currency, Normal: ledger.CreditNormal})
	if err != nil {
		return accounts{}, err
	}
	// Every card payment in a mode and currency lands on the same receivable: a hot
	// account, so its balance is batched (ADR 0008).
	networkID, err := s.scopedAccount(ctx, tx, q, "", owner.Livemode, currency, roleNetworkReceivable,
		ledger.AccountSpec{Book: ledger.ClientFunds, Code: "network_receivable", Currency: currency, Normal: ledger.DebitNormal, Batched: true})
	if err != nil {
		return accounts{}, err
	}
	return accounts{merchantBalance: merchantID, networkReceivable: networkID}, nil
}

// pixAccounts are Jupiter's account at its Pix bank, where every Pix in and out of a
// mode settles, and the Pix received that paid nothing, held until they are returned.
type pixAccounts struct {
	settlement id.ID
	unmatched  id.ID
}

func (s *Service) pixAccounts(ctx context.Context, tx pgx.Tx, livemode bool, currency money.Currency) (pixAccounts, error) {
	q := db.New(tx)
	settlement, err := s.scopedAccount(ctx, tx, q, "", livemode, currency, rolePixSettlement,
		ledger.AccountSpec{Book: ledger.ClientFunds, Code: "pix_settlement", Currency: currency, Normal: ledger.DebitNormal, Batched: true})
	if err != nil {
		return pixAccounts{}, err
	}
	unmatched, err := s.scopedAccount(ctx, tx, q, "", livemode, currency, rolePixUnmatched,
		ledger.AccountSpec{Book: ledger.ClientFunds, Code: "pix_unmatched", Currency: currency, Normal: ledger.CreditNormal})
	if err != nil {
		return pixAccounts{}, err
	}
	return pixAccounts{settlement: settlement, unmatched: unmatched}, nil
}

func (s *Service) scopedAccount(ctx context.Context, tx pgx.Tx, q *db.Queries, merchant string, livemode bool, currency money.Currency, role string, spec ledger.AccountSpec) (id.ID, error) {
	find := func() (id.ID, bool, error) {
		rows, err := q.GetLedgerAccounts(ctx, db.GetLedgerAccountsParams{MerchantID: merchant, Livemode: livemode, Currency: currency.Code()})
		if err != nil {
			return id.ID{}, false, err
		}
		for _, row := range rows {
			if row.Role == role {
				accountID, err := ledger.AccountPrefix.Parse(row.AccountID)
				return accountID, true, err
			}
		}
		return id.ID{}, false, nil
	}
	if accountID, ok, err := find(); err != nil || ok {
		return accountID, err
	}
	scope := merchant + "/" + strconv.FormatBool(livemode) + "/" + currency.Code() + "/" + role
	if err := q.LockLedgerAccountCreation(ctx, scope); err != nil {
		return id.ID{}, err
	}
	if accountID, ok, err := find(); err != nil || ok {
		return accountID, err
	}
	account, err := s.cfg.Ledger.CreateAccount(ctx, tx, spec)
	if err != nil {
		return id.ID{}, fmt.Errorf("creating %s account: %w", role, err)
	}
	err = q.InsertLedgerAccount(ctx, db.InsertLedgerAccountParams{
		MerchantID: merchant, Livemode: livemode, Currency: currency.Code(), Role: role, AccountID: account.ID.String(),
	})
	return account.ID, err
}

// MerchantBalance is the ledger balance of what Jupiter owes an owner in a currency.
func (s *Service) MerchantBalance(ctx context.Context, q db.DBTX, owner Owner, currency money.Currency) (ledger.Balance, error) {
	rows, err := db.New(q).GetLedgerAccounts(ctx, db.GetLedgerAccountsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Currency: currency.Code(),
	})
	if err != nil {
		return ledger.Balance{}, err
	}
	for _, row := range rows {
		if row.Role == roleMerchantBalance {
			accountID, err := ledger.AccountPrefix.Parse(row.AccountID)
			if err != nil {
				return ledger.Balance{}, err
			}
			return s.cfg.Ledger.Balance(ctx, q, accountID)
		}
	}
	return ledger.Balance{}, fmt.Errorf("%w: no balance in %v", ErrNotFound, currency)
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

func intentFromRow(row db.PaymentsIntent) (Intent, error) {
	intentID, err := IntentPrefix.Parse(row.ID)
	if err != nil {
		return Intent{}, err
	}
	merchantID, err := id.Parse(row.MerchantID)
	if err != nil {
		return Intent{}, err
	}
	currency, err := money.CurrencyByCode(row.Currency)
	if err != nil {
		return Intent{}, err
	}
	amount := func(minor int64) money.Amount {
		a, _ := money.New(minor, currency) // the currency was validated above
		return a
	}
	it := Intent{
		ID: intentID, Owner: Owner{Merchant: merchantID, Livemode: row.Livemode},
		Amount: amount(row.Amount), CaptureMethod: CaptureMethod(row.CaptureMethod), Status: Status(row.Status),
		PaymentMethod: row.PaymentMethod, Description: row.Description,
		AmountCapturable: amount(row.AmountCapturable), AmountReceived: amount(row.AmountReceived),
		AmountRefunded: amount(row.AmountRefunded), NextAction: row.NextAction,
		CancellationReason: row.CancellationReason, SetupFutureUsage: row.SetupFutureUsage,
		RequestThreeDSecure: row.RequestThreeDSecure, NextActionURL: row.NextActionUrl,
		RiskDecision: row.RiskDecision, RiskDecisionID: row.RiskDecisionID, CreatedAt: row.CreatedAt.Time,
		NextActionData: row.NextActionData, NextActionExpiresAt: row.NextActionExpiresAt.Time,
	}
	if it.Boleto, err = boletoOf(row); err != nil {
		return Intent{}, err
	}
	if it.Split, err = splitOf(row); err != nil {
		return Intent{}, err
	}
	if it.Pix, err = pixOptionsOf(row); err != nil {
		return Intent{}, err
	}
	if row.Installments.Valid {
		it.Installments = &Installments{Count: int(row.Installments.Int32), FinancedBy: Financing(row.InstallmentsFinancedBy.String)}
	}
	if row.LatestAttempt.Valid {
		if it.LatestAttempt, err = AttemptPrefix.Parse(row.LatestAttempt.String); err != nil {
			return Intent{}, err
		}
	}
	if row.LastErrorCode != "" {
		it.LastError = &PaymentError{Code: row.LastErrorCode, DeclineCode: row.LastDeclineCode, Message: row.LastErrorMessage}
	}
	return it, nil
}

func refundFromRow(row db.PaymentsRefund) (Refund, error) {
	refundID, err := RefundPrefix.Parse(row.ID)
	if err != nil {
		return Refund{}, err
	}
	intentID, err := IntentPrefix.Parse(row.IntentID)
	if err != nil {
		return Refund{}, err
	}
	merchantID, err := id.Parse(row.MerchantID)
	if err != nil {
		return Refund{}, err
	}
	currency, err := money.CurrencyByCode(row.Currency)
	if err != nil {
		return Refund{}, err
	}
	amount, err := money.New(row.Amount, currency)
	if err != nil {
		return Refund{}, err
	}
	status := RefundStatus(row.Status)
	if status == "refund_unknown" {
		status = RefundPending
	}
	return Refund{
		ID: refundID, Owner: Owner{Merchant: merchantID, Livemode: row.Livemode}, Intent: intentID,
		Amount: amount, Reason: row.Reason, Status: status, FailureReason: row.FailureReason, CreatedAt: row.CreatedAt.Time,
	}, nil
}

func saveIntent(ctx context.Context, q *db.Queries, row db.PaymentsIntent) error {
	return q.SaveIntent(ctx, db.SaveIntentParams{
		ID: row.ID, Amount: row.Amount, Status: row.Status, PaymentMethod: row.PaymentMethod,
		Description: row.Description, AmountCapturable: row.AmountCapturable, AmountReceived: row.AmountReceived,
		AmountRefunded: row.AmountRefunded, LatestAttempt: row.LatestAttempt, LastErrorCode: row.LastErrorCode,
		LastDeclineCode: row.LastDeclineCode, LastErrorMessage: row.LastErrorMessage, NextAction: row.NextAction,
		CancellationReason: row.CancellationReason, Installments: row.Installments,
		InstallmentsFinancedBy: row.InstallmentsFinancedBy, SetupFutureUsage: row.SetupFutureUsage,
		RequestThreeDSecure: row.RequestThreeDSecure, NextActionUrl: row.NextActionUrl,
		RiskDecision: row.RiskDecision, RiskDecisionID: row.RiskDecisionID, PixOptions: row.PixOptions, Split: row.Split, BoletoOptions: row.BoletoOptions,
		NextActionData: row.NextActionData, NextActionExpiresAt: row.NextActionExpiresAt, UpdatedAt: row.UpdatedAt,
	})
}

func saveAttempt(ctx context.Context, q *db.Queries, row db.PaymentsAttempt) error {
	return q.SaveAttempt(ctx, db.SaveAttemptParams{
		ID: row.ID, Status: row.Status, Authenticated: row.Authenticated, RailReference: row.RailReference,
		DeclineCode: row.DeclineCode, LedgerHold: row.LedgerHold, CaptureAmount: row.CaptureAmount,
		AmountCaptured: row.AmountCaptured, AuthorizationExpiresAt: row.AuthorizationExpiresAt,
		UnknownSince: row.UnknownSince, Resolutions: row.Resolutions, NetworkTransactionID: row.NetworkTransactionID,
		ClearedOn: row.ClearedOn, AmountCleared: row.AmountCleared, RiskDecision: row.RiskDecision,
		RiskDecisionID: row.RiskDecisionID, ThreeDsServerTransID: row.ThreeDsServerTransID,
		ThreeDsVersion: row.ThreeDsVersion, ThreeDsStatus: row.ThreeDsStatus, DsTransID: row.DsTransID,
		AcsTransID: row.AcsTransID, AcsUrl: row.AcsUrl, Eci: row.Eci, AuthenticationValue: row.AuthenticationValue,
		LiabilityShift: row.LiabilityShift, UpdatedAt: row.UpdatedAt,
	})
}

// bankAccount is Jupiter's account at the bank that collects its boletos and makes its
// transfers: a hot account (ADR 0008).
func (s *Service) bankAccount(ctx context.Context, tx pgx.Tx, livemode bool, currency money.Currency) (id.ID, error) {
	return s.scopedAccount(ctx, tx, db.New(tx), "", livemode, currency, roleBankSettlement,
		ledger.AccountSpec{Book: ledger.ClientFunds, Code: "bank_settlement", Currency: currency, Normal: ledger.DebitNormal, Batched: true})
}

// SettlementAccounts are the accounts a card settlement moves: what the networks owe,
// and Jupiter's account at the bank the settlement is paid into.
func (s *Service) SettlementAccounts(ctx context.Context, tx pgx.Tx, livemode bool, currency money.Currency) (network, bank id.ID, err error) {
	if network, err = s.scopedAccount(ctx, tx, db.New(tx), "", livemode, currency, roleNetworkReceivable,
		ledger.AccountSpec{Book: ledger.ClientFunds, Code: "network_receivable", Currency: currency, Normal: ledger.DebitNormal, Batched: true}); err != nil {
		return id.ID{}, id.ID{}, err
	}
	bank, err = s.bankAccount(ctx, tx, livemode, currency)
	return network, bank, err
}
