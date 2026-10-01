// Package disputes keeps the disputes of captured payments in one model: card
// chargebacks, which the network opens and carries through representment,
// pre-arbitration and arbitration, and MED claims on Pix, which the payer's bank makes
// through Jupiter's own. Each stage has a deadline from a versioned table; when one
// passes, the dispute moves on without anyone acting. A card dispute takes its amount
// from the payment's liable recipient at once; a MED claim holds it on the merchant's
// balance while it is analysed. Fraud reports, which count against a merchant without
// disputing anything, are kept apart, and both feed a monitored ratio per merchant.
package disputes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/disputes/db"
	"github.com/iricardofernandes/jupiter/internal/disputes/migrations"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

var (
	DisputePrefix     = id.MustPrefix("dp")
	FraudReportPrefix = id.MustPrefix("fr")
)

var (
	ErrInvalid      = errors.New("disputes: invalid request")
	ErrNotFound     = errors.New("disputes: not found")
	ErrInvalidState = errors.New("disputes: not allowed in the current state")
	ErrNoNetwork    = errors.New("disputes: no network or bank for this mode")
	// ErrRefused is the network's or the bank's refusal of an action: it will not be
	// taken however often it is sent.
	ErrRefused = errors.New("disputes: the network refused")
)

const (
	KindChargeback = "chargeback"
	KindMED        = "med"
)

const (
	StageChargeback      = "chargeback"
	StagePreArbitration  = "pre_arbitration"
	StageArbitration     = "arbitration"
	StageMEDAnalysis     = "med_analysis"
	StageMEDContestation = "med_contestation"
)

const (
	NeedsResponse = "needs_response"
	UnderReview   = "under_review"
	Won           = "won"
	Lost          = "lost"
)

// Funds says what a dispute did to the merchant's money.
const (
	FundsNone       = "none"
	FundsHeld       = "held"
	FundsWithdrawn  = "withdrawn"
	FundsReinstated = "reinstated"
	FundsReleased   = "released"
)

// Evidence is what the merchant offers against a dispute, as text: Jupiter makes none
// itself.
type Evidence struct {
	ProductDescription     string `json:"product_description,omitempty"`
	CustomerName           string `json:"customer_name,omitempty"`
	CustomerEmail          string `json:"customer_email,omitempty"`
	CustomerCommunication  string `json:"customer_communication,omitempty"`
	ShippingTrackingNumber string `json:"shipping_tracking_number,omitempty"`
	RefundPolicy           string `json:"refund_policy,omitempty"`
	UncategorizedText      string `json:"uncategorized_text,omitempty"`
}

const (
	maxEvidenceField = 5000
	// maxEvidence is the most all fields hold together, in bytes: what the network and the
	// bank take in one message.
	maxEvidence = 32 << 10
)

func (e *Evidence) fields() []*string {
	return []*string{&e.ProductDescription, &e.CustomerName, &e.CustomerEmail, &e.CustomerCommunication, &e.ShippingTrackingNumber, &e.RefundPolicy, &e.UncategorizedText}
}

// merged is e with the fields of update that are not empty.
func (e Evidence) merged(update Evidence) Evidence {
	out := e
	mine := out.fields()
	for i, f := range update.fields() {
		if *f != "" {
			*mine[i] = *f
		}
	}
	return out
}

func (e Evidence) valid() error {
	total := 0
	for _, f := range e.fields() {
		if len([]rune(*f)) > maxEvidenceField {
			return fmt.Errorf("%w: each evidence field holds at most %d characters", ErrInvalid, maxEvidenceField)
		}
		total += len(*f)
	}
	if total > maxEvidence {
		return fmt.Errorf("%w: the evidence holds at most %d bytes in all", ErrInvalid, maxEvidence)
	}
	return nil
}

func (e Evidence) empty() bool {
	for _, f := range e.fields() {
		if strings.TrimSpace(*f) != "" {
			return false
		}
	}
	return true
}

// text is the evidence as one document, for the network.
func (e Evidence) text() string {
	var b strings.Builder
	for _, part := range []struct{ label, value string }{
		{"Product", e.ProductDescription},
		{"Customer", e.CustomerName},
		{"E-mail", e.CustomerEmail},
		{"Communication", e.CustomerCommunication},
		{"Tracking", e.ShippingTrackingNumber},
		{"Refund policy", e.RefundPolicy},
		{"Other", e.UncategorizedText},
	} {
		if part.value != "" {
			// A field's own lines are indented, so none can pass for another field.
			fmt.Fprintf(&b, "%s: %s\n", part.label, strings.ReplaceAll(strings.ReplaceAll(part.value, "\r", ""), "\n", "\n  "))
		}
	}
	return b.String()
}

// Hop is where money a MED claim traces went after Jupiter: a payout of the merchant's.
type Hop struct {
	Payout     string `json:"payout"`
	EndToEndID string `json:"end_to_end_id"`
	Amount     int64  `json:"amount"`
}

// Change is one line of a dispute's history.
type Change struct {
	At     time.Time
	Kind   string
	Detail string
}

type Dispute struct {
	ID                  id.ID
	Owner               payments.Owner
	PaymentIntent       string
	Kind                string
	Network             string
	NetworkID           string
	Reason              string
	ReasonCode          string
	Amount              money.Amount
	Stage               string
	Status              string
	Liability           string
	Evidence            Evidence
	EvidenceSubmittedAt time.Time
	DueBy               time.Time
	NetworkDueBy        time.Time
	Blocked             int64
	Trace               []Hop
	Funds               string
	Outcome             string
	CreatedAt           time.Time
	ClosedAt            time.Time
	History             []Change
}

// Network is a card network's dispute system, which Jupiter answers as the acquirer.
type Network interface {
	// Act sends Jupiter's answer at a case's stage and answers the case as it then is.
	// The same answer again is answered the same.
	Act(ctx context.Context, a Action) (Notice, error)
	// Case reads a case as the network has it.
	Case(ctx context.Context, networkID string) (Notice, error)
	// Cases lists the cases changed since a moment: what notices missed.
	Cases(ctx context.Context, since time.Time) ([]Notice, error)
}

// Bank is Jupiter's Pix bank, through which MED claims reach it and its answers go back.
type Bank interface {
	Infraction(ctx context.Context, id string) (Infraction, error)
	Infractions(ctx context.Context, since, until time.Time) ([]Infraction, error)
	Analyze(ctx context.Context, id string, a Analysis) (Infraction, error)
	Contest(ctx context.Context, id, details string) (Infraction, error)
}

type Config struct {
	Pool     *pgxpool.Pool
	Payments *payments.Service
	Events   *events.Service
	// Networks serve each mode's card disputes; test mode's defaults to the test network.
	LiveNetwork Network
	TestNetwork Network
	// Banks serve each mode's MED claims; a mode without one has none.
	LiveBank Bank
	TestBank Bank
	Now      func() time.Time
	Logger   *slog.Logger
}

type Service struct {
	cfg  Config
	test *TestNetwork
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Service{cfg: cfg}
	if cfg.TestNetwork == nil {
		s.test = NewTestNetwork(cfg.Pool, cfg.Now)
		s.cfg.TestNetwork = s.test
	}
	return s
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "disputes", migrations.FS)
}

func (s *Service) network(livemode bool) (Network, error) {
	n := s.cfg.TestNetwork
	if livemode {
		n = s.cfg.LiveNetwork
	}
	if n == nil {
		return nil, ErrNoNetwork
	}
	return n, nil
}

func (s *Service) bank(livemode bool) (Bank, error) {
	b := s.cfg.TestBank
	if livemode {
		b = s.cfg.LiveBank
	}
	if b == nil {
		return nil, ErrNoNetwork
	}
	return b, nil
}

// Get reads one of the merchant's disputes, with its history.
func (s *Service) Get(ctx context.Context, q db.DBTX, owner payments.Owner, disputeID id.ID) (Dispute, error) {
	row, err := db.New(q).GetDispute(ctx, db.GetDisputeParams{ID: disputeID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return Dispute{}, fmt.Errorf("%w: %s", ErrNotFound, disputeID)
	}
	if err != nil {
		return Dispute{}, err
	}
	d, err := disputeOf(row)
	if err != nil {
		return Dispute{}, err
	}
	changes, err := db.New(q).HistoryOf(ctx, row.ID)
	if err != nil {
		return Dispute{}, err
	}
	for _, c := range changes {
		d.History = append(d.History, Change{At: c.At.Time, Kind: c.Kind, Detail: c.Detail})
	}
	return d, nil
}

// List lists the merchant's disputes, newest first, of one payment intent if named.
func (s *Service) List(ctx context.Context, q db.DBTX, owner payments.Owner, intentID string, r page.Request) ([]Dispute, bool, error) {
	rows, err := db.New(q).ListDisputes(ctx, db.ListDisputesParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, PaymentIntent: intentID,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, err
	}
	out := make([]Dispute, 0, len(rows))
	for _, row := range rows {
		d, err := disputeOf(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, d)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

func disputeOf(row db.DisputesDispute) (Dispute, error) {
	disputeID, err := DisputePrefix.Parse(row.ID)
	if err != nil {
		return Dispute{}, err
	}
	merchantID, err := id.Parse(row.MerchantID)
	if err != nil {
		return Dispute{}, err
	}
	currency, err := money.CurrencyByCode(row.Currency)
	if err != nil {
		return Dispute{}, err
	}
	amount, err := money.New(row.Amount, currency)
	if err != nil {
		return Dispute{}, err
	}
	d := Dispute{
		ID: disputeID, Owner: payments.Owner{Merchant: merchantID, Livemode: row.Livemode}, PaymentIntent: row.PaymentIntent,
		Kind: row.Kind, Network: row.Network, NetworkID: row.NetworkID, Reason: row.Reason, ReasonCode: row.ReasonCode,
		Amount: amount, Stage: row.Stage, Status: row.Status, Liability: row.Liability,
		EvidenceSubmittedAt: row.EvidenceSubmittedAt.Time, DueBy: row.DueBy.Time, NetworkDueBy: row.NetworkDueBy.Time,
		Blocked: row.Blocked, Funds: row.Funds, Outcome: row.Outcome, CreatedAt: row.CreatedAt.Time, ClosedAt: row.ClosedAt.Time,
	}
	if err := json.Unmarshal(row.Evidence, &d.Evidence); err != nil {
		return Dispute{}, err
	}
	if err := json.Unmarshal(row.Trace, &d.Trace); err != nil {
		return Dispute{}, err
	}
	return d, nil
}

// save writes a dispute's row back and records what changed in its history.
func (s *Service) save(ctx context.Context, q *db.Queries, row db.DisputesDispute, kind, detail string) error {
	now := s.cfg.Now().UTC()
	row.UpdatedAt = ts(now)
	if err := q.SaveDispute(ctx, db.SaveDisputeParams{
		ID: row.ID, Stage: row.Stage, Status: row.Status, Liability: row.Liability, Evidence: row.Evidence,
		EvidenceSubmittedAt: row.EvidenceSubmittedAt, DueBy: row.DueBy, NetworkDueBy: row.NetworkDueBy, Blocked: row.Blocked,
		BlockedAt: row.BlockedAt, Trace: row.Trace, Funds: row.Funds, PendingAction: row.PendingAction,
		ActionError: row.ActionError, ActionRetryAt: row.ActionRetryAt, NetworkVersion: row.NetworkVersion,
		Outcome: row.Outcome, UpdatedAt: row.UpdatedAt, ClosedAt: row.ClosedAt,
	}); err != nil {
		return err
	}
	if kind == "" {
		return nil
	}
	return q.InsertHistory(ctx, db.InsertHistoryParams{DisputeID: row.ID, At: ts(now), Kind: kind, Detail: detail})
}

// publish tells the merchant of a dispute's change.
func (s *Service) publish(ctx context.Context, tx pgx.Tx, row db.DisputesDispute, eventType string) error {
	if s.cfg.Events == nil {
		return nil
	}
	merchantID, err := id.Parse(row.MerchantID)
	if err != nil {
		return err
	}
	_, err = s.cfg.Events.Publish(ctx, tx, events.Owner{Merchant: merchantID, Livemode: row.Livemode}, eventType, events.ObjectRef{ID: row.ID, Type: "dispute"})
	return err
}

// close ends a dispute as won or lost, for outcome.
func (s *Service) close(row *db.DisputesDispute, status, outcome string) {
	row.Status, row.Outcome = status, outcome
	row.DueBy, row.NetworkDueBy = pgtype.Timestamptz{}, pgtype.Timestamptz{}
	row.ClosedAt = ts(s.cfg.Now().UTC())
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: !t.IsZero()}
}

func truncate(s string) string { return truncateTo(s, 300) }

func truncateTo(s string, most int) string {
	if r := []rune(s); len(r) > most {
		return string(r[:most])
	}
	return s
}
