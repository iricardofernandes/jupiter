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

// A MED claim (Mecanismo Especial de Devolução, as MED 2.0 has it since February 2026):
// the payer contested a Pix as fraud in its bank's app; its bank reported the infraction
// to Jupiter's bank, which tells Jupiter. Jupiter holds the claim's amount on the
// merchant's balance, as far as the balance has it, and traces the rest through the
// merchant's payouts since the Pix. The merchant answers within days: accepting, or with
// evidence an operator weighs. Jupiter then agrees, and the bank returns the money to the
// payer, or disagrees, and the hold ends. A claim the merchant leaves unanswered is agreed
// with; one it answered that no one decides before the block ends is disagreed with. A
// merchant whose money was returned may contest the return, within the window IN BCB
// 766/2026 sets.

// Infraction statuses and analysis results, as the bank names them (after DICT).
const (
	infractionOpen   = "OPEN"
	infractionClosed = "CLOSED"
	agreed           = "AGREED"
	disagreed        = "DISAGREED"
	refundDone       = "DEVOLVIDO"
	refundFailed     = "NAO_REALIZADO"
	contestPending   = "PENDING"
	contestUpheld    = "UPHELD"
	contestRejected  = "REJECTED"
)

// Actions Jupiter takes at the bank.
const (
	actionAgree    = "agree"
	actionDisagree = "disagree"
	actionContest  = "contest"
)

// Infraction is a MED claim as the bank has it.
type Infraction struct {
	ID           string
	EndToEndID   string
	Amount       int64
	Reason       string
	Details      string
	Status       string
	Result       string
	ContestedAt  time.Time
	NotifiedAt   time.Time
	RefundID     string
	RefundStatus string
	Refunded     int64
	Contestation string
}

// Analysis is Jupiter's answer to a claim: agreed, with the return it asks the bank to
// make, or disagreed; with where the money it could not hold went.
type Analysis struct {
	Result       string
	Details      string
	RefundID     string
	RefundAmount int64
	Trace        []Hop
}

// ApplyInfraction applies what the bank says of a MED claim: a new one opens a dispute
// and holds its amount, a later change moves it on.
func (s *Service) ApplyInfraction(ctx context.Context, pool *pgxpool.Pool, livemode bool, inf Infraction) error {
	if inf.ID == "" || inf.EndToEndID == "" || inf.Amount <= 0 {
		return fmt.Errorf("%w: a claim names itself, a Pix and an amount", ErrInvalid)
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.LockByNetworkID(ctx, db.LockByNetworkIDParams{Livemode: livemode, Kind: KindMED, NetworkID: inf.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return s.openMED(ctx, tx, livemode, inf)
		}
		if err != nil {
			return err
		}
		return s.moveOnMED(ctx, tx, row, inf)
	})
}

// openMED opens a dispute for a claim on a Pix of Jupiter's, holds what the merchant's
// balance has of it, and traces the rest. A claim on a Pix Jupiter does not know is
// logged and left to the bank.
func (s *Service) openMED(ctx context.Context, tx pgx.Tx, livemode bool, inf Infraction) error {
	p, err := s.cfg.Payments.DisputedPaymentByNetworkID(ctx, tx, livemode, inf.EndToEndID)
	if errors.Is(err, payments.ErrNotFound) {
		s.cfg.Logger.WarnContext(ctx, "a MED claim on a Pix that paid nothing of Jupiter's", "infraction", inf.ID)
		return nil
	}
	if err != nil {
		return err
	}
	now := s.cfg.Now().UTC()
	notified := inf.NotifiedAt
	if notified.IsZero() {
		notified = now
	}
	row := db.DisputesDispute{
		ID: DisputePrefix.New().String(), MerchantID: p.Owner.Merchant.String(), Livemode: livemode, PaymentIntent: p.Intent.String(),
		AttemptID: p.Attempt, Kind: KindMED, Network: rules.Pix, NetworkID: inf.ID, NetworkTransactionID: inf.EndToEndID,
		Reason: "fraudulent", ReasonCode: truncateTo(inf.Reason, 16), Amount: inf.Amount, Currency: p.Captured.Currency().Code(),
		Stage: StageMEDAnalysis, Status: NeedsResponse, Liability: "merchant", AuthorizedAt: ts(p.At), NotifiedAt: ts(notified),
		ContestedAt: ts(inf.ContestedAt), Evidence: []byte("{}"), Trace: []byte("[]"), Funds: FundsNone,
		DueBy:        ts(rules.For(rules.Pix, rules.MEDResponse, notified).Due(notified)),
		NetworkDueBy: ts(rules.For(rules.Pix, rules.MEDBlock, notified).Due(notified)),
	}
	contested := inf.ContestedAt
	if contested.IsZero() {
		contested = notified
	}
	late := contested.After(rules.For(rules.Pix, rules.MEDRequest, p.At).Due(p.At))
	if late {
		// The payer asked after the window: Jupiter disagrees at once and holds nothing.
		row.Status, row.PendingAction, row.DueBy = UnderReview, actionDisagree, pgtype.Timestamptz{}
	}
	if err := insert(ctx, db.New(tx), row, now); err != nil {
		return err
	}
	detail := fmt.Sprintf("MED claim %s for %d on Pix %s", inf.ID, inf.Amount, inf.EndToEndID)
	if late {
		detail += ", asked for after the window"
	}
	if err := db.New(tx).InsertHistory(ctx, db.InsertHistoryParams{DisputeID: row.ID, At: ts(now), Kind: "opened", Detail: detail}); err != nil {
		return err
	}
	if err := s.publish(ctx, tx, row, events.TypeDisputeCreated); err != nil {
		return err
	}
	if late {
		return nil
	}
	return s.hold(ctx, tx, row, p)
}

// hold holds the claim on the merchant's balance, as far as it has it, and traces the
// rest through the merchant's payouts since the Pix arrived.
func (s *Service) hold(ctx context.Context, tx pgx.Tx, row db.DisputesDispute, p payments.DisputedPayment) error {
	amount, err := money.New(row.Amount, p.Captured.Currency())
	if err != nil {
		return err
	}
	held, err := s.cfg.Payments.HoldDisputed(ctx, tx, row.ID, p, amount)
	if err != nil {
		return err
	}
	if held > 0 {
		row.Funds, row.Blocked, row.BlockedAt = FundsHeld, held, ts(s.cfg.Now().UTC())
	}
	if rest := row.Amount - held; rest > 0 {
		trace, err := s.trace(ctx, tx, p, rest)
		if err != nil {
			return err
		}
		if row.Trace, err = json.Marshal(trace); err != nil {
			return err
		}
	}
	if err := s.save(ctx, db.New(tx), row, "funds_held", fmt.Sprintf("%d held on the merchant's balance", held)); err != nil {
		return err
	}
	if held == 0 {
		return nil
	}
	return s.publish(ctx, tx, row, events.TypeDisputeFundsWithdrawn)
}

// trace follows up to rest through the merchant's payouts since the Pix: the next
// accounts the bank can follow the money to.
func (s *Service) trace(ctx context.Context, tx pgx.Tx, p payments.DisputedPayment, rest int64) ([]Hop, error) {
	payouts, err := s.cfg.Payments.PaidOutSince(ctx, tx, p.Owner, p.Captured.Currency(), p.At)
	if err != nil {
		return nil, err
	}
	out := []Hop{}
	for _, po := range payouts {
		if rest <= 0 {
			break
		}
		amount := min(rest, po.Amount.Minor())
		out = append(out, Hop{Payout: po.ID.String(), EndToEndID: po.EndToEndID, Amount: amount})
		rest -= amount
	}
	return out, nil
}

// returned takes what the bank returned to the payer: the hold, and what it returned
// beyond it. A claim whose payment can no longer be read still has its hold settled.
func (s *Service) returned(ctx context.Context, tx pgx.Tx, row *db.DisputesDispute, inf Infraction) error {
	if inf.Refunded <= row.Blocked {
		if err := s.cfg.Payments.SettleDisputeHold(ctx, tx, row.ID, true); err != nil {
			return err
		}
		if row.Funds == FundsHeld {
			row.Funds = FundsWithdrawn
		}
		return nil
	}
	p, err := s.cfg.Payments.DisputedPaymentByNetworkID(ctx, tx, row.Livemode, row.NetworkTransactionID)
	if err != nil {
		return err
	}
	// The bank cannot have returned more than the claim was for.
	taken, err := s.cfg.Payments.ReturnedOnMED(ctx, tx, row.ID, p, min(inf.Refunded, row.Amount))
	if err == nil && taken > 0 {
		row.Funds = FundsWithdrawn
	}
	return err
}

// moveOnMED applies a change the bank made to a claim: the return it was asked for made,
// the claim disagreed with, a contestation decided.
func (s *Service) moveOnMED(ctx context.Context, tx pgx.Tx, row db.DisputesDispute, inf Infraction) error {
	if row.Stage == StageMEDContestation {
		return s.contested(ctx, tx, row, inf)
	}
	switch {
	case row.Stage != StageMEDAnalysis:
		return nil
	case inf.Result == agreed && inf.RefundStatus == refundDone && row.Status != Lost:
		if err := s.returned(ctx, tx, &row, inf); err != nil {
			return err
		}
		if row.Outcome == "" {
			row.Outcome = "agreed"
		}
		return s.closeMED(ctx, tx, row, Lost, row.Outcome, fmt.Sprintf("the bank returned %d to the payer", inf.Refunded))
	case (inf.Result == disagreed || (inf.Result == agreed && inf.RefundStatus == refundFailed)) && row.Status != Won:
		if err := s.cfg.Payments.SettleDisputeHold(ctx, tx, row.ID, false); err != nil {
			return err
		}
		if row.Funds == FundsHeld {
			row.Funds = FundsReleased
		}
		switch {
		case inf.Result == agreed:
			row.Outcome = "return_failed"
		case row.Outcome == "":
			row.Outcome = "disagreed"
		}
		return s.closeMED(ctx, tx, row, Won, row.Outcome, "the hold ended; nothing was returned")
	}
	return nil
}

// contested applies the payer's bank's decision on a contestation of a MED return.
func (s *Service) contested(ctx context.Context, tx pgx.Tx, row db.DisputesDispute, inf Infraction) error {
	if row.Status != UnderReview {
		return nil
	}
	switch inf.Contestation {
	case contestUpheld:
		row.Status, row.Outcome, row.ClosedAt = Won, "contestation_upheld", ts(s.cfg.Now().UTC())
		if err := s.save(ctx, db.New(tx), row, Won, "the payer's bank gave the return back"); err != nil {
			return err
		}
		if err := s.publish(ctx, tx, row, events.TypeDisputeClosed); err != nil {
			return err
		}
		return s.reinstate(ctx, tx, row)
	case contestRejected:
		row.Status, row.Outcome, row.ClosedAt = Lost, "contestation_rejected", ts(s.cfg.Now().UTC())
		if err := s.save(ctx, db.New(tx), row, Lost, "the payer's bank rejected the contestation"); err != nil {
			return err
		}
		return s.publish(ctx, tx, row, events.TypeDisputeClosed)
	}
	return nil
}

func (s *Service) closeMED(ctx context.Context, tx pgx.Tx, row db.DisputesDispute, status, outcome, detail string) error {
	s.close(&row, status, outcome)
	if err := s.save(ctx, db.New(tx), row, status, detail); err != nil {
		return err
	}
	if err := s.publish(ctx, tx, row, events.TypeDisputeClosed); err != nil {
		return err
	}
	if row.Funds == FundsReleased {
		return s.publish(ctx, tx, row, events.TypeDisputeFundsReinstated)
	}
	return nil
}

// sendMED sends a claim's pending answer to the bank and applies what it says.
func (s *Service) sendMED(ctx context.Context, pool *pgxpool.Pool, row db.DisputesDispute) error {
	bank, err := s.bank(row.Livemode)
	if err != nil {
		return err
	}
	var inf Infraction
	switch row.PendingAction {
	case actionAgree, actionDisagree:
		a := Analysis{Result: disagreed, Details: row.Outcome}
		if row.PendingAction == actionAgree {
			a.Result, a.RefundID, a.RefundAmount = agreed, refundID(row.ID), row.Blocked
		}
		if err := json.Unmarshal(row.Trace, &a.Trace); err != nil {
			return err
		}
		inf, err = bank.Analyze(ctx, row.NetworkID, a)
	case actionContest:
		var evidence Evidence
		if err := json.Unmarshal(row.Evidence, &evidence); err != nil {
			return err
		}
		inf, err = bank.Contest(ctx, row.NetworkID, evidence.text())
	default:
		return fmt.Errorf("disputes: unknown action %q", row.PendingAction)
	}
	refused := errors.Is(err, ErrRefused)
	if refused {
		s.cfg.Logger.WarnContext(ctx, "the bank refused an answer to a MED claim", "dispute", row.ID, "action", row.PendingAction, "error", truncate(err.Error()))
		inf, err = bank.Infraction(ctx, row.NetworkID)
	}
	if err != nil {
		return err
	}
	if refused && row.PendingAction == actionContest && inf.Contestation == "" {
		return s.contestRefused(ctx, pool, row)
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		current, err := q.LockDispute(ctx, row.ID)
		if err != nil {
			return err
		}
		if current.PendingAction == row.PendingAction {
			current.PendingAction, current.ActionError, current.ActionRetryAt = "", "", pgtype.Timestamptz{}
			if err := s.save(ctx, q, current, "sent", row.PendingAction); err != nil {
				return err
			}
		}
		return s.moveOnMED(ctx, tx, current, inf)
	})
}

// refundID is the return Jupiter asks the bank to make for a claim: the dispute's id,
// without its underscore, as the API Pix's ids allow.
func refundID(disputeID string) string {
	out := make([]byte, 0, len(disputeID))
	for i := range len(disputeID) {
		if disputeID[i] != '_' {
			out = append(out, disputeID[i])
		}
	}
	return string(out)
}

// refreshMED asks the bank for a claim and applies what changed.
func (s *Service) refreshMED(ctx context.Context, pool *pgxpool.Pool, row db.DisputesDispute) error {
	bank, err := s.bank(row.Livemode)
	if err != nil {
		return err
	}
	inf, err := bank.Infraction(ctx, row.NetworkID)
	if err != nil {
		return err
	}
	return s.ApplyInfraction(ctx, pool, row.Livemode, inf)
}

// medLookback is how far back each pass reads the bank's claims: what notifications
// missed.
const medLookback = 14 * 24 * time.Hour

// ReconcileMED reads from each bank the claims of the last fortnight and applies them,
// and answers how many it read.
func (s *Service) ReconcileMED(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	read := 0
	var failures []error
	now := s.cfg.Now().UTC()
	for _, livemode := range []bool{false, true} {
		bank, err := s.bank(livemode)
		if err != nil {
			continue
		}
		claims, err := bank.Infractions(ctx, now.Add(-medLookback), now.Add(time.Second))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, inf := range claims {
			if err := s.ApplyInfraction(ctx, pool, livemode, inf); err != nil {
				failures = append(failures, fmt.Errorf("claim %s: %w", inf.ID, err))
				continue
			}
			read++
		}
	}
	return read, errors.Join(failures...)
}

// contestRefused puts back a lost claim whose contestation the bank would not take: it
// stays lost.
func (s *Service) contestRefused(ctx context.Context, pool *pgxpool.Pool, row db.DisputesDispute) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		current, err := q.LockDispute(ctx, row.ID)
		if err != nil || current.PendingAction != actionContest {
			return err
		}
		current.Stage, current.Status, current.Outcome = StageMEDAnalysis, Lost, "contestation_refused"
		current.PendingAction, current.ActionError, current.ActionRetryAt = "", "", pgtype.Timestamptz{}
		if err := s.save(ctx, q, current, Lost, "the bank would not take the contestation"); err != nil {
			return err
		}
		return s.publish(ctx, tx, current, events.TypeDisputeClosed)
	})
}
