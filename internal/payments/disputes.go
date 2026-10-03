package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
)

// Disputes take money from a payment's merchant balance the way refunds do: a card
// dispute withdraws it at once, to the network, and the payment's recipients give back
// to the merchant's balance what the liable one owes for it; a MED claim on a Pix holds
// it while the claim is analysed, and withdraws it, to the payer, if the claim is upheld.
// Each is recorded under the dispute's id, so that applying it again changes nothing.

// CardDispute is a card dispute's amount, withdrawn from or reinstated to the merchant's
// balance, for receivables to take from, or give back to, the liable recipient.
type CardDispute struct {
	Owner     Owner
	Intent    id.ID
	Attempt   string
	Reference string
	Amount    money.Amount
	// Balance is the merchant's balance.
	Balance id.ID
}

// DisputedPayment is a captured payment a dispute names.
type DisputedPayment struct {
	Owner    Owner
	Intent   id.ID
	Attempt  string
	Method   string // card or pix
	Scheme   string
	Captured money.Amount
	Refunded int64
	// Disputed is what disputes already hold or took of it.
	Disputed int64
	// At is when the card payment was authorized, or the Pix received.
	At time.Time
	// NetworkID is the card network's transaction id, or the Pix's end-to-end id.
	NetworkID string
}

// Disputable is what may still be disputed.
func (p DisputedPayment) Disputable() int64 {
	return p.Captured.Minor() - p.Refunded - p.Disputed
}

// MethodCard is a disputed payment's method when it was by card.
const MethodCard = "card"

// What a dispute did to a payment's money.
const (
	fundsHeld       = "held"
	fundsWithdrawn  = "withdrawn"
	fundsReinstated = "reinstated"
	fundsReleased   = "released"
)

// DisputedPaymentByNetworkID finds the captured payment a network transaction id or a
// Pix end-to-end id names.
func (s *Service) DisputedPaymentByNetworkID(ctx context.Context, q db.DBTX, livemode bool, networkID string) (DisputedPayment, error) {
	if networkID == "" {
		// An empty id names nothing; it would match every payment that has none.
		return DisputedPayment{}, fmt.Errorf("%w: no network transaction id", ErrNotFound)
	}
	attempt, err := db.New(q).CapturedAttemptByNetworkID(ctx, db.CapturedAttemptByNetworkIDParams{NetworkID: networkID, Livemode: livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return DisputedPayment{}, fmt.Errorf("%w: no captured payment %s", ErrNotFound, networkID)
	}
	if err != nil {
		return DisputedPayment{}, err
	}
	return s.disputedPayment(ctx, q, attempt)
}

// DisputedPayment is an intent's captured payment, for a dispute.
func (s *Service) DisputedPayment(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID) (DisputedPayment, error) {
	row, attempt, err := s.current(ctx, db.New(q), owner, intentID)
	if err != nil {
		return DisputedPayment{}, err
	}
	if Status(row.Status) != Succeeded || !inStatus(attempt, attemptCaptured) {
		return DisputedPayment{}, fmt.Errorf("%w: only a succeeded payment intent can be disputed", ErrInvalidState)
	}
	return s.disputedPayment(ctx, q, attempt)
}

func (s *Service) disputedPayment(ctx context.Context, q db.DBTX, attempt db.PaymentsAttempt) (DisputedPayment, error) {
	queries := db.New(q)
	intent, err := queries.GetIntentByID(ctx, attempt.IntentID)
	if err != nil {
		return DisputedPayment{}, err
	}
	owner, err := ownerOf(intent)
	if err != nil {
		return DisputedPayment{}, err
	}
	intentID, err := IntentPrefix.Parse(intent.ID)
	if err != nil {
		return DisputedPayment{}, err
	}
	captured, err := money.New(attempt.AmountCaptured, mustCurrency(intent.Currency))
	if err != nil {
		return DisputedPayment{}, err
	}
	disputed, err := queries.DisputedAmount(ctx, intent.ID)
	if err != nil {
		return DisputedPayment{}, err
	}
	p := DisputedPayment{
		Owner: owner, Intent: intentID, Attempt: attempt.ID, Method: MethodCard, Captured: captured, Refunded: intent.AmountRefunded,
		Disputed: disputed, At: attempt.CreatedAt.Time, NetworkID: attempt.NetworkTransactionID,
	}
	switch attempt.PaymentMethod {
	case PaymentMethodBoleto:
		return DisputedPayment{}, fmt.Errorf("%w: a boleto cannot be disputed", ErrInvalidState)
	case PaymentMethodPix:
		p.Method, p.Scheme = PaymentMethodPix, PaymentMethodPix
		received, err := queries.PixReceivedOf(ctx, db.PixReceivedOfParams{Livemode: owner.Livemode, E2eID: attempt.NetworkTransactionID})
		if err != nil {
			return DisputedPayment{}, notFoundOr(err, intentID)
		}
		p.At = received.ReceivedAt.Time
	default:
		if p.Scheme, err = s.schemeOf(ctx, q, owner, attempt.PaymentMethod); err != nil {
			return DisputedPayment{}, err
		}
	}
	return p, nil
}

// WithdrawDisputed takes a card dispute's amount, up to what is left of the payment once
// refunds and other disputes are counted, from the merchant's balance to the network;
// receivables take it from the liable recipient back to the balance. It answers what it
// withdrew: nothing when nothing is left, or the dispute's funds were recorded already.
func (s *Service) WithdrawDisputed(ctx context.Context, tx pgx.Tx, reference string, p DisputedPayment, most money.Amount) (int64, error) {
	if p.Method != MethodCard {
		return 0, fmt.Errorf("%w: a Pix is held first, then withdrawn", ErrInvalid)
	}
	q := db.New(tx)
	left, err := s.lockForDispute(ctx, q, p)
	if err != nil || left <= 0 || !most.IsPositive() {
		return 0, err
	}
	if err := lockBalance(ctx, q, p.Owner, most.Currency(), ""); err != nil {
		return 0, err
	}
	if most.Minor() > left {
		// The network took more than the payment had left once refunds and other disputes
		// were counted: the rest is Jupiter's to contest with the network, not the
		// merchant's to pay twice.
		s.cfg.Logger.ErrorContext(ctx, "a chargeback beyond what is left of its payment", "dispute", reference, "intent", p.Intent, "beyond", most.Minor()-left)
	}
	amount, _ := money.New(min(most.Minor(), left), most.Currency())
	accts, err := s.ledgerAccounts(ctx, tx, p.Owner, amount.Currency())
	if err != nil {
		return 0, err
	}
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "dispute " + reference,
		Legs:        []ledger.Leg{ledger.Debit(accts.merchantBalance, amount), ledger.Credit(accts.networkReceivable, amount)},
	})
	if err != nil {
		return 0, fmt.Errorf("posting a dispute: %w", err)
	}
	inserted, err := q.InsertDisputeFunds(ctx, db.InsertDisputeFundsParams{
		Reference: reference, IntentID: p.Intent.String(), AttemptID: p.Attempt, MerchantID: p.Owner.Merchant.String(),
		Livemode: p.Owner.Livemode, Currency: amount.Currency().Code(), Amount: amount.Minor(), Status: fundsWithdrawn,
		LedgerTxn: text(txn.ID.String()), Now: ts(s.cfg.Now().UTC()),
	})
	if err != nil || inserted == 0 {
		return 0, errors.Join(err, errDisputeFundsExist)
	}
	if s.cfg.Receivables == nil {
		return amount.Minor(), nil
	}
	back, err := s.cfg.Receivables.Disputed(ctx, tx, CardDispute{
		Owner: p.Owner, Intent: p.Intent, Attempt: p.Attempt, Reference: reference, Amount: amount, Balance: accts.merchantBalance,
	})
	if err != nil {
		return 0, err
	}
	return amount.Minor(), q.SaveDisputeFunds(ctx, db.SaveDisputeFundsParams{
		Reference: reference, Status: fundsWithdrawn, SplitBack: back, LedgerTxn: text(txn.ID.String()), Now: ts(s.cfg.Now().UTC()),
	})
}

// errDisputeFundsExist rolls back a withdrawal or hold whose dispute already has funds:
// the caller checks for them first, under the dispute's lock, so it means a bug.
var errDisputeFundsExist = errors.New("payments: the dispute's funds were recorded already")

// lockForDispute locks the payment's intent, so that refunds and disputes of it go one at
// a time, and answers what is left of it to dispute: what was captured less what was
// refunded, what refunds in flight will take, and what disputes hold or took.
func (s *Service) lockForDispute(ctx context.Context, q *db.Queries, p DisputedPayment) (int64, error) {
	intent, err := q.LockIntentByID(ctx, p.Intent.String())
	if err != nil {
		return 0, err
	}
	disputed, err := q.DisputedAmount(ctx, intent.ID)
	if err != nil {
		return 0, err
	}
	outstanding, err := q.OutstandingRefunds(ctx, intent.ID)
	if err != nil {
		return 0, err
	}
	return p.Captured.Minor() - intent.AmountRefunded - outstanding - disputed, nil
}

// HoldDisputed holds a MED claim's amount on the merchant's balance, up to what it has
// available, and answers how much it held: what the rest of the claim is traced through.
func (s *Service) HoldDisputed(ctx context.Context, tx pgx.Tx, reference string, p DisputedPayment, amount money.Amount) (int64, error) {
	if p.Method != PaymentMethodPix {
		return 0, fmt.Errorf("%w: only a Pix is held for a MED claim", ErrInvalid)
	}
	q := db.New(tx)
	left, err := s.lockForDispute(ctx, q, p)
	if err != nil || left <= 0 {
		return 0, err
	}
	if err := lockBalance(ctx, q, p.Owner, amount.Currency(), ""); err != nil {
		return 0, err
	}
	source, available, err := s.payoutFunds(ctx, tx, p.Owner, PayoutParams{Amount: amount}, id.ID{})
	if err != nil {
		return 0, err
	}
	held := min(amount.Minor(), left, max(available, 0))
	if held == 0 {
		return 0, nil
	}
	pix, err := s.pixAccounts(ctx, tx, p.Owner.Livemode, amount.Currency())
	if err != nil {
		return 0, err
	}
	holding, _ := money.New(held, amount.Currency())
	hold, err := s.cfg.Ledger.Hold(ctx, tx, ledger.Hold{Description: "MED claim " + reference, Debit: source, Credit: pix.settlement, Amount: holding})
	if err != nil {
		return 0, fmt.Errorf("holding a MED claim: %w", err)
	}
	inserted, err := q.InsertDisputeFunds(ctx, db.InsertDisputeFundsParams{
		Reference: reference, IntentID: p.Intent.String(), AttemptID: p.Attempt, MerchantID: p.Owner.Merchant.String(),
		Livemode: p.Owner.Livemode, Currency: amount.Currency().Code(), Amount: held, Status: fundsHeld,
		LedgerHold: text(hold.ID.String()), Now: ts(s.cfg.Now().UTC()),
	})
	if err != nil || inserted == 0 {
		return 0, errors.Join(err, errDisputeFundsExist)
	}
	return held, nil
}

// SettleDisputeHold ends a MED claim's hold: withdrawn to the payer when the claim was
// upheld and the money returned, released otherwise. A hold ended already stays as it is.
func (s *Service) SettleDisputeHold(ctx context.Context, tx pgx.Tx, reference string, withdraw bool) error {
	q := db.New(tx)
	f, err := q.LockDisputeFunds(ctx, reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing was held
	}
	if err != nil || f.Status != fundsHeld {
		return err
	}
	holdID, err := ledger.TransactionPrefix.Parse(f.LedgerHold.String)
	if err != nil {
		return err
	}
	status, txnID := fundsReleased, ""
	if withdraw {
		amount, err := money.New(f.Amount, mustCurrency(f.Currency))
		if err != nil {
			return err
		}
		txn, err := s.cfg.Ledger.PostPending(ctx, tx, holdID, amount)
		if err != nil {
			return fmt.Errorf("withdrawing a MED claim: %w", err)
		}
		status, txnID = fundsWithdrawn, txn.ID.String()
	} else if _, err := s.cfg.Ledger.Void(ctx, tx, holdID); err != nil {
		return fmt.Errorf("releasing a MED claim: %w", err)
	}
	return q.SaveDisputeFunds(ctx, db.SaveDisputeFundsParams{
		Reference: reference, Status: status, SplitBack: f.SplitBack, LedgerTxn: text(txnID), Now: ts(s.cfg.Now().UTC()),
	})
}

// ReturnedOnMED records that the Pix bank returned a MED claim's amount to the payer. The
// claim's hold, if one was set, is withdrawn; what the bank returned beyond it, which
// left Jupiter's Pix account all the same, is taken from the merchant's balance too, below
// zero if the balance has not got it: the merchant owes it. It answers what the claim
// took in all, nothing when it was released or reinstated already.
func (s *Service) ReturnedOnMED(ctx context.Context, tx pgx.Tx, reference string, p DisputedPayment, returned int64) (int64, error) {
	if err := s.SettleDisputeHold(ctx, tx, reference, true); err != nil {
		return 0, err
	}
	q := db.New(tx)
	held := int64(0)
	f, err := q.LockDisputeFunds(ctx, reference)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return 0, err
	case f.Status != fundsWithdrawn:
		return 0, nil
	default:
		held = f.Amount
	}
	// The bank cannot have returned more than the payment still had: what a message says
	// beyond that is not taken from the merchant.
	if most := p.Captured.Minor() - p.Refunded; returned > most {
		s.cfg.Logger.ErrorContext(ctx, "a MED return beyond what is left of its payment", "dispute", reference, "returned", returned, "left", most)
		returned = most
	}
	beyond := returned - held
	if beyond <= 0 {
		return held, nil
	}
	currency := p.Captured.Currency()
	txn, err := s.postBeyondHold(ctx, tx, reference, p, beyond)
	if err != nil {
		return 0, err
	}
	now := ts(s.cfg.Now().UTC())
	if held > 0 {
		return returned, q.AddToDisputeFunds(ctx, db.AddToDisputeFundsParams{Reference: reference, Amount: beyond, Now: now})
	}
	inserted, err := q.InsertDisputeFunds(ctx, db.InsertDisputeFundsParams{
		Reference: reference, IntentID: p.Intent.String(), AttemptID: p.Attempt, MerchantID: p.Owner.Merchant.String(),
		Livemode: p.Owner.Livemode, Currency: currency.Code(), Amount: beyond, Status: fundsWithdrawn,
		LedgerTxn: text(txn.ID.String()), Now: now,
	})
	if err != nil || inserted == 0 {
		return 0, errors.Join(err, errDisputeFundsExist)
	}
	return returned, nil
}

// postBeyondHold takes from the merchant's balance, under its lock, what a MED return
// took from Jupiter's Pix account beyond the claim's hold.
func (s *Service) postBeyondHold(ctx context.Context, tx pgx.Tx, reference string, p DisputedPayment, beyond int64) (ledger.Transaction, error) {
	currency := p.Captured.Currency()
	if err := lockBalance(ctx, db.New(tx), p.Owner, currency, ""); err != nil {
		return ledger.Transaction{}, err
	}
	accts, err := s.ledgerAccounts(ctx, tx, p.Owner, currency)
	if err != nil {
		return ledger.Transaction{}, err
	}
	pix, err := s.pixAccounts(ctx, tx, p.Owner.Livemode, currency)
	if err != nil {
		return ledger.Transaction{}, err
	}
	amount, err := money.New(beyond, currency)
	if err != nil {
		return ledger.Transaction{}, err
	}
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "MED return " + reference + " beyond the hold",
		Legs:        []ledger.Leg{ledger.Debit(accts.merchantBalance, amount), ledger.Credit(pix.settlement, amount)},
	})
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("posting a MED return beyond its hold: %w", err)
	}
	return txn, nil
}

// ReinstateDisputed gives back what a dispute withdrew: from the network, for a card,
// where receivables then give it to the liable recipient; from the payer's bank, for a
// Pix whose MED return was contested. It answers what it gave back, 0 when there was
// nothing withdrawn or it was given back already.
func (s *Service) ReinstateDisputed(ctx context.Context, tx pgx.Tx, reference string) (int64, error) {
	q := db.New(tx)
	f, err := q.LockDisputeFunds(ctx, reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil || f.Status != fundsWithdrawn {
		return 0, err
	}
	merchantID, err := id.Parse(f.MerchantID)
	if err != nil {
		return 0, err
	}
	owner := Owner{Merchant: merchantID, Livemode: f.Livemode}
	currency := mustCurrency(f.Currency)
	amount, err := money.New(f.Amount, currency)
	if err != nil {
		return 0, err
	}
	accts, err := s.ledgerAccounts(ctx, tx, owner, currency)
	if err != nil {
		return 0, err
	}
	source := accts.networkReceivable
	attempt, err := q.GetAttempt(ctx, f.AttemptID)
	if err != nil {
		return 0, err
	}
	if attempt.PaymentMethod == PaymentMethodPix {
		pix, err := s.pixAccounts(ctx, tx, f.Livemode, currency)
		if err != nil {
			return 0, err
		}
		source = pix.settlement
	}
	if _, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "dispute " + reference + " reinstated",
		Legs:        []ledger.Leg{ledger.Debit(source, amount), ledger.Credit(accts.merchantBalance, amount)},
	}); err != nil {
		return 0, fmt.Errorf("reinstating a dispute: %w", err)
	}
	if f.SplitBack > 0 && s.cfg.Receivables != nil {
		intentID, err := IntentPrefix.Parse(f.IntentID)
		if err != nil {
			return 0, err
		}
		back, _ := money.New(f.SplitBack, currency)
		if _, err := s.cfg.Receivables.DisputeReinstated(ctx, tx, CardDispute{
			Owner: owner, Intent: intentID, Attempt: f.AttemptID, Reference: reference, Amount: back, Balance: accts.merchantBalance,
		}); err != nil {
			return 0, err
		}
	}
	return f.Amount, q.SaveDisputeFunds(ctx, db.SaveDisputeFundsParams{
		Reference: reference, Status: fundsReinstated, SplitBack: f.SplitBack, LedgerTxn: f.LedgerTxn, Now: ts(s.cfg.Now().UTC()),
	})
}

// PaidOutSince lists the merchant's payouts paid since a moment: where money it held
// went, which a MED claim traces.
func (s *Service) PaidOutSince(ctx context.Context, q db.DBTX, owner Owner, currency money.Currency, since time.Time) ([]Payout, error) {
	rows, err := db.New(q).PaidOutSince(ctx, db.PaidOutSinceParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Currency: currency.Code(), Since: ts(since),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Payout, 0, len(rows))
	for _, r := range rows {
		payoutID, err := PayoutPrefix.Parse(r.ID)
		if err != nil {
			return nil, err
		}
		amount, err := money.New(r.Amount, currency)
		if err != nil {
			return nil, err
		}
		out = append(out, Payout{ID: payoutID, Owner: owner, Amount: amount, EndToEndID: r.E2eID, Destination: PayoutDestination{Method: r.Method}, CreatedAt: r.CreatedAt.Time})
	}
	return out, nil
}

// CardPaymentsBetween counts a merchant's card payments captured, by when they were
// authorized, in [from, to).
func (s *Service) CardPaymentsBetween(ctx context.Context, q db.DBTX, owner Owner, from, to time.Time) (int64, error) {
	return db.New(q).CardAttemptsBetween(ctx, db.CardAttemptsBetweenParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		FromTime: pgtype.Timestamptz{Time: from, Valid: true}, ToTime: pgtype.Timestamptz{Time: to, Valid: true},
	})
}

// DisputeFunds is what payments recorded a dispute took, by the dispute's id.
type DisputeFunds struct {
	Status string
	Amount int64
}

// AllDisputeFunds lists what every dispute took, for the disputes check.
func (s *Service) AllDisputeFunds(ctx context.Context, q db.DBTX) (map[string]DisputeFunds, error) {
	rows, err := db.New(q).AllDisputeFunds(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]DisputeFunds, len(rows))
	for _, r := range rows {
		out[r.Reference] = DisputeFunds{Status: r.Status, Amount: r.Amount}
	}
	return out, nil
}
