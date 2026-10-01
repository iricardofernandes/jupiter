package disputes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/disputes/db"
	"github.com/iricardofernandes/jupiter/internal/disputes/rules"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// The network's statuses and outcomes, as pkg/cardnet names them.
const (
	networkOpen      = "open"
	networkResponded = "responded"
	networkClosed    = "closed"
	acquirerWon      = "acquirer_won"
	issuerWon        = "issuer_won"
)

// Actions Jupiter takes at the network.
const (
	actionRepresent       = "represent"
	actionRepresentCapped = "represent_liability_cap"
	actionAccept          = "accept"
	actionEscalate        = "escalate"
)

// Notice is a card dispute as the network has it.
type Notice struct {
	NetworkID            string
	NetworkTransactionID string
	Amount               int64
	Currency             string
	ReasonCode           string
	Category             string
	Stage                string
	Status               string
	Outcome              string
	RespondBy            time.Time
	AuthorizedAt         time.Time
	OpenedAt             time.Time
	Version              int
}

// Action is Jupiter's answer at a case's stage: represent, escalate or accept.
type Action struct {
	NetworkID string
	Kind      string
	Stage     string
	Evidence  string
	// Reason is liability_cap for a representment that the dispute came after the
	// participants' liability ended.
	Reason string
}

// reasonOf is the merchant-facing reason for a Visa or Mastercard reason code.
func reasonOf(code, category string) string {
	switch code {
	case "12.6.1", "12.6.2", "4834":
		return "duplicate"
	case "13.1", "4855":
		return "product_not_received"
	case "13.2", "4841":
		return "subscription_canceled"
	case "13.3", "4853":
		return "product_unacceptable"
	case "13.6", "4860":
		return "credit_not_processed"
	}
	if category == "fraud" {
		return "fraudulent"
	}
	return "general"
}

// ApplyNotice applies what the network says of a case: a new chargeback opens a dispute,
// a later change moves it on. A notice older than one applied changes nothing.
func (s *Service) ApplyNotice(ctx context.Context, pool *pgxpool.Pool, livemode bool, n Notice) error {
	if n.NetworkID == "" || n.Amount <= 0 || n.Version < 1 {
		return fmt.Errorf("%w: a notice names a case, an amount and a version", ErrInvalid)
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error { return s.applyNotice(ctx, tx, livemode, n) })
}

func (s *Service) applyNotice(ctx context.Context, tx pgx.Tx, livemode bool, n Notice) error {
	q := db.New(tx)
	row, err := q.LockByNetworkID(ctx, db.LockByNetworkIDParams{Livemode: livemode, Kind: KindChargeback, NetworkID: n.NetworkID})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.openChargeback(ctx, tx, livemode, n)
	}
	if err != nil || int(row.NetworkVersion) >= n.Version {
		return err
	}
	if n.NetworkTransactionID != row.NetworkTransactionID || n.Amount != row.Amount {
		return fmt.Errorf("%w: case %s names %s for %d; it was %s for %d", ErrInvalid, n.NetworkID, n.NetworkTransactionID, n.Amount, row.NetworkTransactionID, row.Amount)
	}
	return s.moveOn(ctx, tx, row, n)
}

// openChargeback opens a dispute for a chargeback. Within the participants' liability
// its amount is withdrawn at once; past it, Jupiter represents citing the cap and takes
// nothing.
func (s *Service) openChargeback(ctx context.Context, tx pgx.Tx, livemode bool, n Notice) error {
	p, err := s.cfg.Payments.DisputedPaymentByNetworkID(ctx, tx, livemode, n.NetworkTransactionID)
	if err != nil {
		return err
	}
	if p.Method != payments.MethodCard {
		return fmt.Errorf("%w: %s is not a card payment", ErrInvalid, n.NetworkTransactionID)
	}
	now := s.cfg.Now().UTC()
	authorized := p.At
	if !n.AuthorizedAt.IsZero() {
		authorized = n.AuthorizedAt
	}
	row := db.DisputesDispute{
		ID: DisputePrefix.New().String(), MerchantID: p.Owner.Merchant.String(), Livemode: livemode, PaymentIntent: p.Intent.String(),
		AttemptID: p.Attempt, Kind: KindChargeback, Network: p.Scheme, NetworkID: n.NetworkID, NetworkTransactionID: n.NetworkTransactionID,
		Reason: reasonOf(n.ReasonCode, n.Category), ReasonCode: n.ReasonCode, Amount: n.Amount, Currency: p.Captured.Currency().Code(),
		Stage: StageChargeback, Status: NeedsResponse, Liability: "merchant", AuthorizedAt: ts(authorized), NotifiedAt: ts(now),
		NetworkVersion: versionOf(n), Evidence: []byte("{}"), Trace: []byte("[]"), Funds: FundsNone,
	}
	opened := n.OpenedAt
	if opened.IsZero() {
		opened = now
	}
	if rules.Has(p.Scheme, rules.LiabilityCap, opened) && opened.After(rules.For(p.Scheme, rules.LiabilityCap, opened).Due(authorized)) {
		row.Liability, row.Status, row.PendingAction = "scheme", UnderReview, actionRepresentCapped
	} else {
		s.setDeadlines(&row, n.RespondBy)
	}
	if err := insert(ctx, db.New(tx), row, now); err != nil {
		return err
	}
	if err := db.New(tx).InsertHistory(ctx, db.InsertHistoryParams{
		DisputeID: row.ID, At: ts(now), Kind: "opened", Detail: fmt.Sprintf("chargeback %s for %d, reason %s; liability %s", n.NetworkID, n.Amount, n.ReasonCode, row.Liability),
	}); err != nil {
		return err
	}
	if err := s.publish(ctx, tx, row, events.TypeDisputeCreated); err != nil {
		return err
	}
	if row.Liability == "merchant" {
		if err := s.withdraw(ctx, tx, &row, p); err != nil {
			return err
		}
	}
	if n.Status != networkOpen || n.Stage != StageChargeback {
		return s.moveOn(ctx, tx, row, n)
	}
	return nil
}

// withdraw takes a card dispute's amount from the merchant, up to what is left of the
// payment: what was refunded went back to the cardholder already.
func (s *Service) withdraw(ctx context.Context, tx pgx.Tx, row *db.DisputesDispute, p payments.DisputedPayment) error {
	amount, err := money.New(row.Amount, p.Captured.Currency())
	if err != nil {
		return err
	}
	withdrawn, err := s.cfg.Payments.WithdrawDisputed(ctx, tx, row.ID, p, amount)
	if err != nil || withdrawn == 0 {
		return err
	}
	row.Funds = FundsWithdrawn
	if err := s.save(ctx, db.New(tx), *row, "funds_withdrawn", fmt.Sprintf("%d taken from the merchant", withdrawn)); err != nil {
		return err
	}
	return s.publish(ctx, tx, *row, events.TypeDisputeFundsWithdrawn)
}

func insert(ctx context.Context, q *db.Queries, row db.DisputesDispute, now time.Time) error {
	return q.InsertDispute(ctx, db.InsertDisputeParams{
		ID: row.ID, MerchantID: row.MerchantID, Livemode: row.Livemode, PaymentIntent: row.PaymentIntent, AttemptID: row.AttemptID,
		Kind: row.Kind, Network: row.Network, NetworkID: row.NetworkID, NetworkTransactionID: row.NetworkTransactionID,
		Reason: row.Reason, ReasonCode: row.ReasonCode, Amount: row.Amount, Currency: row.Currency, Stage: row.Stage,
		Status: row.Status, Liability: row.Liability, DueBy: row.DueBy, NetworkDueBy: row.NetworkDueBy,
		AuthorizedAt: row.AuthorizedAt, NotifiedAt: row.NotifiedAt, ContestedAt: row.ContestedAt,
		PendingAction: row.PendingAction, NetworkVersion: row.NetworkVersion, Now: ts(now),
	})
}

// setDeadlines sets the network's deadline and the merchant's, a margin earlier: or the
// network's own, when the margin has passed.
func (s *Service) setDeadlines(row *db.DisputesDispute, respondBy time.Time) {
	row.NetworkDueBy = ts(respondBy)
	due := rules.For(row.Network, rules.MerchantMargin, s.cfg.Now()).Before(respondBy)
	if !due.After(s.cfg.Now()) {
		due = respondBy
	}
	row.DueBy = ts(due)
}

func versionOf(n Notice) int32 {
	return int32(min(max(n.Version, 0), 1<<30))
}

// moveOn applies a change the network made to a case Jupiter has.
func (s *Service) moveOn(ctx context.Context, tx pgx.Tx, row db.DisputesDispute, n Notice) error {
	row.NetworkVersion = versionOf(n)
	if row.Status == Won || row.Status == Lost {
		return s.save(ctx, db.New(tx), row, "", "") // closed already, by Jupiter's acceptance
	}
	switch {
	case n.Status == networkClosed:
		return s.closeCard(ctx, tx, row, n.Outcome)
	case n.Status == networkOpen && n.Stage == StagePreArbitration:
		row.Stage, row.Status, row.EvidenceSubmittedAt = StagePreArbitration, NeedsResponse, pgtype.Timestamptz{}
		s.setDeadlines(&row, n.RespondBy)
		if err := s.save(ctx, db.New(tx), row, "pre_arbitration", "the issuer rejected the representment"); err != nil {
			return err
		}
		return s.publish(ctx, tx, row, events.TypeDisputeUpdated)
	case n.Status == networkResponded:
		changed := row.Stage != n.Stage || row.Status != UnderReview
		row.Stage, row.Status, row.DueBy, row.NetworkDueBy = n.Stage, UnderReview, pgtype.Timestamptz{}, pgtype.Timestamptz{}
		if !changed {
			return s.save(ctx, db.New(tx), row, "", "")
		}
		with := "with the issuer"
		if n.Stage == StageArbitration {
			with = "with the network"
		}
		if err := s.save(ctx, db.New(tx), row, n.Stage, with); err != nil {
			return err
		}
		return s.publish(ctx, tx, row, events.TypeDisputeUpdated)
	}
	return s.save(ctx, db.New(tx), row, "", "")
}

// closeCard ends a card dispute as the network ruled, giving the merchant back what was
// withdrawn if the acquirer won.
func (s *Service) closeCard(ctx context.Context, tx pgx.Tx, row db.DisputesDispute, outcome string) error {
	status, why := Lost, "issuer_won"
	if outcome == acquirerWon {
		status, why = Won, "acquirer_won"
	}
	if row.Liability == "scheme" && status == Won {
		why = "liability_cap"
	}
	s.close(&row, status, why)
	if err := s.save(ctx, db.New(tx), row, status, why); err != nil {
		return err
	}
	if err := s.publish(ctx, tx, row, events.TypeDisputeClosed); err != nil {
		return err
	}
	if status == Won && row.Funds == FundsWithdrawn {
		return s.reinstate(ctx, tx, row)
	}
	return nil
}

func (s *Service) reinstate(ctx context.Context, tx pgx.Tx, row db.DisputesDispute) error {
	back, err := s.cfg.Payments.ReinstateDisputed(ctx, tx, row.ID)
	if err != nil || back == 0 {
		return err
	}
	row.Funds = FundsReinstated
	if err := s.save(ctx, db.New(tx), row, "funds_reinstated", fmt.Sprintf("%d given back to the merchant", back)); err != nil {
		return err
	}
	return s.publish(ctx, tx, row, events.TypeDisputeFundsReinstated)
}

// sendCard sends a card dispute's pending action and applies the network's answer.
func (s *Service) sendCard(ctx context.Context, pool *pgxpool.Pool, row db.DisputesDispute) error {
	network, err := s.network(row.Livemode)
	if err != nil {
		return err
	}
	var evidence Evidence
	if err := json.Unmarshal(row.Evidence, &evidence); err != nil {
		return err
	}
	a := Action{NetworkID: row.NetworkID, Kind: row.PendingAction, Stage: row.Stage, Evidence: evidence.text()}
	if a.Kind == actionRepresentCapped {
		a.Kind, a.Reason = actionRepresent, rules.LiabilityCap
	}
	n, err := network.Act(ctx, a)
	if errors.Is(err, ErrRefused) {
		// Too late, or out of step: what the network has now settles it.
		s.cfg.Logger.WarnContext(ctx, "the network refused a dispute action", "dispute", row.ID, "action", a.Kind, "error", truncate(err.Error()))
		if n, err = network.Case(ctx, row.NetworkID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		current, err := q.LockDispute(ctx, row.ID)
		if err != nil {
			return err
		}
		if current.PendingAction == row.PendingAction {
			current.PendingAction, current.ActionError, current.ActionRetryAt = "", "", pgtype.Timestamptz{}
			if err := s.save(ctx, q, current, "sent", a.Kind+" at "+a.Stage); err != nil {
				return err
			}
		}
		if int(current.NetworkVersion) >= n.Version {
			return nil
		}
		return s.moveOn(ctx, tx, current, n)
	})
}

// refreshCard asks the network for a case and applies what changed.
func (s *Service) refreshCard(ctx context.Context, pool *pgxpool.Pool, row db.DisputesDispute) error {
	network, err := s.network(row.Livemode)
	if err != nil {
		return err
	}
	n, err := network.Case(ctx, row.NetworkID)
	if err != nil {
		return err
	}
	return s.ApplyNotice(ctx, pool, row.Livemode, n)
}

// casesLookback is how far back each pass reads the network's cases.
const casesLookback = 14 * 24 * time.Hour

// ReconcileCases reads from the live network the cases that changed in the last
// fortnight and applies them: chargebacks whose notice was lost are opened, and changes
// missed are applied. It answers how many it read.
func (s *Service) ReconcileCases(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	network, err := s.network(true)
	if errors.Is(err, ErrNoNetwork) {
		return 0, nil
	}
	cases, err := network.Cases(ctx, s.cfg.Now().Add(-casesLookback))
	if err != nil {
		return 0, err
	}
	var failures []error
	for _, n := range cases {
		if err := s.ApplyNotice(ctx, pool, true, n); err != nil {
			failures = append(failures, fmt.Errorf("case %s: %w", n.NetworkID, err))
		}
	}
	return len(cases), errors.Join(failures...)
}
