package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
)

var refundReasons = map[string]bool{"": true, "duplicate": true, "fraudulent": true, "requested_by_customer": true}

type RefundParams struct {
	Intent id.ID
	// Amount nil refunds everything not yet refunded or being refunded.
	Amount *money.Amount
	Reason string
}

// StartRefund records a refund before the rail is called. Refunds in flight count
// against what remains refundable, so concurrent refunds can never exceed the payment.
func (s *Service) StartRefund(ctx context.Context, tx pgx.Tx, owner Owner, p RefundParams) (Refund, error) {
	if !refundReasons[p.Reason] {
		return Refund{}, fmt.Errorf("%w: reason must be duplicate, fraudulent or requested_by_customer", ErrInvalid)
	}
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, p.Intent)
	if err != nil {
		return Refund{}, err
	}
	if Status(row.Status) != Succeeded || !inStatus(attempt, attemptCaptured) {
		return Refund{}, fmt.Errorf("%w: only a succeeded payment intent can be refunded", ErrInvalidState)
	}
	outstanding, err := q.OutstandingRefunds(ctx, row.ID)
	if err != nil {
		return Refund{}, err
	}
	remaining := row.AmountReceived - row.AmountRefunded - outstanding
	amount := remaining
	if p.Amount != nil {
		if p.Amount.Currency() != mustCurrency(row.Currency) || !p.Amount.IsPositive() {
			return Refund{}, fmt.Errorf("%w: amount must be positive and in %s", ErrInvalid, row.Currency)
		}
		amount = p.Amount.Minor()
	}
	if amount <= 0 || amount > remaining {
		return Refund{}, fmt.Errorf("%w: %d requested, %d left to refund", ErrAmountTooLarge, amount, remaining)
	}
	refundID := RefundPrefix.New().String()
	now := ts(s.cfg.Now().UTC())
	if err := q.InsertRefund(ctx, db.InsertRefundParams{
		ID: refundID, IntentID: row.ID, AttemptID: attempt.ID, MerchantID: row.MerchantID, Livemode: row.Livemode,
		Amount: amount, Currency: row.Currency, Reason: p.Reason, CreatedAt: now,
	}); err != nil {
		return Refund{}, fmt.Errorf("recording refund: %w", err)
	}
	if err := s.publish(ctx, tx, owner, events.TypeRefundCreated, refundID); err != nil {
		return Refund{}, err
	}
	return s.Refund(ctx, tx, owner, mustRefundID(refundID))
}

func (s *Service) RefundOnRail(ctx context.Context, q db.DBTX, owner Owner, refundID id.ID) (Result, error) {
	refund, err := db.New(q).GetRefund(ctx, db.GetRefundParams{ID: refundID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Result{}, notFoundOr(err, refundID)
	}
	if refund.Status != string(RefundPending) && refund.Status != "refund_unknown" {
		return Result{}, nil
	}
	return s.refundOnRail(ctx, q, refund)
}

func (s *Service) refundOnRail(ctx context.Context, q db.DBTX, refund db.PaymentsRefund) (Result, error) {
	attempt, err := db.New(q).GetAttempt(ctx, refund.AttemptID)
	if err != nil {
		return Result{}, err
	}
	var rail Rail
	if !isPix(attempt.PaymentMethod) {
		if rail, err = s.rail(refund.Livemode); err != nil {
			return Result{}, err
		}
	}
	amount, err := money.New(refund.Amount, mustCurrency(refund.Currency))
	if err != nil {
		return Result{}, err
	}
	if isPix(attempt.PaymentMethod) {
		return s.returnPix(ctx, refund.Livemode, attempt.NetworkTransactionID, bankID(refund.ID), amount)
	}
	return rail.Refund(ctx, OperationRequest{
		Key: refund.ID, AuthorizationKey: attempt.ID, Reference: attempt.RailReference, Amount: amount,
	}), nil
}

// FinishRefund applies the rail's answer. A successful refund is a posted ledger
// transaction moving the amount back from the merchant to the network.
func (s *Service) FinishRefund(ctx context.Context, tx pgx.Tx, owner Owner, refundID id.ID, res Result) (Refund, error) {
	q := db.New(tx)
	refund, err := q.LockRefundByID(ctx, refundID.String())
	if err != nil {
		return Refund{}, notFoundOr(err, refundID)
	}
	if refund.MerchantID != owner.Merchant.String() || refund.Livemode != owner.Livemode {
		return Refund{}, fmt.Errorf("%w: %s", ErrNotFound, refundID)
	}
	if res.Outcome == "" || (refund.Status != string(RefundPending) && refund.Status != "refund_unknown") {
		return refundFromRow(refund)
	}
	now := s.cfg.Now().UTC()
	switch res.Outcome {
	case Approved:
		err = s.refunded(ctx, tx, &refund, res)
	case Declined:
		refund.Status, refund.FailureReason, refund.RailReference = string(RefundFailed), res.DeclineCode, res.Reference
		err = s.publish(ctx, tx, owner, events.TypeRefundUpdated, refund.ID)
	default:
		refund.Status = "refund_unknown"
		if !refund.UnknownSince.Valid {
			refund.UnknownSince = ts(now)
		}
	}
	if err != nil {
		return Refund{}, err
	}
	refund.UpdatedAt = ts(now)
	if err := q.SaveRefund(ctx, db.SaveRefundParams{
		ID: refund.ID, Status: refund.Status, RailReference: refund.RailReference, FailureReason: refund.FailureReason,
		LedgerTxn: refund.LedgerTxn, UnknownSince: refund.UnknownSince, Resolutions: refund.Resolutions, UpdatedAt: refund.UpdatedAt,
	}); err != nil {
		return Refund{}, err
	}
	return refundFromRow(refund)
}

func (s *Service) refunded(ctx context.Context, tx pgx.Tx, refund *db.PaymentsRefund, res Result) error {
	q := db.New(tx)
	intent, err := q.LockIntentByID(ctx, refund.IntentID)
	if err != nil {
		return err
	}
	owner, err := ownerOf(intent)
	if err != nil {
		return err
	}
	currency := mustCurrency(refund.Currency)
	accts, err := s.ledgerAccounts(ctx, tx, owner, currency)
	if err != nil {
		return err
	}
	amount, err := money.New(refund.Amount, currency)
	if err != nil {
		return err
	}
	// The money goes back the way it came: to the card network, or out of Jupiter's
	// Pix account.
	source := accts.networkReceivable
	attempt, err := q.GetAttempt(ctx, refund.AttemptID)
	if err != nil {
		return err
	}
	if isPix(attempt.PaymentMethod) {
		pix, err := s.pixAccounts(ctx, tx, refund.Livemode, currency)
		if err != nil {
			return err
		}
		source = pix.settlement
	}
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "refund " + refund.ID,
		Legs:        []ledger.Leg{ledger.Debit(accts.merchantBalance, amount), ledger.Credit(source, amount)},
	})
	if err != nil {
		return fmt.Errorf("posting refund to the ledger: %w", err)
	}
	refund.Status, refund.RailReference = string(RefundSucceeded), res.Reference
	refund.LedgerTxn = text(txn.ID.String())
	refund.UnknownSince = pgtype.Timestamptz{}
	intent.AmountRefunded += refund.Amount
	if _, err := s.save(ctx, q, intent); err != nil {
		return err
	}
	return s.publish(ctx, tx, owner, events.TypeRefundUpdated, refund.ID)
}

func (s *Service) Refund(ctx context.Context, q db.DBTX, owner Owner, refundID id.ID) (Refund, error) {
	row, err := db.New(q).GetRefund(ctx, db.GetRefundParams{ID: refundID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Refund{}, notFoundOr(err, refundID)
	}
	return refundFromRow(row)
}

func (s *Service) Refunds(ctx context.Context, q db.DBTX, owner Owner, intent *id.ID, r page.Request) ([]Refund, bool, error) {
	params := db.ListRefundsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	}
	if intent != nil {
		params.IntentID = intent.String()
	}
	rows, err := db.New(q).ListRefunds(ctx, params)
	if err != nil {
		return nil, false, fmt.Errorf("listing refunds: %w", err)
	}
	out := make([]Refund, 0, len(rows))
	for _, row := range rows {
		refund, err := refundFromRow(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, refund)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

func notFoundOr(err error, objectID id.ID) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, objectID)
	}
	return err
}

func mustRefundID(s string) id.ID {
	refundID, err := RefundPrefix.Parse(s)
	if err != nil {
		panic(err)
	}
	return refundID
}
