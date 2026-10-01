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

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

// PaymentMethodBoleto is a boleto: registered at Jupiter's bank, paid by its typed line
// or, being hybrid, by the Pix QR code on it, and reported paid in the bank's return file.
const PaymentMethodBoleto = "boleto"

// BoletoOptions are a boleto's terms: when it is due, how many days after it the bank
// still takes it before writing it off, who pays it, and whether it carries a Pix QR code
// (hybrid; it does unless Pix is false).
type BoletoOptions struct {
	DueDate      string      `json:"due_date"`
	DaysAfterDue int         `json:"days_after_due"`
	Payer        BoletoPayer `json:"payer"`
	Pix          *bool       `json:"pix,omitempty"`
}

type BoletoPayer struct {
	Name  string `json:"name"`
	TaxID string `json:"tax_id"`
}

// Hybrid reports whether the boleto carries a Pix QR code.
func (o BoletoOptions) Hybrid() bool { return o.Pix == nil || *o.Pix }

const (
	maxBoletoDays      = 60
	maxBoletoAhead     = 365
	maxBoletoPayerName = 40
	// maxBoletoAmount is the most a barcode's ten digits of centavos hold.
	maxBoletoAmount = 99_999_999_99
)

// BoletoIssue is a boleto for the bank to register.
type BoletoIssue struct {
	Attempt      string
	Livemode     bool
	Amount       money.Amount
	Due          time.Time
	DaysAfterDue int
	Payer        BoletoPayer
	Hybrid       bool
	Description  string
}

// BoletoTitle is a boleto as issued: its nosso número at the bank, its barcode and typed
// line, and, once the bank registered it, its Pix QR code.
type BoletoTitle struct {
	OurNumber string `json:"our_number"`
	Barcode   string `json:"barcode"`
	Line      string `json:"line"`
	DueDate   string `json:"due_date"`
	PixCode   string `json:"pix_code,omitempty"`
}

// BoletoRail is Jupiter's bank for boletos. Issue numbers a boleto and queues it for the
// bank to register; WriteOff asks the bank to cancel it. Both are named by the attempt and
// may be repeated.
type BoletoRail interface {
	Issue(ctx context.Context, b BoletoIssue) (BoletoTitle, error)
	WriteOff(ctx context.Context, attemptID string) error
}

func (s *Service) boletoRail(livemode bool) (BoletoRail, error) {
	r := s.cfg.TestBoleto
	if livemode {
		r = s.cfg.LiveBoleto
	}
	if r == nil {
		return nil, fmt.Errorf("%w: no boleto bank in this mode", ErrRailUnavailable)
	}
	return r, nil
}

func validateBoleto(o *BoletoOptions, amount money.Amount, method CaptureMethod, installments *Installments, setup string, now time.Time) error {
	if o == nil {
		return fmt.Errorf("%w: a boleto needs boleto[due_date] and boleto[payer]", ErrInvalid)
	}
	today := now.In(brasilia).Format(time.DateOnly)
	due, err := time.Parse(time.DateOnly, o.DueDate)
	switch {
	case err != nil || o.DueDate < today || due.After(now.AddDate(0, 0, maxBoletoAhead)):
		return fmt.Errorf("%w: boleto[due_date] is a date from today to a year from now", ErrInvalid)
	case o.DaysAfterDue < 0 || o.DaysAfterDue > maxBoletoDays:
		return fmt.Errorf("%w: boleto[days_after_due] is 0 to %d", ErrInvalid, maxBoletoDays)
	case strings.TrimSpace(o.Payer.Name) == "" || len(o.Payer.Name) > maxBoletoPayerName || !printableASCII(o.Payer.Name):
		return fmt.Errorf("%w: boleto[payer][name] is up to %d characters, without accents", ErrInvalid, maxBoletoPayerName)
	case !taxid.Valid(o.Payer.TaxID):
		return fmt.Errorf("%w: boleto[payer][tax_id] is not a CPF or CNPJ", ErrInvalid)
	case amount.Currency() != money.BRL || amount.Minor() > maxBoletoAmount:
		return fmt.Errorf("%w: a boleto is in BRL, up to R$ 99,999,999.99", ErrInvalid)
	case method != CaptureAutomatic || installments != nil || setup != "":
		return fmt.Errorf("%w: a boleto is captured when paid, in one go, and stores nothing", ErrInvalid)
	}
	return nil
}

func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// checkBoleto applies the boleto's rules to an intent paid, or to be paid, by boleto;
// boleto options on an intent paid otherwise are refused.
func (s *Service) checkBoleto(pm string, o *BoletoOptions, amount money.Amount, method CaptureMethod, installments *Installments, setup string) error {
	if pm != PaymentMethodBoleto {
		if o != nil {
			return fmt.Errorf("%w: boleto options are for the payment method boleto", ErrInvalid)
		}
		return nil
	}
	return validateBoleto(o, amount, method, installments, setup, s.cfg.Now())
}

func boletoColumn(o *BoletoOptions) []byte {
	if o == nil {
		return nil
	}
	raw, _ := json.Marshal(o)
	return raw
}

func boletoOf(row db.PaymentsIntent) (*BoletoOptions, error) {
	if row.BoletoOptions == nil {
		return nil, nil //nolint:nilnil // not paid by boleto
	}
	var o BoletoOptions
	if err := json.Unmarshal(row.BoletoOptions, &o); err != nil {
		return nil, fmt.Errorf("payments: the boleto options of %s: %w", row.ID, err)
	}
	return &o, nil
}

// chargeBoleto issues the attempt's boleto. Issuing is named by the attempt, so a repeat
// answers the same boleto.
func (s *Service) chargeBoleto(ctx context.Context, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (Result, error) {
	rail, err := s.boletoRail(intent.Livemode)
	if err != nil {
		return Result{}, err
	}
	o, err := boletoOf(intent)
	if err != nil || o == nil {
		return Result{}, fmt.Errorf("payments: %s is a boleto without its options: %w", intent.ID, err)
	}
	due, err := time.Parse(time.DateOnly, o.DueDate)
	if err != nil {
		return Result{}, err
	}
	amount, err := money.New(attempt.Amount, mustCurrency(intent.Currency))
	if err != nil {
		return Result{}, err
	}
	title, err := rail.Issue(ctx, BoletoIssue{
		Attempt: attempt.ID, Livemode: intent.Livemode, Amount: amount, Due: due, DaysAfterDue: o.DaysAfterDue, Payer: o.Payer,
		Hybrid: o.Hybrid(), Description: intent.Description,
	})
	if err != nil {
		return Result{Outcome: Unknown}, nil //nolint:nilerr // issuing is repeated until it answers
	}
	return Result{Outcome: ActionRequired, Reference: title.OurNumber, Boleto: &title}, nil
}

// awaitBoleto shows the customer the boleto to pay.
func (s *Service) awaitBoleto(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt, title BoletoTitle) error {
	raw, err := json.Marshal(title)
	if err != nil {
		return err
	}
	attempt.Status, attempt.RailReference = string(attemptRequiresAction), title.OurNumber
	attempt.UnknownSince = pgtype.Timestamptz{}
	row.NextAction, row.NextActionData = "boleto_display_details", string(raw)
	return s.setStatus(ctx, tx, row, RequiresAction)
}

// BoletoEvent is what the bank's return file says happened to a boleto.
type BoletoEvent struct {
	Attempt  string
	Livemode bool
	Kind     BoletoEventKind
	// PixCode is a registered hybrid boleto's Pix QR code.
	PixCode string
	// Paid is the amount paid, on a payment; Channel how (pix, or the bank's own).
	Paid    int64
	Channel string
	// Reason is the bank's, on a rejection or a write-off.
	Reason string
}

type BoletoEventKind string

const (
	BoletoRegistered BoletoEventKind = "registered"
	BoletoRejected   BoletoEventKind = "rejected"
	BoletoPaid       BoletoEventKind = "paid"
	BoletoWrittenOff BoletoEventKind = "written_off"
)

// ErrNotWaiting says an event came for an attempt that no longer waits for it: one the
// bank reports again, or late.
var ErrNotWaiting = errors.New("payments: the attempt does not wait for this")

// ApplyBoletoEvent moves a boleto's payment intent as its bank reports:
//   - registered, the QR code of a hybrid boleto joins what the customer is shown;
//   - rejected, the attempt fails;
//   - paid, the amount is posted and the intent succeeds, even if it was being canceled;
//   - written off, the intent is canceled if that was asked, or else fails, expired.
func (s *Service) ApplyBoletoEvent(ctx context.Context, tx pgx.Tx, e BoletoEvent) error {
	q := db.New(tx)
	attempt, err := q.LockAttempt(ctx, e.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: attempt %s", ErrNotFound, e.Attempt)
	}
	if err != nil {
		return err
	}
	row, err := q.LockIntentByID(ctx, attempt.IntentID)
	if err != nil {
		return err
	}
	if row.Livemode != e.Livemode || attempt.PaymentMethod != PaymentMethodBoleto || !row.LatestAttempt.Valid || row.LatestAttempt.String != attempt.ID {
		return fmt.Errorf("%w: %s is not a waiting boleto", ErrNotWaiting, e.Attempt)
	}
	waiting := inStatus(attempt, attemptRequiresAction, attemptVoiding, attemptVoidUnknown)
	if !waiting {
		return fmt.Errorf("%w: %s is %s", ErrNotWaiting, e.Attempt, attempt.Status)
	}
	switch e.Kind {
	case BoletoRegistered:
		err = s.boletoRegistered(&row, e)
	case BoletoRejected:
		if inStatus(attempt, attemptVoiding, attemptVoidUnknown) {
			// Never registered, so there is nothing left to cancel.
			err = s.boletoWrittenOff(ctx, tx, &row, &attempt)
			break
		}
		attempt.Status, attempt.DeclineCode = string(attemptFailed), "boleto_rejected"
		err = s.fail(ctx, tx, &row, "payment_intent_payment_attempt_failed", "boleto_rejected", "The bank did not register the boleto: "+e.Reason+".")
	case BoletoPaid:
		err = s.boletoPaid(ctx, tx, &row, &attempt, e)
	case BoletoWrittenOff:
		err = s.boletoWrittenOff(ctx, tx, &row, &attempt)
	default:
		return fmt.Errorf("%w: boleto event %q", ErrInvalid, e.Kind)
	}
	if err != nil {
		return err
	}
	attempt.UpdatedAt = ts(s.cfg.Now().UTC())
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return err
	}
	_, err = s.save(ctx, q, row)
	return err
}

func (s *Service) boletoRegistered(row *db.PaymentsIntent, e BoletoEvent) error {
	if e.PixCode == "" || row.NextAction != "boleto_display_details" {
		return nil
	}
	var title BoletoTitle
	if err := json.Unmarshal([]byte(row.NextActionData), &title); err != nil {
		return err
	}
	title.PixCode = e.PixCode
	raw, err := json.Marshal(title)
	row.NextActionData = string(raw)
	return err
}

// boletoPaid posts what was paid: from Jupiter's account at the bank to the merchant.
func (s *Service) boletoPaid(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt, e BoletoEvent) error {
	owner, err := ownerOf(*row)
	if err != nil {
		return err
	}
	currency := mustCurrency(row.Currency)
	// Jupiter's boletos carry no interest, fine or discount: paid is paid in full.
	amount, err := money.New(e.Paid, currency)
	if err != nil || e.Paid != attempt.Amount {
		return fmt.Errorf("%w: a boleto of %d paid %d", ErrInvalid, attempt.Amount, e.Paid)
	}
	accts, err := s.ledgerAccounts(ctx, tx, owner, currency)
	if err != nil {
		return err
	}
	bank, err := s.bankAccount(ctx, tx, row.Livemode, currency)
	if err != nil {
		return err
	}
	if _, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "boleto " + attempt.RailReference + " paid",
		Legs:        []ledger.Leg{ledger.Debit(bank, amount), ledger.Credit(accts.merchantBalance, amount)},
	}); err != nil {
		return fmt.Errorf("posting the boleto: %w", err)
	}
	attempt.Status, attempt.AmountCaptured = string(attemptCaptured), e.Paid
	row.AmountReceived, row.NextAction, row.NextActionData = e.Paid, "", ""
	return s.setStatus(ctx, tx, row, Succeeded)
}

func (s *Service) boletoWrittenOff(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt) error {
	if inStatus(*attempt, attemptVoiding, attemptVoidUnknown) {
		attempt.Status, attempt.UnknownSince = string(attemptVoided), pgtype.Timestamptz{}
		row.NextAction, row.NextActionData = "", ""
		return s.setStatus(ctx, tx, row, Canceled)
	}
	attempt.Status, attempt.DeclineCode = string(attemptFailed), "boleto_expired"
	return s.fail(ctx, tx, row, "payment_intent_payment_attempt_failed", "boleto_expired", "The boleto was not paid by the end of its term.")
}

// writeOffBoleto asks the bank to cancel the attempt's boleto. The cancellation ends when
// the bank's return reports it written off, or paid.
func (s *Service) writeOffBoleto(ctx context.Context, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (Result, error) {
	rail, err := s.boletoRail(intent.Livemode)
	if err != nil {
		return Result{}, err
	}
	if err := rail.WriteOff(ctx, attempt.ID); err != nil {
		return Result{}, err
	}
	return Result{Outcome: Unknown}, nil
}

// checkBoletoRow checks an intent's boleto options as they are now.
func (s *Service) checkBoletoRow(row db.PaymentsIntent) error {
	o, err := boletoOf(row)
	if err != nil {
		return err
	}
	return s.checkBoleto(row.PaymentMethod, o, mustAmount(row), CaptureMethod(row.CaptureMethod), installmentsOf(row), row.SetupFutureUsage)
}

// resolveBoleto issues again a boleto whose issuing went unanswered, and asks the bank
// again to write off one being canceled: the bank's return ends the cancellation.
func (s *Service) resolveBoleto(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (bool, error) {
	if inStatus(attempt, attemptVoiding, attemptVoidUnknown) {
		if _, err := s.writeOffBoleto(ctx, intent, attempt); err != nil {
			return false, err
		}
		return false, postgres.InTx(ctx, pool, func(tx pgx.Tx) error { return s.countResolution(ctx, db.New(tx), attempt.ID) })
	}
	res, err := s.chargeBoleto(ctx, intent, attempt)
	if err != nil || res.Outcome == Unknown {
		return false, err
	}
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := s.FinishAuthorization(ctx, tx, owner, intentID, res)
		return err
	})
	return err == nil, err
}
