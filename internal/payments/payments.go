package payments

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/migrations"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/risk"
)

var (
	IntentPrefix  = id.MustPrefix("pi")
	AttemptPrefix = id.MustPrefix("pa")
	RefundPrefix  = id.MustPrefix("re")
)

var (
	ErrInvalid          = errors.New("payments: invalid request")
	ErrNotFound         = errors.New("payments: not found")
	ErrInvalidState     = errors.New("payments: not allowed in the current state")
	ErrRailUnavailable  = errors.New("payments: no payment rail for this mode")
	ErrAmountTooLarge   = errors.New("payments: amount exceeds what remains")
	errAttemptNotLatest = errors.New("payments: the intent has no attempt in progress")
)

type Owner struct {
	Merchant id.ID
	Livemode bool
}

type CaptureMethod string

const (
	CaptureAutomatic CaptureMethod = "automatic"
	CaptureManual    CaptureMethod = "manual"
)

type PaymentError struct {
	Code        string
	DeclineCode string
	Message     string
}

type Intent struct {
	ID               id.ID
	Owner            Owner
	Amount           money.Amount
	CaptureMethod    CaptureMethod
	Status           Status
	PaymentMethod    string
	Description      string
	AmountCapturable money.Amount
	AmountReceived   money.Amount
	AmountRefunded   money.Amount
	LatestAttempt    id.ID
	LastError        *PaymentError
	NextAction       string
	NextActionURL    string
	// NextActionData is the BR Code a Pix payment waits to be paid with, until
	// NextActionExpiresAt.
	NextActionData      string
	NextActionExpiresAt time.Time
	Pix                 *PixOptions
	CancellationReason  string
	Installments        *Installments
	SetupFutureUsage    string
	RequestThreeDSecure string
	// RiskDecision is the latest attempt's: allow, review, request_3ds or block.
	RiskDecision   string
	RiskDecisionID string
	CreatedAt      time.Time
}

// Financing says who pays for installments: the merchant, who receives each one as it
// falls due (parcelado lojista, without interest to the cardholder), or the issuer, who
// charges the cardholder interest (parcelado emissor).
type Financing string

const (
	FinancedByMerchant Financing = "merchant"
	FinancedByIssuer   Financing = "issuer"
)

type Installments struct {
	Count      int
	FinancedBy Financing
}

// SetupOffSession is the setup_future_usage that stores a card for merchant-initiated
// payments.
const SetupOffSession = "off_session"

type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

type Refund struct {
	ID            id.ID
	Owner         Owner
	Intent        id.ID
	Amount        money.Amount
	Reason        string
	Status        RefundStatus
	FailureReason string
	CreatedAt     time.Time
}

type Config struct {
	Ledger *ledger.Ledger
	Events *events.Service
	// Rails serve each mode; a mode without one cannot confirm payments.
	TestRail Rail
	LiveRail Rail
	// Risk decides on every attempt before it reaches a rail; nil lets every attempt
	// through.
	Risk *risk.Service
	// Authenticator runs 3-D Secure for live-mode cards; without one, live payments go
	// unauthenticated.
	Authenticator Authenticator
	// Pix rails serve each mode's Pix payments and payouts; a mode without one has no Pix.
	TestPix PixRail
	LivePix PixRail
	// Receivables, if set, is told of every card capture and refund.
	Receivables Receivables
	Now         func() time.Time
	// ResolveAfter is how long an operation stays in flight before the resolver asks
	// the rail what happened; GiveUpAfter is when an authorization still unknown is
	// reversed and failed, so no attempt stays unknown for longer.
	ResolveAfter time.Duration
	GiveUpAfter  time.Duration
}

type Service struct {
	cfg Config
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ResolveAfter == 0 {
		cfg.ResolveAfter = time.Minute
	}
	if cfg.GiveUpAfter == 0 {
		cfg.GiveUpAfter = 15 * time.Minute
	}
	return &Service{cfg: cfg}
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "payments", migrations.FS)
}

func (s *Service) rail(livemode bool) (Rail, error) {
	r := s.cfg.TestRail
	if livemode {
		r = s.cfg.LiveRail
	}
	if r == nil {
		return nil, ErrRailUnavailable
	}
	return r, nil
}
