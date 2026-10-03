package receivables

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/internal/recipients"
)

// A recipient's balance is three accounts. Pending is what its unsettled units hold that
// Jupiter did not buy; available, what it can be paid out; reserved, what is held back
// from it.
const (
	Pending   = "pending"
	Available = "available"
	Reserved  = "reserved"
	// roleAnticipationFees is Jupiter's: what anticipations earned it.
	roleAnticipationFees = "anticipation_fees"
	// roleFinanciers is what settlements owe the financiers a registry paid.
	roleFinanciers = "financiers"
)

var _ payments.RecipientBalances = (*Service)(nil)

// account finds, or makes on first use, a recipient's account (recipient "" for
// Jupiter's).
func (s *Service) account(ctx context.Context, tx pgx.Tx, recipientID string, livemode bool, currency money.Currency, role string) (id.ID, error) {
	q := db.New(tx)
	find := func() (id.ID, bool, error) {
		rows, err := q.LedgerAccounts(ctx, db.LedgerAccountsParams{RecipientID: recipientID, Livemode: livemode, Currency: currency.Code()})
		if err != nil {
			return id.ID{}, false, err
		}
		for _, r := range rows {
			if r.Role == role {
				accountID, err := ledger.AccountPrefix.Parse(r.AccountID)
				return accountID, true, err
			}
		}
		return id.ID{}, false, nil
	}
	if accountID, ok, err := find(); err != nil || ok {
		return accountID, err
	}
	if err := q.LockAccountCreation(ctx, recipientID+"/"+strconv.FormatBool(livemode)+"/"+currency.Code()); err != nil {
		return id.ID{}, err
	}
	if accountID, ok, err := find(); err != nil || ok {
		return accountID, err
	}
	// Jupiter's fee account is hot: every anticipation of a mode posts to it (ADR 0008).
	spec := ledger.AccountSpec{Book: ledger.ClientFunds, Code: "recipient_" + role, Currency: currency, Normal: ledger.CreditNormal, Batched: recipientID == ""}
	account, err := s.cfg.Ledger.CreateAccount(ctx, tx, spec)
	if err != nil {
		return id.ID{}, fmt.Errorf("creating %s account: %w", role, err)
	}
	err = q.InsertLedgerAccount(ctx, db.InsertLedgerAccountParams{
		RecipientID: recipientID, Livemode: livemode, Currency: currency.Code(), Role: role, AccountID: account.ID.String(),
	})
	return account.ID, err
}

// movement records an amount posted to a recipient's account.
func (s *Service) movement(ctx context.Context, q *db.Queries, recipientID string, livemode bool, currency money.Currency, bucket, kind string, amount int64, reference string) error {
	if amount == 0 {
		return nil
	}
	return q.InsertMovement(ctx, db.InsertMovementParams{
		RecipientID: recipientID, Livemode: livemode, Currency: currency.Code(), Bucket: bucket, Kind: kind, Amount: amount,
		Reference: reference, At: ts(s.cfg.Now()),
	})
}

// Balance is a recipient's balance, and when its pending amounts become available: on
// their units' settlement dates.
type Balance struct {
	Recipient string
	Currency  money.Currency
	Pending   int64
	Available int64
	Reserved  int64
	// PendingOn is the pending balance by the day it is to become available.
	PendingOn []DatedAmount
}

type DatedAmount struct {
	Date   string
	Amount int64
}

func (s *Service) Balance(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientID string, currency money.Currency) (Balance, error) {
	rec, err := s.recipient(ctx, tx, owner, recipientID)
	if err != nil {
		return Balance{}, err
	}
	out := Balance{Recipient: rec.ID.String(), Currency: currency}
	for _, role := range []string{Pending, Available, Reserved} {
		accountID, err := s.account(ctx, tx, out.Recipient, owner.Livemode, currency, role)
		if err != nil {
			return Balance{}, err
		}
		b, err := s.cfg.Ledger.Balance(ctx, tx, accountID)
		if err != nil {
			return Balance{}, err
		}
		posted, err := b.Posted()
		if err != nil {
			return Balance{}, err
		}
		switch role {
		case Pending:
			out.Pending = posted.Minor()
		case Available:
			out.Available = posted.Minor() - b.PendingDebits.Minor() // less payouts in flight
		default:
			out.Reserved = posted.Minor()
		}
	}
	dated, err := db.New(tx).PendingByDate(ctx, db.PendingByDateParams{RecipientID: out.Recipient, Livemode: owner.Livemode, Currency: currency.Code()})
	if err != nil {
		return Balance{}, err
	}
	for _, d := range dated {
		out.PendingOn = append(out.PendingOn, DatedAmount{Date: d.SettlementDate.Time.Format(time.DateOnly), Amount: d.Amount})
	}
	return out, nil
}

// recipient reads the merchant's recipient, or its own when recipientID is empty.
func (s *Service) recipient(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientID string) (recipients.Recipient, error) {
	o := recipients.Owner{Merchant: owner.Merchant, Livemode: owner.Livemode}
	if recipientID == "" {
		return s.cfg.Recipients.Default(ctx, tx, o)
	}
	rid, err := recipients.Prefix.Parse(recipientID)
	if err != nil {
		return recipients.Recipient{}, fmt.Errorf("%w: no recipient %s", ErrNotFound, recipientID)
	}
	rec, err := s.cfg.Recipients.Get(ctx, tx, o, rid)
	if err != nil {
		return recipients.Recipient{}, fmt.Errorf("%w: no recipient %s", ErrNotFound, recipientID)
	}
	return rec, nil
}

// PayoutSource is a verified recipient's available account and payout destination.
func (s *Service) PayoutSource(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientID string, currency money.Currency) (payments.PayoutSource, error) {
	rec, err := s.recipient(ctx, tx, owner, recipientID)
	// Only that the recipient is not the merchant's is the merchant's mistake: a failure
	// to read it is not, and must neither be told to it nor kept as the request's answer.
	if errors.Is(err, ErrNotFound) || errors.Is(err, recipients.ErrInvalid) {
		return payments.PayoutSource{}, fmt.Errorf("%w: %w", payments.ErrInvalid, err)
	}
	if err != nil {
		return payments.PayoutSource{}, err
	}
	if rec.Status != recipients.Verified {
		return payments.PayoutSource{}, fmt.Errorf("%w: recipient %s is %s, not verified", payments.ErrInvalid, recipientID, rec.Status)
	}
	accountID, err := s.account(ctx, tx, rec.ID.String(), owner.Livemode, currency, Available)
	d := rec.Destination
	method := d.Method
	if method == "bank_account" {
		method = payments.PayoutBankTransfer
	}
	return payments.PayoutSource{
		Account: accountID, Held: rec.PayoutsHeld,
		Destination: payments.PayoutDestination{
			Method: method, PixKey: d.PixKey, ISPB: d.ISPB, Branch: d.Branch, Account: d.Account, HolderName: rec.Name, HolderTaxID: rec.TaxID,
		},
	}, err
}

// PayoutReturned records that a payout came back to the recipient's available balance.
func (s *Service) PayoutReturned(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientID, payoutID string, amount money.Amount) error {
	return s.movement(ctx, db.New(tx), recipientID, owner.Livemode, amount.Currency(), Available, "payout", amount.Minor(), payoutID)
}

// PaidOut records that a payout left a recipient's available balance.
func (s *Service) PaidOut(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientID, payoutID string, amount money.Amount) error {
	return s.movement(ctx, db.New(tx), recipientID, owner.Livemode, amount.Currency(), Available, "payout", -amount.Minor(), payoutID)
}

// recipientOf is the merchant's recipient named, checked to be the merchant's, or its own
// when none is named; read only, for queries.
func (s *Service) recipientOf(ctx context.Context, q db.DBTX, owner payments.Owner, recipientID string) (string, error) {
	o := recipients.Owner{Merchant: owner.Merchant, Livemode: owner.Livemode}
	if recipientID == "" {
		rec, err := s.cfg.Recipients.DefaultOf(ctx, q, o)
		if err != nil {
			return "", fmt.Errorf("%w: the merchant has no recipient of its own yet", ErrNotFound)
		}
		return rec.ID.String(), nil
	}
	rid, err := recipients.Prefix.Parse(recipientID)
	if err == nil {
		_, err = s.cfg.Recipients.Get(ctx, q, o, rid)
	}
	if err != nil {
		return "", fmt.Errorf("%w: no recipient %s", ErrNotFound, recipientID)
	}
	return recipientID, nil
}
