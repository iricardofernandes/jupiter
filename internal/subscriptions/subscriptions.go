// Package subscriptions charges customers periodically by Pix Automático (ADR 0028). A
// subscription is a recurrence at Jupiter's bank, which the customer authorizes once,
// by a request their bank shows them or by reading a QR code, and then a payment intent
// per cycle, which the bank charges (a cobr) and the customer's bank debits on its due
// date, retrying if the subscription allows. The bank's word decides every status: the
// module reads it back after each notification and on a schedule.
package subscriptions

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/subscriptions/migrations"
)

var Prefix = id.MustPrefix("sub")

var (
	ErrInvalid         = errors.New("subscriptions: invalid request")
	ErrNotFound        = errors.New("subscriptions: not found")
	ErrInvalidState    = errors.New("subscriptions: not allowed in the current state")
	ErrBankUnavailable = errors.New("subscriptions: no Pix bank for this mode")
	// ErrBankNotFound is the bank having no such record.
	ErrBankNotFound = errors.New("subscriptions: the bank has no such record")
	// ErrBankRefused is the bank refusing a request; any other bank error leaves the
	// outcome unknown.
	ErrBankRefused = errors.New("subscriptions: the bank refused the request")
)

type Status string

const (
	Incomplete Status = "incomplete"
	Active     Status = "active"
	PastDue    Status = "past_due"
	Rejected   Status = "rejected"
	Canceled   Status = "canceled"
	Ended      Status = "ended"
)

type Interval string

const (
	Week     Interval = "week"
	Month    Interval = "month"
	Quarter  Interval = "quarter"
	HalfYear Interval = "half_year"
	Year     Interval = "year"
)

// How the customer authorizes the subscription.
const (
	// ByQRCode: the customer reads a QR code (journey 2).
	ByQRCode = "qr_code"
	// ByPayerRequest: Jupiter asks the customer's bank, which shows them the request to
	// accept or reject (journey 1).
	ByPayerRequest = "payer_request"
)

type Owner = payments.Owner

type Subscription struct {
	ID          id.ID
	Owner       Owner
	Amount      money.Amount
	Interval    Interval
	StartDate   string
	EndDate     string
	Description string
	Customer    Customer
	// Retries lets the customer's bank try again after the due date: up to three times
	// within seven days.
	Retries       bool
	Authorization Authorization
	Status        Status
	// QRCode is what the customer reads to authorize it, while it waits for them.
	QRCode       string
	RecurrenceID string
	CanceledBy   string
	EndCode      string
	Cycles       []Cycle
	CreatedAt    time.Time
}

type Customer struct {
	Name  string
	TaxID string
}

type Authorization struct {
	Method string
	// The customer's account, for a payer request: their bank's ISPB, branch and account.
	PayerISPB    string
	PayerBranch  string
	PayerAccount string
}

type CycleStatus string

const (
	CyclePending  CycleStatus = "pending"
	CyclePaid     CycleStatus = "paid"
	CycleFailed   CycleStatus = "failed"
	CycleCanceled CycleStatus = "canceled"
)

type Cycle struct {
	Number        int
	DueDate       string
	PaymentIntent string
	Status        CycleStatus
}

// Bank is the recurrence side of Jupiter's Pix bank, through the API Pix.
type Bank interface {
	// CreateRecurrenceLocation makes a location for a recurrence's QR code.
	CreateRecurrenceLocation(ctx context.Context) (int64, error)
	CreateRecurrence(ctx context.Context, r RecurrenceRequest) (Recurrence, error)
	Recurrence(ctx context.Context, id string) (Recurrence, error)
	// FindRecurrence looks for the recurrence made for a contract since a time: what a
	// creation whose answer was lost made, if anything. It wraps ErrBankNotFound.
	FindRecurrence(ctx context.Context, contract, payerTaxID string, since time.Time) (Recurrence, error)
	RequestAuthorization(ctx context.Context, r AuthorizationRequest) error
	CancelRecurrence(ctx context.Context, id string) (Recurrence, error)
}

type RecurrenceRequest struct {
	Contract   string
	Object     string
	PayerName  string
	PayerTaxID string
	Start      string
	End        string
	Period     string
	Amount     money.Amount
	Retries    bool
	Location   int64
}

// Recurrence statuses, as the API Pix names them.
const (
	RecurrenceCreated  = "CRIADA"
	RecurrenceApproved = "APROVADA"
	RecurrenceRejected = "REJEITADA"
	RecurrenceExpired  = "EXPIRADA"
	RecurrenceCanceled = "CANCELADA"
)

type Recurrence struct {
	ID     string
	Status string
	QRCode string
	// EndedBy (PSP_PAGADOR, USUARIO_PAGADOR, PSP_RECEBEDOR or USUARIO_RECEBEDOR) and
	// EndCode say who ended a canceled or rejected recurrence, and why.
	EndedBy string
	EndCode string
	// PendingRequest says a request is waiting for the payer; RequestRejected, that the
	// latest one was rejected before the recurrence was, so it never reached them.
	PendingRequest  bool
	RequestRejected bool
}

type AuthorizationRequest struct {
	RecurrenceID string
	Expires      time.Time
	PayerISPB    string
	PayerBranch  string
	PayerAccount string
	PayerTaxID   string
}

type Config struct {
	Pool     *pgxpool.Pool
	Payments *payments.Service
	Events   *events.Service
	LiveBank Bank
	TestBank Bank
	Now      func() time.Time
	Logger   *slog.Logger
}

type Service struct {
	cfg Config
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{cfg: cfg}
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "subscriptions", migrations.FS)
}

func (s *Service) bank(livemode bool) (Bank, error) {
	b := s.cfg.TestBank
	if livemode {
		b = s.cfg.LiveBank
	}
	if b == nil {
		return nil, ErrBankUnavailable
	}
	return b, nil
}

// brasilia is the time zone of due dates. Brazil has kept no summer time since 2019.
var brasilia = time.FixedZone("BRT", -3*60*60)

func (s *Service) today() time.Time {
	y, m, d := s.cfg.Now().In(brasilia).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
