package disputes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/disputes/db"
	"github.com/iricardofernandes/jupiter/internal/disputes/rules"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// TestNetwork stands for the card networks in test mode, where no network is reached. It
// keeps its cases in Jupiter's database, takes its deadlines from Jupiter's table, and
// plays the issuer from the evidence it is sent:
//   - "winning_evidence" in a representment: the issuer accepts it, and the merchant wins;
//   - "losing_evidence": the issuer rejects it into pre-arbitration;
//   - anything else: the issuer stays silent, and the merchant wins when its time is up.
//
// At arbitration the network rules for the merchant only on "winning_evidence".
type TestNetwork struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var testCasePrefix = id.MustPrefix("tcb")

const (
	winningEvidence = "winning_evidence"
	losingEvidence  = "losing_evidence"
)

var _ Network = (*TestNetwork)(nil)

func NewTestNetwork(pool *pgxpool.Pool, now func() time.Time) *TestNetwork {
	if now == nil {
		now = time.Now
	}
	return &TestNetwork{pool: pool, now: now}
}

// open starts a chargeback on a test payment, as an issuer would.
func (n *TestNetwork) open(ctx context.Context, tx pgx.Tx, p payments.DisputedPayment, reasonCode string, amount int64) (Notice, error) {
	now := n.now().UTC()
	c := db.DisputesTestCase{
		ID: testCasePrefix.New().String(), NetworkTransactionID: p.NetworkID, Network: p.Scheme, Amount: amount,
		Currency: p.Captured.Currency().Code(), ReasonCode: reasonCode, Stage: StageChargeback, Status: networkOpen,
		RespondBy: ts(rules.For(p.Scheme, rules.Represent, now).Due(now)), AuthorizedAt: ts(p.At), OpenedAt: ts(now), Version: 1,
	}
	if err := db.New(tx).InsertTestCase(ctx, db.InsertTestCaseParams{
		ID: c.ID, NetworkTransactionID: c.NetworkTransactionID, Network: c.Network, Amount: c.Amount, Currency: c.Currency,
		ReasonCode: c.ReasonCode, Stage: c.Stage, Status: c.Status, RespondBy: c.RespondBy, AuthorizedAt: c.AuthorizedAt, OpenedAt: c.OpenedAt,
	}); err != nil {
		return Notice{}, err
	}
	return noticeOf(c), nil
}

func (n *TestNetwork) Act(ctx context.Context, a Action) (Notice, error) {
	var out Notice
	err := postgres.InTx(ctx, n.pool, func(tx pgx.Tx) error {
		c, err := n.load(ctx, tx, a.NetworkID)
		if err != nil {
			return err
		}
		if c.Status != networkOpen || c.Stage != a.Stage {
			out = noticeOf(c)
			return nil // taken already, or past: answered with the case as it is
		}
		now := n.now().UTC()
		switch {
		case a.Kind == actionAccept:
			c.Status, c.Outcome = networkClosed, issuerWon
		case a.Kind == actionRepresent && a.Reason == rules.LiabilityCap && c.OpenedAt.Time.After(rules.For(c.Network, rules.LiabilityCap, c.OpenedAt.Time).Due(c.AuthorizedAt.Time)):
			c.Status, c.Outcome = networkClosed, acquirerWon
		case a.Kind == actionRepresent && strings.Contains(a.Evidence, winningEvidence):
			c.Status, c.Outcome = networkClosed, acquirerWon
		case a.Kind == actionRepresent && strings.Contains(a.Evidence, losingEvidence):
			c.Stage, c.RespondBy = StagePreArbitration, ts(rules.For(c.Network, rules.PreArbitration, now).Due(now))
		case a.Kind == actionRepresent:
			c.Status, c.DecideBy = networkResponded, ts(rules.For(c.Network, rules.IssuerResponse, now).Due(now))
		case a.Kind == actionEscalate && c.Stage == StagePreArbitration:
			c.Stage, c.Status, c.EscalatedEvidence = StageArbitration, networkResponded, a.Evidence
			c.DecideBy = ts(rules.For(c.Network, rules.Arbitration, now).Due(now))
		default:
			return fmt.Errorf("%w: %s at %s", ErrRefused, a.Kind, c.Stage)
		}
		if c.Status != networkOpen {
			c.RespondBy = pgtype.Timestamptz{}
		}
		c.Version++
		out = noticeOf(c)
		return n.save(ctx, tx, c)
	})
	return out, err
}

// Cases lists nothing: test cases are opened in Jupiter's own transactions, and never
// missed.
func (n *TestNetwork) Cases(context.Context, time.Time) ([]Notice, error) { return nil, nil }

func (n *TestNetwork) Case(ctx context.Context, networkID string) (Notice, error) {
	var out Notice
	err := postgres.InTx(ctx, n.pool, func(tx pgx.Tx) error {
		c, err := n.load(ctx, tx, networkID)
		out = noticeOf(c)
		return err
	})
	return out, err
}

// load reads a case, locked, and closes it if a deadline passed: the acquirer's, which it
// loses, or the issuer's or network's, which settles it.
func (n *TestNetwork) load(ctx context.Context, tx pgx.Tx, networkID string) (db.DisputesTestCase, error) {
	q := db.New(tx)
	c, err := q.GetTestCase(ctx, networkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, fmt.Errorf("%w: no test case %s", ErrRefused, networkID)
	}
	if err != nil {
		return c, err
	}
	now := n.now()
	switch {
	case c.Status == networkOpen && now.After(c.RespondBy.Time):
		c.Status, c.Outcome = networkClosed, issuerWon
	case c.Status == networkResponded && now.After(c.DecideBy.Time) && c.Stage == StageArbitration:
		c.Status, c.Outcome = networkClosed, issuerWon
		if strings.Contains(c.EscalatedEvidence, winningEvidence) {
			c.Outcome = acquirerWon
		}
	case c.Status == networkResponded && now.After(c.DecideBy.Time):
		c.Status, c.Outcome = networkClosed, acquirerWon
	default:
		return c, nil
	}
	c.RespondBy, c.DecideBy = pgtype.Timestamptz{}, pgtype.Timestamptz{}
	c.Version++
	return c, n.save(ctx, tx, c)
}

func (n *TestNetwork) save(ctx context.Context, tx pgx.Tx, c db.DisputesTestCase) error {
	return db.New(tx).SaveTestCase(ctx, db.SaveTestCaseParams{
		ID: c.ID, Stage: c.Stage, Status: c.Status, Outcome: c.Outcome, RespondBy: c.RespondBy, DecideBy: c.DecideBy,
		EscalatedEvidence: c.EscalatedEvidence, Version: c.Version,
	})
}

func noticeOf(c db.DisputesTestCase) Notice {
	category := "consumer_dispute"
	if strings.HasPrefix(c.ReasonCode, "10.") || c.ReasonCode == "4837" {
		category = "fraud"
	}
	return Notice{
		NetworkID: c.ID, NetworkTransactionID: c.NetworkTransactionID, Amount: c.Amount, Currency: c.Currency,
		ReasonCode: c.ReasonCode, Category: category, Stage: c.Stage, Status: c.Status, Outcome: c.Outcome,
		RespondBy: c.RespondBy.Time, AuthorizedAt: c.AuthorizedAt.Time, OpenedAt: c.OpenedAt.Time, Version: int(c.Version),
	}
}

// CreateTestDispute has the test network open a chargeback on one of the merchant's test
// card payments, as an issuer would, and applies it: for amount, or all that is left.
func (s *Service) CreateTestDispute(ctx context.Context, tx pgx.Tx, owner payments.Owner, intentID id.ID, reasonCode string, amount int64) (Dispute, error) {
	if owner.Livemode || s.test == nil {
		return Dispute{}, fmt.Errorf("%w: test disputes are made in test mode", ErrInvalid)
	}
	if !validReasonCode(reasonCode) {
		return Dispute{}, fmt.Errorf("%w: reason_code is a Visa (10.4, 13.1, ...) or Mastercard (4837, 4855, ...) code", ErrInvalid)
	}
	p, err := s.cfg.Payments.DisputedPayment(ctx, tx, owner, intentID)
	if err != nil {
		return Dispute{}, err
	}
	if p.Method != payments.MethodCard {
		return Dispute{}, fmt.Errorf("%w: only a card payment has chargebacks", ErrInvalid)
	}
	if amount == 0 {
		amount = p.Disputable()
	}
	if amount <= 0 || amount > p.Disputable() {
		return Dispute{}, fmt.Errorf("%w: %d can be disputed", ErrInvalid, p.Disputable())
	}
	n, err := s.test.open(ctx, tx, p, reasonCode, amount)
	if err != nil {
		return Dispute{}, err
	}
	if err := s.applyNotice(ctx, tx, false, n); err != nil {
		return Dispute{}, err
	}
	row, err := db.New(tx).GetByNetworkID(ctx, db.GetByNetworkIDParams{Livemode: false, Kind: KindChargeback, NetworkID: n.NetworkID})
	if err != nil {
		return Dispute{}, err
	}
	disputeID, err := DisputePrefix.Parse(row.ID)
	if err != nil {
		return Dispute{}, err
	}
	return s.Get(ctx, tx, owner, disputeID)
}

func validReasonCode(code string) bool {
	if len(code) == 0 || len(code) > 8 {
		return false
	}
	return strings.Trim(code, "0123456789.") == "" && (strings.Count(code, ".") >= 1 || len(code) == 4)
}
