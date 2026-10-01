// Package recipients keeps the people and companies a merchant's payments are split to:
// sellers in a marketplace, and the merchant itself. Each has a CPF or CNPJ, under which
// its receivables are registered, a verification (KYC/KYB) status, where it is paid out,
// and its transfer and automatic anticipation settings.
package recipients

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/recipients/db"
	"github.com/iricardofernandes/jupiter/internal/recipients/migrations"
	"github.com/iricardofernandes/jupiter/pkg/bizday"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

var Prefix = id.MustPrefix("rp")

const maxRecipients = 10_000

var (
	ErrInvalid  = errors.New("recipients: invalid request")
	ErrNotFound = errors.New("recipients: not found")
)

type Owner = events.Owner

type Status string

const (
	Pending  Status = "pending"
	Verified Status = "verified"
	Rejected Status = "rejected"
)

type Interval string

const (
	Manual  Interval = "manual"
	Daily   Interval = "daily"
	Weekly  Interval = "weekly"
	Monthly Interval = "monthly"
)

// Destination is where a recipient is paid out: a Pix key, or a bank account.
type Destination struct {
	Method  string // "pix" or "bank_account"; empty when none is set
	PixKey  string
	ISPB    string
	Branch  string
	Account string
}

// Transfers say when a recipient's available balance is paid out on its own: never
// (manual), every business day, on a weekday (1 Monday to 5 Friday) or on a day of the
// month (1 to 28).
type Transfers struct {
	Interval Interval
	Day      int
}

// AutoAnticipation anticipates every unit of the recipient's that is free, once it is
// DelayDays old.
type AutoAnticipation struct {
	Enabled   bool
	DelayDays int
}

type Recipient struct {
	ID               id.ID
	Owner            Owner
	Name             string
	TaxID            string
	Default          bool
	Status           Status
	Destination      Destination
	Transfers        Transfers
	AutoAnticipation AutoAnticipation
	// PayoutsHeld says an operator holds its payouts.
	PayoutsHeld bool
	CreatedAt   time.Time
}

type Params struct {
	Name             string
	TaxID            string
	Destination      *Destination
	Transfers        *Transfers
	AutoAnticipation *AutoAnticipation
}

type Config struct {
	Merchants *merchant.Service
	Events    *events.Service
	Now       func() time.Time
}

type Service struct {
	cfg Config
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg}
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "recipients", migrations.FS)
}

var (
	ispbPattern    = regexp.MustCompile(`^\d{8}$`)
	branchPattern  = regexp.MustCompile(`^\d{1,4}$`)
	accountPattern = regexp.MustCompile(`^\d{1,19}(-[\dXx])?$`)
)

func validDestination(d Destination) error {
	switch d.Method {
	case "pix":
		if !payments.ValidPixKey(d.PixKey) {
			return fmt.Errorf("%w: payout_destination[pix_key] is not a Pix key", ErrInvalid)
		}
	case "bank_account":
		if !ispbPattern.MatchString(d.ISPB) || !branchPattern.MatchString(d.Branch) || !accountPattern.MatchString(d.Account) {
			return fmt.Errorf("%w: a bank account needs an ISPB of 8 digits, a branch and an account number", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: payout_destination[type] must be pix or bank_account", ErrInvalid)
	}
	return nil
}

func validTransfers(t Transfers) error {
	switch {
	case t.Interval == Manual || t.Interval == Daily:
		if t.Day != 0 {
			return fmt.Errorf("%w: transfer_settings[day] is only for weekly and monthly transfers", ErrInvalid)
		}
	case t.Interval == Weekly && (t.Day < 1 || t.Day > 5):
		return fmt.Errorf("%w: a weekly transfer's day is 1 (Monday) to 5 (Friday)", ErrInvalid)
	case t.Interval == Monthly && (t.Day < 1 || t.Day > 28):
		return fmt.Errorf("%w: a monthly transfer's day is 1 to 28", ErrInvalid)
	case t.Interval != Weekly && t.Interval != Monthly:
		return fmt.Errorf("%w: transfer_settings[interval] must be manual, daily, weekly or monthly", ErrInvalid)
	}
	return nil
}

func validAuto(a AutoAnticipation) error {
	if a.DelayDays < 1 || a.DelayDays > 30 {
		return fmt.Errorf("%w: automatic_anticipation[delay_days] must be 1 to 30", ErrInvalid)
	}
	return nil
}

// Create adds a recipient, pending verification.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, owner Owner, p Params) (Recipient, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len([]rune(p.Name)) > 200 || !taxid.Valid(p.TaxID) {
		return Recipient{}, fmt.Errorf("%w: a recipient needs a name of up to 200 characters and a valid CPF or CNPJ", ErrInvalid)
	}
	n, err := db.New(tx).CountRecipients(ctx, db.CountRecipientsParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Recipient{}, err
	}
	if n >= maxRecipients {
		return Recipient{}, fmt.Errorf("%w: a merchant has at most %d recipients", ErrInvalid, maxRecipients)
	}
	row := db.RecipientsRecipient{
		ID: Prefix.New().String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Name: p.Name, TaxID: p.TaxID,
		Status: string(Pending), TransferInterval: string(Manual), AutoAnticipationDelay: 1,
	}
	if err := apply(&row, p); err != nil {
		return Recipient{}, err
	}
	return s.insert(ctx, tx, owner, row)
}

func (s *Service) insert(ctx context.Context, tx pgx.Tx, owner Owner, row db.RecipientsRecipient) (Recipient, error) {
	now := s.cfg.Now().UTC()
	err := db.New(tx).InsertRecipient(ctx, db.InsertRecipientParams{
		ID: row.ID, MerchantID: row.MerchantID, Livemode: row.Livemode, Name: row.Name, TaxID: row.TaxID, IsDefault: row.IsDefault,
		Status: row.Status, PayoutMethod: row.PayoutMethod, PixKey: row.PixKey, BankIspb: row.BankIspb, BankBranch: row.BankBranch,
		BankAccount: row.BankAccount, TransferInterval: row.TransferInterval, TransferDay: row.TransferDay,
		AutoAnticipation: row.AutoAnticipation, AutoAnticipationDelay: row.AutoAnticipationDelay, Now: ts(now),
	})
	if postgres.ErrorCode(err) == "23505" {
		return Recipient{}, fmt.Errorf("%w: that CPF or CNPJ cannot be a recipient here", ErrInvalid)
	}
	if err != nil {
		return Recipient{}, err
	}
	if err := s.publish(ctx, tx, owner, events.TypeRecipientCreated, row.ID); err != nil {
		return Recipient{}, err
	}
	row.CreatedAt = ts(now)
	return fromRow(row)
}

// apply sets what p changes on row, checking it.
func apply(row *db.RecipientsRecipient, p Params) error {
	if d := p.Destination; d != nil {
		if err := validDestination(*d); err != nil {
			return err
		}
		row.PayoutMethod, row.PixKey, row.BankIspb, row.BankBranch, row.BankAccount = d.Method, "", "", "", ""
		if d.Method == "pix" {
			row.PixKey = d.PixKey
		} else {
			row.BankIspb, row.BankBranch, row.BankAccount = d.ISPB, d.Branch, d.Account
		}
	}
	if t := p.Transfers; t != nil {
		if err := validTransfers(*t); err != nil {
			return err
		}
		row.TransferInterval, row.TransferDay = string(t.Interval), int32(t.Day) //nolint:gosec // 0 to 28
	}
	if a := p.AutoAnticipation; a != nil {
		if err := validAuto(*a); err != nil {
			return err
		}
		row.AutoAnticipation, row.AutoAnticipationDelay = a.Enabled, int32(a.DelayDays) //nolint:gosec // 1 to 30
	}
	return nil
}

// Update changes a recipient's name, destination and settings; its tax id stays.
func (s *Service) Update(ctx context.Context, tx pgx.Tx, owner Owner, recipientID id.ID, p Params) (Recipient, error) {
	q := db.New(tx)
	row, err := q.LockRecipient(ctx, db.LockRecipientParams{ID: recipientID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Recipient{}, notFoundOr(err, recipientID)
	}
	if p.TaxID != "" && p.TaxID != row.TaxID {
		return Recipient{}, fmt.Errorf("%w: a recipient's CPF or CNPJ cannot change", ErrInvalid)
	}
	if name := strings.TrimSpace(p.Name); name != "" {
		if len([]rune(name)) > 200 {
			return Recipient{}, fmt.Errorf("%w: name is longer than 200 characters", ErrInvalid)
		}
		row.Name = name
	}
	before := row
	if err := apply(&row, p); err != nil {
		return Recipient{}, err
	}
	moved := row.PayoutMethod != before.PayoutMethod || row.PixKey != before.PixKey ||
		row.BankIspb != before.BankIspb || row.BankBranch != before.BankBranch || row.BankAccount != before.BankAccount
	// Who it is, and where it is paid, are what was verified: a change is verified anew.
	if !row.IsDefault && (row.Name != before.Name || moved) {
		row.Status = string(Pending)
	}
	rec, err := s.save(ctx, tx, owner, row)
	if err != nil || !row.IsDefault || !moved || before.PayoutMethod == "" {
		return rec, err
	}
	// The merchant's own recipient is the merchant, verified with it; a new destination for
	// its money, which a stolen key could set, waits for an operator before payouts go.
	if err := s.HoldPayouts(ctx, tx, row.ID, true); err != nil {
		return Recipient{}, err
	}
	rec.PayoutsHeld = true
	return rec, nil
}

// Verify records the outcome of a recipient's verification.
func (s *Service) Verify(ctx context.Context, tx pgx.Tx, owner Owner, recipientID id.ID, status Status) (Recipient, error) {
	if status != Verified && status != Rejected {
		return Recipient{}, fmt.Errorf("%w: a verification ends verified or rejected", ErrInvalid)
	}
	row, err := db.New(tx).LockRecipient(ctx, db.LockRecipientParams{ID: recipientID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Recipient{}, notFoundOr(err, recipientID)
	}
	row.Status = string(status)
	return s.save(ctx, tx, owner, row)
}

func (s *Service) save(ctx context.Context, tx pgx.Tx, owner Owner, row db.RecipientsRecipient) (Recipient, error) {
	row.UpdatedAt = ts(s.cfg.Now().UTC())
	if err := db.New(tx).SaveRecipient(ctx, db.SaveRecipientParams{
		ID: row.ID, Name: row.Name, Status: row.Status, PayoutMethod: row.PayoutMethod, PixKey: row.PixKey, BankIspb: row.BankIspb,
		BankBranch: row.BankBranch, BankAccount: row.BankAccount, TransferInterval: row.TransferInterval, TransferDay: row.TransferDay,
		AutoAnticipation: row.AutoAnticipation, AutoAnticipationDelay: row.AutoAnticipationDelay, UpdatedAt: row.UpdatedAt,
	}); err != nil {
		return Recipient{}, err
	}
	if err := s.publish(ctx, tx, owner, events.TypeRecipientUpdated, row.ID); err != nil {
		return Recipient{}, err
	}
	return fromRow(row)
}

// Default is the merchant's own recipient, made from its CPF or CNPJ the first time it is
// needed. The merchant was verified when it signed up, so its recipient is too.
func (s *Service) Default(ctx context.Context, tx pgx.Tx, owner Owner) (Recipient, error) {
	q := db.New(tx)
	key := db.DefaultRecipientParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode}
	row, err := q.DefaultRecipient(ctx, key)
	if err == nil {
		return fromRow(row)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Recipient{}, err
	}
	m, err := s.cfg.Merchants.Get(ctx, tx, owner.Merchant)
	if err != nil {
		return Recipient{}, err
	}
	if m.TaxID == "" {
		return Recipient{}, fmt.Errorf("%w: the merchant has no CPF or CNPJ yet", ErrInvalid)
	}
	return s.insert(ctx, tx, owner, db.RecipientsRecipient{
		ID: Prefix.New().String(), MerchantID: key.MerchantID, Livemode: key.Livemode, Name: m.Name, TaxID: m.TaxID,
		IsDefault: true, Status: string(Verified), TransferInterval: string(Manual), AutoAnticipationDelay: 1,
	})
}

// DefaultOf reads the merchant's own recipient, if it was made.
func (s *Service) DefaultOf(ctx context.Context, q db.DBTX, owner Owner) (Recipient, error) {
	row, err := db.New(q).DefaultRecipient(ctx, db.DefaultRecipientParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Recipient{}, notFoundOr(err, id.ID{})
	}
	return fromRow(row)
}

func (s *Service) Get(ctx context.Context, q db.DBTX, owner Owner, recipientID id.ID) (Recipient, error) {
	row, err := db.New(q).GetRecipient(ctx, db.GetRecipientParams{ID: recipientID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Recipient{}, notFoundOr(err, recipientID)
	}
	return fromRow(row)
}

// ByID reads a recipient without its owner: for the jobs that act on every merchant's.
func (s *Service) ByID(ctx context.Context, q db.DBTX, recipientID string) (Recipient, error) {
	row, err := db.New(q).GetRecipientByID(ctx, recipientID)
	if err != nil {
		return Recipient{}, notFoundOr(err, id.ID{})
	}
	return fromRow(row)
}

func (s *Service) List(ctx context.Context, q db.DBTX, owner Owner, r page.Request) ([]Recipient, bool, error) {
	rows, err := db.New(q).ListRecipients(ctx, db.ListRecipientsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]Recipient, 0, len(rows))
	for _, row := range rows {
		rec, err := fromRow(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, rec)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

// Of reads the merchant's recipients among ids; an error names any that is not the
// merchant's, in this mode.
func (s *Service) Of(ctx context.Context, q db.DBTX, owner Owner, ids []string) (map[string]Recipient, error) {
	rows, err := db.New(q).RecipientsOf(ctx, db.RecipientsOfParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Ids: ids})
	if err != nil {
		return nil, err
	}
	out := map[string]Recipient{}
	for _, row := range rows {
		rec, err := fromRow(row)
		if err != nil {
			return nil, err
		}
		out[row.ID] = rec
	}
	for _, rid := range ids {
		if _, ok := out[rid]; !ok {
			return nil, fmt.Errorf("%w: no recipient %s", ErrInvalid, rid)
		}
	}
	return out, nil
}

// AutoAnticipating lists a page of the verified recipients of a mode that anticipate on
// their own, after the one named.
func (s *Service) AutoAnticipating(ctx context.Context, q db.DBTX, livemode bool, after string) ([]Recipient, error) {
	rows, err := db.New(q).AutoAnticipating(ctx, db.AutoAnticipatingParams{Livemode: livemode, After: after})
	if err != nil {
		return nil, err
	}
	out := make([]Recipient, 0, len(rows))
	for _, row := range rows {
		rec, err := fromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (s *Service) publish(ctx context.Context, tx pgx.Tx, owner Owner, eventType, recipientID string) error {
	if s.cfg.Events == nil {
		return nil
	}
	_, err := s.cfg.Events.Publish(ctx, tx, owner, eventType, events.ObjectRef{ID: recipientID, Type: "recipient"})
	return err
}

func fromRow(row db.RecipientsRecipient) (Recipient, error) {
	rid, err := Prefix.Parse(row.ID)
	if err != nil {
		return Recipient{}, err
	}
	m, err := merchant.MerchantPrefix.Parse(row.MerchantID)
	if err != nil {
		return Recipient{}, err
	}
	return Recipient{
		ID: rid, Owner: Owner{Merchant: m, Livemode: row.Livemode}, Name: row.Name, TaxID: row.TaxID, Default: row.IsDefault,
		Status:           Status(row.Status),
		Destination:      Destination{Method: row.PayoutMethod, PixKey: row.PixKey, ISPB: row.BankIspb, Branch: row.BankBranch, Account: row.BankAccount},
		Transfers:        Transfers{Interval: Interval(row.TransferInterval), Day: int(row.TransferDay)},
		AutoAnticipation: AutoAnticipation{Enabled: row.AutoAnticipation, DelayDays: int(row.AutoAnticipationDelay)},
		PayoutsHeld:      row.PayoutsHeld,
		CreatedAt:        row.CreatedAt.Time,
	}, nil
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

func notFoundOr(err error, recipientID id.ID) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, recipientID)
	}
	return err
}

var _ payments.Recipients = (*Service)(nil)

// CheckSplit admits a split to the merchant's verified recipients in its mode: their
// receivables are registered under their CPF or CNPJ, which must be theirs.
func (s *Service) CheckSplit(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientIDs []string) error {
	recs, err := s.Of(ctx, tx, Owner{Merchant: owner.Merchant, Livemode: owner.Livemode}, recipientIDs)
	if err != nil {
		return fmt.Errorf("%w: %w", payments.ErrInvalid, err)
	}
	for _, rec := range recs {
		if rec.Status != Verified {
			return fmt.Errorf("%w: recipient %s is %s, not verified", payments.ErrInvalid, rec.ID, rec.Status)
		}
	}
	return nil
}

// HoldPayouts holds, or lifts the hold on, a recipient's payouts: an operator's decision.
func (s *Service) HoldPayouts(ctx context.Context, tx pgx.Tx, recipientID string, held bool) error {
	return db.New(tx).SetPayoutsHeld(ctx, db.SetPayoutsHeldParams{ID: recipientID, Held: held, Now: ts(s.cfg.Now().UTC())})
}

// Transferring lists a page of the verified recipients of a mode whose payouts are
// scheduled, after the one named.
func (s *Service) Transferring(ctx context.Context, q db.DBTX, livemode bool, after string) ([]Recipient, error) {
	rows, err := db.New(q).TransferringRecipients(ctx, db.TransferringRecipientsParams{Livemode: livemode, After: after})
	if err != nil {
		return nil, err
	}
	out := make([]Recipient, 0, len(rows))
	for _, row := range rows {
		rec, err := fromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// TransferDue reports whether a recipient's scheduled payout falls on day: every business
// day; or on its weekday, or day of the month, or the first business day after when that
// is not one.
func (t Transfers) Due(day time.Time) bool {
	if !bizday.IsBusinessDay(day) {
		return false
	}
	var target time.Time
	switch t.Interval {
	case Daily:
		return true
	case Weekly:
		monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
		target = monday.AddDate(0, 0, t.Day-1)
	case Monthly:
		target = time.Date(day.Year(), day.Month(), t.Day, 0, 0, 0, 0, day.Location())
	default:
		return false
	}
	return bizday.Next(target).Equal(day)
}
