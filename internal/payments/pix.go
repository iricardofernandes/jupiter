package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// Pix: a payment intent confirmed with the payment method "pix" becomes a charge at
// Jupiter's bank and waits, in requires_action, for the customer to pay its BR Code.
// The bank tells Jupiter of each Pix received (ReceivePix); one that pays the charge of
// the intent's current attempt settles it, and any other is held apart and returned to
// the payer. Refunds are returns (devoluções) of the Pix that paid.

const PaymentMethodPix = "pix"

// PixRail is Jupiter's bank, through the API Pix. Errors wrapping ErrPixRefused are the
// bank's answer; any other error means the outcome is unknown.
type PixRail interface {
	// CreateCharge creates the charge named TxID; asked again, it answers with the
	// charge already made.
	CreateCharge(ctx context.Context, r PixChargeRequest) (PixCharge, error)
	// Charge reads a charge, and the Pix that paid it. It wraps ErrPixNotFound for a
	// charge the bank does not have.
	Charge(ctx context.Context, txid string, due bool) (PixCharge, error)
	// RemoveCharge removes an unpaid charge, so it can no longer be paid; one already
	// paid is returned as it is.
	RemoveCharge(ctx context.Context, txid string, due bool) (PixCharge, error)
	// Payment reads a Pix received.
	Payment(ctx context.Context, e2eID string) (PixPayment, error)
	// Return asks the bank to return part or all of a Pix received; asked again with the
	// same id, it answers with the return already requested.
	Return(ctx context.Context, r PixReturnRequest) (PixReturn, error)
	// ReturnStatus reads a return; it wraps ErrPixNotFound for one never requested.
	ReturnStatus(ctx context.Context, e2eID, id string) (PixReturn, error)
	// Transfer sends a Pix to a key; asked again with the same id, it answers with the
	// transfer already made.
	Transfer(ctx context.Context, r PixTransferRequest) (PixTransfer, error)
	// TransferStatus reads a transfer; it wraps ErrPixNotFound for one never sent.
	TransferStatus(ctx context.Context, id string) (PixTransfer, error)
}

var (
	ErrPixRefused  = errors.New("payments: the bank refused the Pix operation")
	ErrPixNotFound = errors.New("payments: the bank has no such Pix record")
)

type PixChargeRequest struct {
	TxID        string
	Amount      money.Amount
	Description string
	// ExpiresAfter is how long an immediate charge (cob) can be paid; Due makes a charge
	// with a due date (cobv) instead.
	ExpiresAfter time.Duration
	Due          *PixDue
}

// Charge statuses, as the API Pix names them.
const (
	PixChargeActive    = "ATIVA"
	PixChargeCompleted = "CONCLUIDA"
)

type PixCharge struct {
	TxID      string
	Status    string
	CopyPaste string
	ExpiresAt time.Time
	// Payments are the endToEndIds of the Pix that paid the charge.
	Payments []string
}

type PixPayment struct {
	E2EID      string
	TxID       string
	Amount     money.Amount
	ReceivedAt time.Time
	Returns    []PixReturn
}

type PixReturnRequest struct {
	E2EID  string
	ID     string
	Amount money.Amount
}

// Return statuses.
const (
	PixReturnProcessing = "processing"
	PixReturnReturned   = "returned"
	PixReturnFailed     = "failed"
)

type PixReturn struct {
	ID     string
	Status string
	Amount money.Amount
	Reason string
}

type PixTransferRequest struct {
	ID          string
	Amount      money.Amount
	Key         string
	Description string
}

type PixTransfer struct {
	ID        string
	E2EID     string
	Status    string // one of the PixTransfer* constants
	Reason    string
	Recipient string
	SettledAt time.Time
}

const (
	PixTransferProcessing = "processing"
	PixTransferPaid       = "paid"
	PixTransferFailed     = "failed"
)

// PixOptions says how an intent's Pix charge is made: immediate, payable for
// ExpiresAfterSeconds, or, with Due, a charge with a due date.
type PixOptions struct {
	ExpiresAfterSeconds int64   `json:"expires_after_seconds,omitempty"`
	Due                 *PixDue `json:"due,omitempty"`
}

// PixDue is a charge with a due date (cobv). Percentages are in hundredths of a percent
// (200 is 2%); amounts in centavos. A fine is fixed or a percentage, never both.
type PixDue struct {
	Date                   string `json:"date"`
	DaysAfter              int32  `json:"days_after"`
	PayerName              string `json:"payer_name"`
	PayerTaxID             string `json:"payer_tax_id"`
	FineAmount             int64  `json:"fine_amount,omitempty"`
	FinePercent            int64  `json:"fine_percent,omitempty"`
	InterestMonthlyPercent int64  `json:"interest_monthly_percent,omitempty"`
	DiscountAmount         int64  `json:"discount_amount,omitempty"`
	DiscountUntil          string `json:"discount_until,omitempty"`
}

const (
	defaultPixExpiry  = 24 * time.Hour
	minPixExpiry      = time.Minute
	maxPixExpiry      = 14 * 24 * time.Hour
	defaultDaysAfter  = 30
	maxDaysAfter      = 365
	maxPercent        = 10000
	pixExpiryGrace    = 5 * time.Minute
	pixDescriptionMax = 140
)

// brasilia is the time zone of due dates. Brazil has kept no summer time since 2019.
var brasilia = time.FixedZone("BRT", -3*60*60)

func validatePix(o *PixOptions, amount money.Amount, method CaptureMethod, installments *Installments, setup string, now time.Time) error {
	switch {
	case amount.Currency() != money.BRL:
		return fmt.Errorf("%w: Pix is only for payments in BRL", ErrInvalid)
	case method != CaptureAutomatic:
		return fmt.Errorf("%w: a Pix payment is captured when it is paid; capture_method must be automatic", ErrInvalid)
	case installments != nil:
		return fmt.Errorf("%w: a Pix payment has no installments", ErrInvalid)
	case setup != "":
		return fmt.Errorf("%w: a Pix payment stores nothing for later; setup_future_usage does not apply", ErrInvalid)
	case o == nil:
		return nil
	case o.Due != nil && o.ExpiresAfterSeconds != 0:
		return fmt.Errorf("%w: pix[expires_after_seconds] is for immediate charges, not ones with a due date", ErrInvalid)
	case o.Due == nil && o.ExpiresAfterSeconds != 0 &&
		(o.ExpiresAfterSeconds < int64(minPixExpiry.Seconds()) || o.ExpiresAfterSeconds > int64(maxPixExpiry.Seconds())):
		return fmt.Errorf("%w: pix[expires_after_seconds] must be between %d and %d", ErrInvalid, int(minPixExpiry.Seconds()), int(maxPixExpiry.Seconds()))
	case o.Due != nil:
		return validateDue(o.Due, amount, now)
	}
	return nil
}

func validateDue(d *PixDue, amount money.Amount, now time.Time) error {
	today := now.In(brasilia).Format(time.DateOnly)
	due, err := time.Parse(time.DateOnly, d.Date)
	switch {
	case err != nil || d.Date < today:
		return fmt.Errorf("%w: pix[due_date] must be a date (YYYY-MM-DD) from today on", ErrInvalid)
	case due.After(now.AddDate(1, 0, 0)):
		return fmt.Errorf("%w: pix[due_date] must be within a year", ErrInvalid)
	case d.DaysAfter < 0 || d.DaysAfter > maxDaysAfter:
		return fmt.Errorf("%w: pix[days_after_due] must be between 0 and %d", ErrInvalid, maxDaysAfter)
	case d.PayerName == "" || len([]rune(d.PayerName)) > 200:
		return fmt.Errorf("%w: pix[payer][name] is required for a charge with a due date", ErrInvalid)
	case !ValidTaxID(d.PayerTaxID):
		return fmt.Errorf("%w: pix[payer][tax_id] must be a valid CPF or CNPJ", ErrInvalid)
	case d.FineAmount != 0 && d.FinePercent != 0:
		return fmt.Errorf("%w: pix[fine] is an amount or a percent, not both", ErrInvalid)
	case d.FineAmount < 0 || d.FineAmount > amount.Minor() || d.FinePercent < 0 || d.FinePercent > maxPercent ||
		d.InterestMonthlyPercent < 0 || d.InterestMonthlyPercent > maxPercent:
		return fmt.Errorf("%w: pix[fine] and pix[interest] must be positive, a fine at most the amount, and percentages at most 100", ErrInvalid)
	case d.DiscountAmount < 0 || d.DiscountAmount >= amount.Minor():
		return fmt.Errorf("%w: pix[discount][amount] must be less than the amount", ErrInvalid)
	case (d.DiscountAmount > 0) != (d.DiscountUntil != ""):
		return fmt.Errorf("%w: pix[discount] needs both an amount and a date", ErrInvalid)
	case d.DiscountUntil != "" && (d.DiscountUntil < today || d.DiscountUntil > d.Date):
		return fmt.Errorf("%w: pix[discount][until] must be from today to the due date", ErrInvalid)
	}
	if d.DiscountUntil != "" {
		if _, err := time.Parse(time.DateOnly, d.DiscountUntil); err != nil {
			return fmt.Errorf("%w: pix[discount][until] must be a date (YYYY-MM-DD)", ErrInvalid)
		}
	}
	return nil
}

func pixOptionsColumn(o *PixOptions) []byte {
	if o == nil {
		return nil
	}
	raw, _ := json.Marshal(o)
	return raw
}

func pixOptionsOf(row db.PaymentsIntent) (*PixOptions, error) {
	if row.PixOptions == nil {
		return nil, nil //nolint:nilnil // no options: an immediate charge with the defaults
	}
	var o PixOptions
	if err := json.Unmarshal(row.PixOptions, &o); err != nil {
		return nil, fmt.Errorf("payments: pix options of %s: %w", row.ID, err)
	}
	return &o, nil
}

// bankID names at the bank what Jupiter names id: a charge's txid is its attempt's id, a
// return's id its refund's, a transfer's its payout's, without the underscore: 28 letters
// and digits, within what the API Pix allows.
func bankID(id string) string { return strings.ReplaceAll(id, "_", "") }

// PixTxID is the txid of an attempt's charge.
func PixTxID(attemptID string) string { return bankID(attemptID) }

// unmatchedReturnID names the return of a Pix that paid nothing Jupiter wanted.
func unmatchedReturnID(e2eID string) string { return "rt" + e2eID }

func (s *Service) pixRail(livemode bool) (PixRail, error) {
	r := s.cfg.TestPix
	if livemode {
		r = s.cfg.LivePix
	}
	if r == nil {
		return nil, fmt.Errorf("%w: Pix is not available in this mode", ErrRailUnavailable)
	}
	return r, nil
}

// expiresAt is when a charge stops being payable: the end of the last day it is valid,
// in Brasília, for one with a due date.
func pixExpiry(o *PixOptions, created time.Time) time.Time {
	if o != nil && o.Due != nil {
		due, _ := time.ParseInLocation(time.DateOnly, o.Due.Date, brasilia)
		return due.AddDate(0, 0, int(o.Due.DaysAfter)+1)
	}
	return created.Add(pixExpiryOf(o))
}

func pixExpiryOf(o *PixOptions) time.Duration {
	if o == nil || o.ExpiresAfterSeconds == 0 {
		return defaultPixExpiry
	}
	return time.Duration(o.ExpiresAfterSeconds) * time.Second
}

// chargePix creates the attempt's charge at the bank. It runs outside any transaction
// and may be repeated: the charge is named by the attempt.
func (s *Service) chargePix(ctx context.Context, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (Result, error) {
	rail, err := s.pixRail(intent.Livemode)
	if err != nil {
		return Result{}, err
	}
	opts, err := pixOptionsOf(intent)
	if err != nil {
		return Result{}, err
	}
	amount, err := money.New(attempt.Amount, mustCurrency(intent.Currency))
	if err != nil {
		return Result{}, err
	}
	req := PixChargeRequest{TxID: PixTxID(attempt.ID), Amount: amount, ExpiresAfter: pixExpiryOf(opts)}
	if opts != nil {
		req.Due = opts.Due
	}
	if d := []rune(intent.Description); len(d) > pixDescriptionMax {
		req.Description = string(d[:pixDescriptionMax])
	} else {
		req.Description = intent.Description
	}
	charge, err := rail.CreateCharge(ctx, req)
	return pixChargeResult(charge, err), nil
}

func pixChargeResult(charge PixCharge, err error) Result {
	switch {
	case errors.Is(err, ErrPixRefused):
		return Result{Outcome: Declined, DeclineCode: "pix_charge_refused"}
	case err != nil:
		return Result{Outcome: Unknown}
	case charge.Status != PixChargeActive && charge.Status != PixChargeCompleted:
		// Removed at the bank: nobody can pay it.
		return Result{Outcome: Declined, DeclineCode: "pix_charge_removed"}
	}
	return Result{Outcome: ActionRequired, Reference: charge.TxID, Pix: &charge}
}

// awaitPix records the charge and shows its BR Code to the customer.
func (s *Service) awaitPix(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt, charge PixCharge) error {
	opts, err := pixOptionsOf(*row)
	if err != nil {
		return err
	}
	expires := charge.ExpiresAt
	if expires.IsZero() {
		expires = pixExpiry(opts, s.cfg.Now().UTC())
	}
	if err := db.New(tx).InsertPixCharge(ctx, db.InsertPixChargeParams{
		Txid: charge.TxID, AttemptID: attempt.ID, Livemode: row.Livemode, Due: opts != nil && opts.Due != nil,
		CopyPaste: charge.CopyPaste, ExpiresAt: ts(expires), CreatedAt: ts(s.cfg.Now().UTC()),
	}); err != nil {
		return err
	}
	attempt.Status, attempt.RailReference = string(attemptRequiresAction), charge.TxID
	attempt.UnknownSince = pgtype.Timestamptz{}
	row.NextAction, row.NextActionData, row.NextActionExpiresAt = "pix_display_qr_code", charge.CopyPaste, ts(expires)
	return s.setStatus(ctx, tx, row, RequiresAction)
}

// ReceivePix applies a Pix the bank says Jupiter received, once: to the attempt whose
// charge it paid, if that attempt is still the one its intent waits on, and otherwise
// apart, to be returned. Returns of it the bank reports are applied too.
func (s *Service) ReceivePix(ctx context.Context, pool *pgxpool.Pool, livemode bool, p PixPayment) error {
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error { return s.receivePix(ctx, tx, livemode, p) })
	if err != nil {
		return fmt.Errorf("receiving Pix %s: %w", p.E2EID, err)
	}
	for _, r := range p.Returns {
		if err := s.ApplyPixReturn(ctx, pool, livemode, p.E2EID, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) receivePix(ctx context.Context, tx pgx.Tx, livemode bool, p PixPayment) error {
	q := db.New(tx)
	now := s.cfg.Now().UTC()
	rec, err := q.InsertPixReceived(ctx, db.InsertPixReceivedParams{
		Livemode: livemode, E2eID: p.E2EID, Txid: p.TxID, Amount: p.Amount.Minor(), Currency: p.Amount.Currency().Code(),
		ReceivedAt: ts(p.ReceivedAt), Now: ts(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // received already
	}
	if err != nil {
		return err
	}
	reason, err := s.settleWithPix(ctx, tx, livemode, p)
	if err != nil {
		return err
	}
	if reason == "" {
		return nil
	}
	accts, err := s.pixAccounts(ctx, tx, livemode, p.Amount.Currency())
	if err != nil {
		return err
	}
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "Pix " + p.E2EID + " held to be returned",
		Legs:        []ledger.Leg{ledger.Debit(accts.settlement, p.Amount), ledger.Credit(accts.unmatched, p.Amount)},
	})
	if err != nil {
		return fmt.Errorf("posting an unmatched Pix: %w", err)
	}
	return q.SavePixReceived(ctx, db.SavePixReceivedParams{
		Livemode: livemode, E2eID: rec.E2eID, Status: "unmatched", LedgerTxn: text(txn.ID.String()), Reason: reason, UpdatedAt: ts(now),
	})
}

// settleWithPix pays the attempt the Pix's charge belongs to. It returns why it could
// not, when the Pix has no charge or its attempt no longer waits for it.
func (s *Service) settleWithPix(ctx context.Context, tx pgx.Tx, livemode bool, p PixPayment) (string, error) {
	q := db.New(tx)
	if p.TxID == "" {
		return "the Pix pays no charge", nil
	}
	attemptID, err := s.attemptOfCharge(ctx, q, livemode, p.TxID)
	if err != nil || attemptID == "" {
		return "no charge " + p.TxID, err
	}
	intent, attempt, reason, err := s.waitingAttempt(ctx, q, attemptID, p)
	if err != nil || reason != "" {
		return reason, err
	}
	txn, err := s.postPix(ctx, tx, livemode, intent, attempt, p)
	if err != nil {
		return "", err
	}
	now := s.cfg.Now().UTC()
	attempt.Status, attempt.RailReference, attempt.NetworkTransactionID = string(attemptCaptured), p.TxID, p.E2EID
	attempt.CaptureAmount = pgtype.Int8{Int64: p.Amount.Minor(), Valid: true}
	attempt.AmountCaptured, attempt.UnknownSince, attempt.UpdatedAt = p.Amount.Minor(), pgtype.Timestamptz{}, ts(now)
	intent.AmountReceived, intent.CancellationReason = p.Amount.Minor(), ""
	intent.NextAction, intent.NextActionData, intent.NextActionExpiresAt = "", "", pgtype.Timestamptz{}
	if err := s.setStatus(ctx, tx, &intent, Succeeded); err != nil {
		return "", err
	}
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return "", err
	}
	if _, err := s.save(ctx, q, intent); err != nil {
		return "", err
	}
	return "", q.SavePixReceived(ctx, db.SavePixReceivedParams{
		Livemode: livemode, E2eID: p.E2EID, Status: "applied", AttemptID: text(attempt.ID), LedgerTxn: text(txn.ID.String()), UpdatedAt: ts(now),
	})
}

// attemptOfCharge finds the attempt a txid names: from the charge recorded for it, or,
// when the charge's creation is still unanswered and so unrecorded, from the txid itself,
// which is the attempt's id without its underscore. "" when the txid is not Jupiter's.
func (s *Service) attemptOfCharge(ctx context.Context, q *db.Queries, livemode bool, txid string) (string, error) {
	charge, err := q.PixChargeByTxid(ctx, db.PixChargeByTxidParams{Txid: txid, Livemode: livemode})
	if err == nil {
		return charge.AttemptID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	rest, ok := strings.CutPrefix(txid, string(AttemptPrefix))
	if !ok {
		return "", nil
	}
	attemptID, err := AttemptPrefix.Parse(string(AttemptPrefix) + "_" + rest)
	if err != nil {
		return "", nil //nolint:nilerr // a txid that names no attempt is not Jupiter's
	}
	attempt, err := q.GetAttempt(ctx, attemptID.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	intent, err := q.GetIntentByID(ctx, attempt.IntentID)
	if err != nil || intent.Livemode != livemode || attempt.PaymentMethod != PaymentMethodPix {
		return "", err
	}
	return attempt.ID, nil
}

// waitingAttempt locks a charge's attempt and its intent, and says why a Pix cannot pay
// it when it no longer waits for one.
func (s *Service) waitingAttempt(ctx context.Context, q *db.Queries, attemptID string, p PixPayment) (db.PaymentsIntent, db.PaymentsAttempt, string, error) {
	found, err := q.GetAttempt(ctx, attemptID)
	if err != nil {
		return db.PaymentsIntent{}, db.PaymentsAttempt{}, "", err
	}
	intent, err := q.LockIntentByID(ctx, found.IntentID)
	if err != nil {
		return db.PaymentsIntent{}, db.PaymentsAttempt{}, "", err
	}
	attempt, err := q.LockAttempt(ctx, found.ID)
	if err != nil {
		return db.PaymentsIntent{}, db.PaymentsAttempt{}, "", err
	}
	waiting := inStatus(attempt, attemptRequiresAction, attemptAuthorizing, attemptAuthorizationUnknown, attemptVoiding, attemptVoidUnknown)
	switch {
	case !waiting || intent.LatestAttempt.String != attempt.ID || (Status(intent.Status) != RequiresAction && Status(intent.Status) != Processing):
		return intent, attempt, "the payment intent is " + intent.Status, nil
	case p.Amount.Currency() != mustCurrency(intent.Currency):
		return intent, attempt, "a Pix in another currency", nil
	}
	opts, err := pixOptionsOf(intent)
	if err != nil {
		return intent, attempt, "", err
	}
	if least, most := pixBounds(attempt.Amount, opts); p.Amount.Minor() < least || p.Amount.Minor() > most {
		return intent, attempt, fmt.Sprintf("a Pix of %d for a charge of %d to %d", p.Amount.Minor(), least, most), nil
	}
	return intent, attempt, "", nil
}

// pixBounds is what a Pix may pay for a charge of amount: exactly that, for an immediate
// charge; for one with a due date, from the amount less its discount to the amount plus
// its fine and the interest of every day it can be paid late, with a centavo to spare for
// rounding. The bank computes the amount; these bounds only keep a bank's mistake from
// paying for more, or less, than was asked.
func pixBounds(amount int64, o *PixOptions) (least, most int64) {
	if o == nil || o.Due == nil {
		return amount, amount
	}
	d := o.Due
	fine := d.FineAmount + (amount*d.FinePercent+9999)/10000
	interest := (amount*d.InterestMonthlyPercent*int64(d.DaysAfter+1) + 30*10000 - 1) / (30 * 10000)
	return amount - d.DiscountAmount - 1, amount + fine + interest + 1
}

// postPix posts a Pix to the merchant's balance.
func (s *Service) postPix(ctx context.Context, tx pgx.Tx, livemode bool, intent db.PaymentsIntent, attempt db.PaymentsAttempt, p PixPayment) (ledger.Transaction, error) {
	owner, err := ownerOf(intent)
	if err != nil {
		return ledger.Transaction{}, err
	}
	merchant, err := s.ledgerAccounts(ctx, tx, owner, p.Amount.Currency())
	if err != nil {
		return ledger.Transaction{}, err
	}
	accts, err := s.pixAccounts(ctx, tx, livemode, p.Amount.Currency())
	if err != nil {
		return ledger.Transaction{}, err
	}
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "Pix " + p.E2EID + " for " + attempt.ID,
		Legs:        []ledger.Leg{ledger.Debit(accts.settlement, p.Amount), ledger.Credit(merchant.merchantBalance, p.Amount)},
	})
	if err != nil {
		return ledger.Transaction{}, fmt.Errorf("posting a Pix: %w", err)
	}
	return txn, nil
}
