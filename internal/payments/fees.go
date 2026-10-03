package payments

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/payments/rules"
)

const roleCardFees = "card_fees"

// Receivables learns of every card capture and refund in the transaction that posts it,
// so what the merchant is owed and when is never out of step with the ledger. A capture's
// net amount may be moved from the merchant's balance to its recipients', and a refund's
// back: each says how much it moved.
type Receivables interface {
	Captured(ctx context.Context, tx pgx.Tx, c CardCapture) (int64, error)
	Refunded(ctx context.Context, tx pgx.Tx, r CardRefund) (int64, error)
	// Disputed takes a card dispute from the payment's liable recipient back to the
	// merchant's balance, and answers how much; DisputeReinstated gives it back.
	Disputed(ctx context.Context, tx pgx.Tx, d CardDispute) (int64, error)
	DisputeReinstated(ctx context.Context, tx pgx.Tx, d CardDispute) (int64, error)
	// Carries refuses a card payment whose receivables it could not keep, before it is
	// authorized: its money would otherwise be paid out long before the network pays it.
	Carries(ctx context.Context, tx pgx.Tx, owner Owner, scheme string) error
}

// CardCapture is a card payment Jupiter captured: Amount is what was captured and Fee
// Jupiter's part of it.
type CardCapture struct {
	Owner        Owner
	Intent       id.ID
	Attempt      string
	Scheme       string
	Installments *Installments
	Amount       money.Amount
	Fee          money.Amount
	At           time.Time
	Split        []SplitRule
	// Balance is the merchant's balance, where the capture was posted.
	Balance id.ID
}

// CardRefund is a refund of a card payment: Amount went back to the customer, and
// FeeReturned of Jupiter's fee back to the merchant.
type CardRefund struct {
	Owner       Owner
	Intent      id.ID
	Attempt     string
	Refund      string
	Amount      money.Amount
	FeeReturned money.Amount
	At          time.Time
	// Balance is the merchant's balance, which the refund was posted from.
	Balance id.ID
}

func (s *Service) feeAccount(ctx context.Context, tx pgx.Tx, livemode bool, currency money.Currency) (id.ID, error) {
	// Every fee in a mode and currency lands here: a hot account (ADR 0008). The fees
	// stay among client funds until settlement moves them to Jupiter's own (SweepFees).
	return s.scopedAccount(ctx, tx, db.New(tx), "", livemode, currency, roleCardFees,
		ledger.AccountSpec{Book: ledger.ClientFunds, Code: "card_fees", Currency: currency, Normal: ledger.CreditNormal, Batched: true})
}

const (
	roleOwnBank        = "own_bank"
	roleCardFeeRevenue = "card_fee_revenue"
)

// SweepFees moves to Jupiter's own funds what the fee account holds beyond keep, the fees
// of payments yet to settle, once per reference: a settlement paid the rest into the
// client funds account with the merchants' money, and they are Jupiter's. In each book
// the money moves between the bank account and what it is for: out of the card fees
// Jupiter holds among client funds, into its revenue.
func (s *Service) SweepFees(ctx context.Context, tx pgx.Tx, livemode bool, currency money.Currency, keep int64, reference string) error {
	q := db.New(tx)
	fees, err := s.feeAccount(ctx, tx, livemode, currency)
	if err != nil {
		return err
	}
	balance, err := s.cfg.Ledger.Balance(ctx, tx, fees)
	if err != nil {
		return err
	}
	posted, err := balance.Posted()
	if err != nil {
		return err
	}
	if posted.Minor() <= keep {
		return nil
	}
	amount, err := money.New(posted.Minor()-keep, currency)
	if err != nil {
		return err
	}
	clientBank, err := s.bankAccount(ctx, tx, livemode, amount.Currency())
	if err != nil {
		return err
	}
	ownBank, err := s.scopedAccount(ctx, tx, q, "", livemode, amount.Currency(), roleOwnBank,
		ledger.AccountSpec{Book: ledger.OwnFunds, Code: "own_bank", Currency: amount.Currency(), Normal: ledger.DebitNormal})
	if err != nil {
		return err
	}
	revenue, err := s.scopedAccount(ctx, tx, q, "", livemode, amount.Currency(), roleCardFeeRevenue,
		ledger.AccountSpec{Book: ledger.OwnFunds, Code: "card_fee_revenue", Currency: amount.Currency(), Normal: ledger.CreditNormal})
	if err != nil {
		return err
	}
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "fees of " + reference + " to own funds",
		Legs: []ledger.Leg{
			ledger.Debit(fees, amount), ledger.Credit(clientBank, amount),
			ledger.Debit(ownBank, amount), ledger.Credit(revenue, amount),
		},
	})
	if err != nil {
		return fmt.Errorf("moving fees to own funds: %w", err)
	}
	inserted, err := q.InsertFeeSweep(ctx, db.InsertFeeSweepParams{
		Reference: reference, Livemode: livemode, Currency: amount.Currency().Code(), Amount: amount.Minor(),
		LedgerTxn: txn.ID.String(), Now: ts(s.cfg.Now().UTC()),
	})
	if err == nil && inserted == 0 {
		err = fmt.Errorf("payments: the fees of %s were moved already", reference)
	}
	return err
}

// chargeFee takes Jupiter's fee on a card capture out of the merchant's balance, at the
// price in effect when it was captured, and tells receivables.
func (s *Service) chargeFee(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt, captured money.Amount) error {
	owner, err := ownerOf(*row)
	if err != nil {
		return err
	}
	scheme, err := s.schemeOf(ctx, tx, owner, row.PaymentMethod)
	if err != nil {
		return err
	}
	count, financedBy := 1, ""
	var installments *Installments
	if attempt.Installments.Valid {
		count, financedBy = int(attempt.Installments.Int32), attempt.InstallmentsFinancedBy.String
		installments = &Installments{Count: count, FinancedBy: Financing(financedBy)}
	}
	now := s.cfg.Now().UTC()
	price, ok := rules.CardFee(scheme, financedBy, count, now)
	if !ok {
		return fmt.Errorf("payments: no price for %s in %d installments financed by %q", scheme, count, financedBy)
	}
	fee, err := captured.MulRate(price.Rate, money.HalfUp)
	if err != nil {
		return err
	}
	if fee.IsPositive() {
		if err := s.postFee(ctx, tx, owner, fee, "fee on "+attempt.ID, false); err != nil {
			return err
		}
		if err := db.New(tx).SetAttemptFee(ctx, db.SetAttemptFeeParams{ID: attempt.ID, Fee: fee.Minor()}); err != nil {
			return err
		}
	}
	if s.cfg.Receivables == nil {
		return nil
	}
	intentID, err := IntentPrefix.Parse(row.ID)
	if err != nil {
		return err
	}
	rules, err := splitOf(*row)
	if err != nil {
		return err
	}
	accts, err := s.ledgerAccounts(ctx, tx, owner, captured.Currency())
	if err != nil {
		return err
	}
	moved, err := s.cfg.Receivables.Captured(ctx, tx, CardCapture{
		Owner: owner, Intent: intentID, Attempt: attempt.ID, Scheme: scheme, Installments: installments,
		Amount: captured, Fee: fee, At: now, Split: rules, Balance: accts.merchantBalance,
	})
	if err != nil || moved == 0 {
		return err
	}
	return db.New(tx).SetSplitOut(ctx, db.SetSplitOutParams{ID: attempt.ID, SplitOut: moved})
}

// postFee moves a fee from the merchant to Jupiter's fee account, or back.
func (s *Service) postFee(ctx context.Context, tx pgx.Tx, owner Owner, fee money.Amount, description string, back bool) error {
	accts, err := s.ledgerAccounts(ctx, tx, owner, fee.Currency())
	if err != nil {
		return err
	}
	fees, err := s.feeAccount(ctx, tx, owner.Livemode, fee.Currency())
	if err != nil {
		return err
	}
	legs := []ledger.Leg{ledger.Debit(accts.merchantBalance, fee), ledger.Credit(fees, fee)}
	if back {
		legs = []ledger.Leg{ledger.Debit(fees, fee), ledger.Credit(accts.merchantBalance, fee)}
	}
	if _, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{Description: description, Legs: legs}); err != nil {
		return fmt.Errorf("posting %s to the ledger: %w", description, err)
	}
	return nil
}

// returnFee gives the merchant back the refunded share of the fee: cumulatively, so the
// refunds of a whole payment return the whole fee, never a centavo more.
func (s *Service) returnFee(ctx context.Context, tx pgx.Tx, intent db.PaymentsIntent, attempt db.PaymentsAttempt, refund *db.PaymentsRefund) error {
	owner, err := ownerOf(intent)
	if err != nil {
		return err
	}
	currency := mustCurrency(refund.Currency)
	returned, err := feeToReturn(ctx, db.New(tx), intent, attempt, refund)
	if err != nil {
		return err
	}
	if returned > 0 {
		amount, _ := money.New(returned, currency)
		if err := s.postFee(ctx, tx, owner, amount, "fee returned by "+refund.ID, true); err != nil {
			return err
		}
		if err := db.New(tx).SetRefundFee(ctx, db.SetRefundFeeParams{ID: refund.ID, FeeReturned: returned}); err != nil {
			return err
		}
	}
	if s.cfg.Receivables == nil {
		return nil
	}
	intentID, err := IntentPrefix.Parse(intent.ID)
	if err != nil {
		return err
	}
	amount, err := money.New(refund.Amount, currency)
	if err != nil {
		return err
	}
	if returned < 0 {
		return fmt.Errorf("payments: refund %s would return %d of the fee", refund.ID, returned)
	}
	feeReturned, err := money.New(returned, currency)
	if err != nil {
		return err
	}
	accts, err := s.ledgerAccounts(ctx, tx, owner, currency)
	if err != nil {
		return err
	}
	back, err := s.cfg.Receivables.Refunded(ctx, tx, CardRefund{
		Owner: owner, Intent: intentID, Attempt: attempt.ID, Refund: refund.ID, Amount: amount, FeeReturned: feeReturned,
		At: s.cfg.Now().UTC(), Balance: accts.merchantBalance,
	})
	if err != nil || back == 0 {
		return err
	}
	return db.New(tx).SetSplitBack(ctx, db.SetSplitBackParams{ID: refund.ID, SplitBack: back})
}

// feeToReturn is the share of the fee refund gives back: what refunds up to and
// including it return in all, less what earlier ones returned.
func feeToReturn(ctx context.Context, q *db.Queries, intent db.PaymentsIntent, attempt db.PaymentsAttempt, refund *db.PaymentsRefund) (int64, error) {
	if attempt.Fee == 0 || attempt.AmountCaptured == 0 {
		return 0, nil
	}
	before, err := q.FeesReturned(ctx, intent.ID)
	if err != nil {
		return 0, err
	}
	// intent.AmountRefunded does not count this refund yet.
	return floorShare(attempt.Fee, intent.AmountRefunded+refund.Amount, attempt.AmountCaptured) - before, nil
}

// floorShare is amount × num / den rounded down, without overflowing.
func floorShare(amount, num, den int64) int64 {
	p := new(big.Int).Mul(big.NewInt(amount), big.NewInt(num))
	return p.Quo(p, big.NewInt(den)).Int64()
}
