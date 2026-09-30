package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/subscriptions/db"
)

type Params struct {
	Amount        money.Amount
	Interval      Interval
	StartDate     string
	EndDate       string
	Description   string
	Customer      Customer
	Retries       bool
	Authorization Authorization
}

const (
	// maxDescription is what the recurrence's object (vinculo.objeto) holds.
	maxDescription = 35
	maxName        = 140
	// requestLifetime is how long the customer's bank shows a request before it expires.
	requestLifetime = 7 * 24 * time.Hour
	// setupGrace is how long a recurrence just asked for may take to be listed.
	setupGrace = 10 * time.Minute
	// maxLead is how far ahead a subscription may start.
	maxLead = 1
)

var (
	ispbPattern    = regexp.MustCompile(`^[0-9A-Z]{8}$`)
	accountPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
	branchPattern  = regexp.MustCompile(`^[0-9]{1,4}$`)
	intervals      = map[Interval]string{Week: "SEMANAL", Month: "MENSAL", Quarter: "TRIMESTRAL", HalfYear: "SEMESTRAL", Year: "ANUAL"}
)

func (s *Service) validate(p Params) error {
	today := s.today().Format(time.DateOnly)
	a := p.Authorization
	switch {
	case p.Amount.Currency() != money.BRL || !p.Amount.IsPositive():
		return fmt.Errorf("%w: a subscription is a positive amount in BRL", ErrInvalid)
	case intervals[p.Interval] == "":
		return fmt.Errorf("%w: interval must be week, month, quarter, half_year or year", ErrInvalid)
	case !validDate(p.StartDate) || p.StartDate < today || p.StartDate > s.today().AddDate(maxLead, 0, 0).Format(time.DateOnly):
		return fmt.Errorf("%w: start_date must be a date (YYYY-MM-DD) from today to a year from now", ErrInvalid)
	case p.EndDate != "" && (!validDate(p.EndDate) || p.EndDate < p.StartDate):
		return fmt.Errorf("%w: end_date must be a date on or after start_date", ErrInvalid)
	case p.Description == "" || len([]rune(p.Description)) > maxDescription:
		return fmt.Errorf("%w: description is required, up to %d characters: the customer's bank shows it to them", ErrInvalid, maxDescription)
	case p.Customer.Name == "" || len([]rune(p.Customer.Name)) > maxName:
		return fmt.Errorf("%w: customer[name] is required, up to %d characters", ErrInvalid, maxName)
	case !payments.ValidTaxID(p.Customer.TaxID):
		return fmt.Errorf("%w: customer[tax_id] must be a valid CPF or CNPJ", ErrInvalid)
	case a.Method != ByQRCode && a.Method != ByPayerRequest:
		return fmt.Errorf("%w: authorization[method] must be qr_code or payer_request", ErrInvalid)
	case a.Method == ByPayerRequest && (!ispbPattern.MatchString(a.PayerISPB) || !accountPattern.MatchString(a.PayerAccount) ||
		(a.PayerBranch != "" && !branchPattern.MatchString(a.PayerBranch))):
		return fmt.Errorf("%w: a payer request needs the customer's bank (an ISPB), account and branch", ErrInvalid)
	case a.Method == ByQRCode && (a.PayerISPB != "" || a.PayerAccount != "" || a.PayerBranch != ""):
		return fmt.Errorf("%w: the customer's bank account is only for a payer request", ErrInvalid)
	}
	return nil
}

func validDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// Create records a subscription, incomplete until the customer authorizes it. The bank
// is asked for the recurrence after (SetUp).
func (s *Service) Create(ctx context.Context, tx pgx.Tx, owner Owner, p Params) (Subscription, error) {
	if err := s.validate(p); err != nil {
		return Subscription{}, err
	}
	if _, err := s.bank(owner.Livemode); err != nil {
		return Subscription{}, err
	}
	// One authorization at a time per customer: a merchant cannot flood someone's bank
	// with requests.
	waiting, err := db.New(tx).IncompleteFor(ctx, db.IncompleteForParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, CustomerTaxID: p.Customer.TaxID,
	})
	if err != nil {
		return Subscription{}, err
	}
	if waiting > 0 {
		return Subscription{}, fmt.Errorf("%w: another subscription is waiting for this customer's authorization", ErrInvalidState)
	}
	var end pgtype.Date
	if p.EndDate != "" {
		end = date(p.EndDate)
	}
	row, err := db.New(tx).InsertSubscription(ctx, db.InsertSubscriptionParams{
		ID: Prefix.New().String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Amount: p.Amount.Minor(),
		Currency: p.Amount.Currency().Code(), Interval: string(p.Interval), StartDate: date(p.StartDate), EndDate: end,
		Description: p.Description, CustomerName: p.Customer.Name, CustomerTaxID: p.Customer.TaxID, Retries: p.Retries,
		AuthorizationMethod: p.Authorization.Method, PayerIspb: p.Authorization.PayerISPB, PayerBranch: p.Authorization.PayerBranch,
		PayerAccount: p.Authorization.PayerAccount, Now: ts(s.cfg.Now().UTC()),
	})
	if err != nil {
		return Subscription{}, fmt.Errorf("creating subscription: %w", err)
	}
	if err := s.publish(ctx, tx, owner, events.TypeSubscriptionCreated, row.ID); err != nil {
		return Subscription{}, err
	}
	return s.fromRow(ctx, tx, row)
}

// SetUp asks the bank for the subscription's recurrence and, as its authorization
// method needs, a QR code or a request to the customer's bank. It runs outside any
// transaction and may be repeated: a recurrence made by a request whose answer was lost
// is found by its contract, the subscription's id. A bank's refusal rejects the
// subscription.
func (s *Service) SetUp(ctx context.Context, pool *pgxpool.Pool, owner Owner, subscriptionID id.ID) (Recurrence, error) {
	var rec Recurrence
	err := s.exclusively(ctx, pool, subscriptionID.String(), func() error {
		var err error
		rec, err = s.setUp(ctx, pool, owner, subscriptionID)
		return err
	})
	return rec, err
}

func (s *Service) setUp(ctx context.Context, pool *pgxpool.Pool, owner Owner, subscriptionID id.ID) (Recurrence, error) {
	q := db.New(pool)
	row, err := q.GetSubscription(ctx, db.GetSubscriptionParams{ID: subscriptionID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Recurrence{}, notFoundOr(err, subscriptionID)
	}
	if row.SetUp || Status(row.Status) != Incomplete {
		return Recurrence{}, nil
	}
	bank, err := s.bank(row.Livemode)
	if err != nil {
		return Recurrence{}, err
	}
	rec, err := bank.FindRecurrence(ctx, row.ID, row.CustomerTaxID, row.CreatedAt.Time.Add(-time.Minute))
	if errors.Is(err, ErrBankNotFound) {
		// A recurrence asked for moments ago may not be listed yet: wait before asking
		// again, or the customer could be asked twice.
		if row.SetupAttemptedAt.Valid && s.cfg.Now().Before(row.SetupAttemptedAt.Time.Add(setupGrace)) {
			return Recurrence{}, nil
		}
		if err := q.MarkSetupAttempt(ctx, db.MarkSetupAttemptParams{ID: row.ID, At: ts(s.cfg.Now().UTC())}); err != nil {
			return Recurrence{}, err
		}
		rec, err = s.createRecurrence(ctx, bank, row)
	}
	if err == nil && row.AuthorizationMethod == ByPayerRequest && rec.Status == RecurrenceCreated && !rec.PendingRequest {
		rec, err = s.requestAuthorization(ctx, bank, row, rec)
	}
	if errors.Is(err, ErrBankRefused) {
		s.cfg.Logger.WarnContext(ctx, "the bank refused a subscription", "subscription", row.ID, "error", err)
		return Recurrence{}, s.reject(ctx, pool, owner, subscriptionID)
	}
	return rec, err
}

// requestAuthorization sends the customer's bank the request to authorize rec. One it
// cannot receive cancels rec, which nobody would authorize.
func (s *Service) requestAuthorization(ctx context.Context, bank Bank, row db.SubscriptionsSubscription, rec Recurrence) (Recurrence, error) {
	err := bank.RequestAuthorization(ctx, AuthorizationRequest{
		RecurrenceID: rec.ID, Expires: s.cfg.Now().Add(requestLifetime), PayerISPB: row.PayerIspb, PayerBranch: row.PayerBranch,
		PayerAccount: row.PayerAccount, PayerTaxID: row.CustomerTaxID,
	})
	if err == nil {
		rec, err = bank.Recurrence(ctx, rec.ID)
	}
	if err == nil && rec.Status == RecurrenceCreated && rec.RequestRejected {
		err = fmt.Errorf("%w: the customer's bank did not take the request", ErrBankRefused)
	}
	if errors.Is(err, ErrBankRefused) {
		if _, cerr := bank.CancelRecurrence(ctx, rec.ID); cerr != nil {
			return Recurrence{}, cerr
		}
	}
	return rec, err
}

// reject ends a subscription the bank refused to set up.
func (s *Service) reject(ctx context.Context, pool *pgxpool.Pool, owner Owner, subscriptionID id.ID) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := lockOwned(ctx, q, owner, subscriptionID)
		if err != nil || row.SetUp || Status(row.Status) != Incomplete {
			return err
		}
		row.Status, row.EndCode = string(Rejected), "bank_refused"
		if err := s.save(ctx, q, row); err != nil {
			return err
		}
		return s.publish(ctx, tx, owner, events.TypeSubscriptionUpdated, row.ID)
	})
}

func (s *Service) createRecurrence(ctx context.Context, bank Bank, row db.SubscriptionsSubscription) (Recurrence, error) {
	amount, err := money.New(row.Amount, money.BRL)
	if err != nil {
		return Recurrence{}, err
	}
	req := RecurrenceRequest{
		Contract: row.ID, Object: row.Description, PayerName: row.CustomerName, PayerTaxID: row.CustomerTaxID,
		Start: dateString(row.StartDate), End: dateString(row.EndDate), Period: intervals[Interval(row.Interval)], Amount: amount,
		Retries: row.Retries,
	}
	if row.AuthorizationMethod == ByQRCode {
		if req.Location, err = bank.CreateRecurrenceLocation(ctx); err != nil {
			return Recurrence{}, err
		}
	}
	rec, err := bank.CreateRecurrence(ctx, req)
	if err != nil {
		return Recurrence{}, err
	}
	// Read back: the QR code comes with the recurrence's own view.
	return bank.Recurrence(ctx, rec.ID)
}

// FinishSetUp records what the bank made. A setup that got no answer changes nothing;
// the worker repeats it.
func (s *Service) FinishSetUp(ctx context.Context, tx pgx.Tx, owner Owner, subscriptionID id.ID, rec Recurrence) (Subscription, error) {
	q := db.New(tx)
	row, err := lockOwned(ctx, q, owner, subscriptionID)
	if err != nil {
		return Subscription{}, err
	}
	if rec.ID != "" && !row.SetUp {
		row.SetUp, row.RecurrenceID = true, rec.ID
		if err := s.applyRecurrence(ctx, tx, &row, rec); err != nil {
			return Subscription{}, err
		}
	}
	return s.fromRow(ctx, tx, row)
}

// StartCancel checks a subscription can be canceled: the bank must have its recurrence.
func (s *Service) StartCancel(ctx context.Context, tx pgx.Tx, owner Owner, subscriptionID id.ID) (Subscription, error) {
	row, err := lockOwned(ctx, db.New(tx), owner, subscriptionID)
	if err != nil {
		return Subscription{}, err
	}
	switch {
	case Status(row.Status) != Incomplete && Status(row.Status) != Active && Status(row.Status) != PastDue:
		return Subscription{}, fmt.Errorf("%w: a %s subscription cannot be canceled", ErrInvalidState, row.Status)
	case !row.SetUp:
		return Subscription{}, fmt.Errorf("%w: the subscription is still being set up at the bank; try again in a minute", ErrInvalidState)
	}
	return s.fromRow(ctx, tx, row)
}

// CancelAtBank cancels the recurrence; the bank cancels the charges under it not yet
// due to be debited.
func (s *Service) CancelAtBank(ctx context.Context, q db.DBTX, owner Owner, subscriptionID id.ID) (Recurrence, error) {
	row, err := db.New(q).GetSubscription(ctx, db.GetSubscriptionParams{ID: subscriptionID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Recurrence{}, notFoundOr(err, subscriptionID)
	}
	bank, err := s.bank(row.Livemode)
	if err != nil {
		return Recurrence{}, err
	}
	return bank.CancelRecurrence(ctx, row.RecurrenceID)
}

// ApplyRecurrence records the recurrence's status as the bank reported it.
func (s *Service) ApplyRecurrence(ctx context.Context, tx pgx.Tx, owner Owner, subscriptionID id.ID, rec Recurrence) (Subscription, error) {
	q := db.New(tx)
	row, err := lockOwned(ctx, q, owner, subscriptionID)
	if err != nil {
		return Subscription{}, err
	}
	if rec.ID != "" && rec.ID == row.RecurrenceID {
		if err := s.applyRecurrence(ctx, tx, &row, rec); err != nil {
			return Subscription{}, err
		}
	}
	return s.fromRow(ctx, tx, row)
}

// applyRecurrence moves the subscription as its recurrence moved, and saves it.
func (s *Service) applyRecurrence(ctx context.Context, tx pgx.Tx, row *db.SubscriptionsSubscription, rec Recurrence) error {
	before := row.Status
	switch rec.Status {
	case RecurrenceCreated:
		row.QrCode = rec.QRCode
	case RecurrenceApproved:
		if Status(row.Status) == Incomplete {
			row.Status = string(Active)
		}
	case RecurrenceRejected:
		row.Status, row.EndCode = string(Rejected), rec.EndCode
	case RecurrenceCanceled:
		row.Status, row.EndCode, row.CanceledBy = string(Canceled), rec.EndCode, "merchant"
		if rec.EndedBy == "USUARIO_PAGADOR" || rec.EndedBy == "PSP_PAGADOR" {
			row.CanceledBy = "customer"
		}
	case RecurrenceExpired:
		row.Status = string(Ended)
	}
	if Status(row.Status) != Incomplete {
		row.QrCode = ""
	}
	if err := s.save(ctx, db.New(tx), *row); err != nil {
		return err
	}
	if row.Status == before {
		return nil
	}
	owner, err := ownerOf(*row)
	if err != nil {
		return err
	}
	return s.publish(ctx, tx, owner, events.TypeSubscriptionUpdated, row.ID)
}

func (s *Service) save(ctx context.Context, q *db.Queries, row db.SubscriptionsSubscription) error {
	return q.SaveSubscription(ctx, db.SaveSubscriptionParams{
		ID: row.ID, Status: row.Status, SetUp: row.SetUp, RecurrenceID: row.RecurrenceID, QrCode: row.QrCode,
		CanceledBy: row.CanceledBy, EndCode: row.EndCode, UpdatedAt: ts(s.cfg.Now().UTC()),
	})
}

func (s *Service) Get(ctx context.Context, q db.DBTX, owner Owner, subscriptionID id.ID) (Subscription, error) {
	row, err := db.New(q).GetSubscription(ctx, db.GetSubscriptionParams{ID: subscriptionID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Subscription{}, notFoundOr(err, subscriptionID)
	}
	return s.fromRow(ctx, q, row)
}

func (s *Service) List(ctx context.Context, q db.DBTX, owner Owner, r page.Request) ([]Subscription, bool, error) {
	rows, err := db.New(q).ListSubscriptions(ctx, db.ListSubscriptionsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing subscriptions: %w", err)
	}
	out := make([]Subscription, 0, len(rows))
	for _, row := range rows {
		sub, err := s.fromRow(ctx, q, row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, sub)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

func (s *Service) fromRow(ctx context.Context, q db.DBTX, row db.SubscriptionsSubscription) (Subscription, error) {
	subID, err := Prefix.Parse(row.ID)
	if err != nil {
		return Subscription{}, err
	}
	owner, err := ownerOf(row)
	if err != nil {
		return Subscription{}, err
	}
	amount, err := money.New(row.Amount, money.BRL)
	if err != nil {
		return Subscription{}, err
	}
	cycles, err := db.New(q).Cycles(ctx, row.ID)
	if err != nil {
		return Subscription{}, err
	}
	sub := Subscription{
		ID: subID, Owner: owner, Amount: amount, Interval: Interval(row.Interval), StartDate: dateString(row.StartDate),
		EndDate: dateString(row.EndDate), Description: row.Description, Customer: Customer{Name: row.CustomerName, TaxID: row.CustomerTaxID},
		Retries: row.Retries, Status: Status(row.Status), QRCode: row.QrCode, RecurrenceID: row.RecurrenceID,
		CanceledBy: row.CanceledBy, EndCode: row.EndCode, CreatedAt: row.CreatedAt.Time,
		Authorization: Authorization{Method: row.AuthorizationMethod, PayerISPB: row.PayerIspb, PayerBranch: row.PayerBranch, PayerAccount: row.PayerAccount},
	}
	for _, c := range cycles {
		sub.Cycles = append(sub.Cycles, Cycle{Number: int(c.Number), DueDate: dateString(c.DueDate), PaymentIntent: c.PaymentIntent, Status: CycleStatus(c.Status)})
	}
	return sub, nil
}

func (s *Service) publish(ctx context.Context, tx pgx.Tx, owner Owner, eventType, subscriptionID string) error {
	_, err := s.cfg.Events.Publish(ctx, tx, events.Owner{Merchant: owner.Merchant, Livemode: owner.Livemode}, eventType,
		events.ObjectRef{ID: subscriptionID, Type: "subscription"})
	return err
}

func ownerOf(row db.SubscriptionsSubscription) (Owner, error) {
	merchant, err := id.Parse(row.MerchantID)
	return Owner{Merchant: merchant, Livemode: row.Livemode}, err
}

// lockOwned locks a subscription of owner's.
func lockOwned(ctx context.Context, q *db.Queries, owner Owner, subscriptionID id.ID) (db.SubscriptionsSubscription, error) {
	row, err := q.LockSubscription(ctx, subscriptionID.String())
	if err == nil && (row.MerchantID != owner.Merchant.String() || row.Livemode != owner.Livemode) {
		err = pgx.ErrNoRows
	}
	return row, notFoundOr(err, subscriptionID)
}

func notFoundOr(err error, subscriptionID id.ID) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, subscriptionID)
	}
	return err
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()} }

func date(s string) pgtype.Date {
	t, err := time.Parse(time.DateOnly, s)
	return pgtype.Date{Time: t, Valid: err == nil}
}

func dateString(d pgtype.Date) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format(time.DateOnly)
}
