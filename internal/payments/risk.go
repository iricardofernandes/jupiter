package payments

import (
	"context"
	"net/netip"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/risk"
)

func validIP(s string) bool {
	addr, err := netip.ParseAddr(s)
	return err == nil && addr.Zone() == ""
}

// canonicalIP writes an address one way, so that one customer is one key: a mapped IPv4
// address as IPv4, an IPv6 one compressed. Anything else is no address.
func canonicalIP(s string) string {
	addr, err := netip.ParseAddr(s)
	if err != nil || addr.Zone() != "" {
		return ""
	}
	return addr.Unmap().String()
}

// decide asks the risk engine about a new attempt and applies its decision. Block ends
// the attempt before any rail. Request 3-D Secure, or 3-D Secure the merchant asked for,
// sends a customer-initiated payment to authentication first. Review and allow go on to
// the authorization; review is recorded for the merchant to look at.
func (s *Service) decide(ctx context.Context, tx pgx.Tx, owner Owner, row *db.PaymentsIntent, attempt *db.PaymentsAttempt, offSession bool) (Step, error) {
	action := risk.Allow
	if s.cfg.Risk != nil {
		in, err := s.riskInput(ctx, tx, owner, *row, *attempt, offSession)
		if err != nil {
			return StepDone, err
		}
		d, err := s.cfg.Risk.Decide(ctx, tx, in)
		if err != nil {
			return StepDone, err
		}
		attempt.RiskDecision, attempt.RiskDecisionID, action = string(d.Action), d.ID, d.Action
		row.RiskDecision, row.RiskDecisionID = attempt.RiskDecision, attempt.RiskDecisionID
	}
	switch {
	case action == risk.Block:
		attempt.Status, attempt.DeclineCode = string(attemptDeclined), declineBlocked
		return StepDone, s.fail(ctx, tx, row, "card_declined", declineBlocked, "The payment was blocked as likely fraudulent.")
	case offSession:
		// A merchant-initiated payment has no cardholder present to authenticate.
		return StepConfirm, nil
	case action == risk.Request3DS || row.RequestThreeDSecure == ThreeDSAny:
		return s.requestAuthentication(ctx, tx, owner, row, attempt)
	}
	return StepConfirm, nil
}

const declineBlocked = "blocked_by_risk"

// requestAuthentication sends the attempt to 3-D Secure where the rail has it. Test mode
// has no directory server: the attempt waits for the test helper, as a challenge would.
func (s *Service) requestAuthentication(ctx context.Context, tx pgx.Tx, owner Owner, row *db.PaymentsIntent, attempt *db.PaymentsAttempt) (Step, error) {
	if !owner.Livemode {
		attempt.Status = string(attemptRequiresAction)
		row.NextAction = "use_test_authentication"
		return StepDone, s.setStatus(ctx, tx, row, RequiresAction)
	}
	if s.cfg.Authenticator == nil {
		return StepConfirm, nil
	}
	attempt.Status = string(attemptAuthenticating)
	return StepAuthenticate, nil
}

func (s *Service) riskInput(ctx context.Context, q db.DBTX, owner Owner, row db.PaymentsIntent, attempt db.PaymentsAttempt, offSession bool) (risk.Input, error) {
	in := risk.Input{
		Owner: risk.Owner{Merchant: owner.Merchant, Livemode: owner.Livemode}, Attempt: attempt.ID, Intent: row.ID,
		Amount: row.Amount, Currency: row.Currency, IP: attempt.Ip, OffSession: offSession,
	}
	if row.Installments.Valid {
		in.Installments = int(row.Installments.Int32)
	}
	if knownPaymentMethod(row.PaymentMethod) {
		// A test payment method stands for a new card each time: it has no history.
		scheme, err := s.schemeOf(ctx, q, owner, row.PaymentMethod)
		if err != nil {
			return risk.Input{}, err
		}
		in.Brand, in.BIN = scheme, testBINs[scheme]
		in.CardFingerprint, in.MerchantFingerprint = "test:"+row.ID, "test:"+row.ID
		return in, nil
	}
	pm, err := s.methodRow(ctx, q, owner, row.PaymentMethod)
	if err != nil {
		return risk.Input{}, err
	}
	in.Brand, in.BIN, in.CardIP = pm.Brand, pm.Bin, pm.ClientIp
	in.CardFingerprint, in.MerchantFingerprint = pm.VaultFingerprint, merchantFingerprint(pm.VaultFingerprint, pm.MerchantID)
	return in, nil
}

var testBINs = map[string]string{"visa": "42424242", "mastercard": "55555555"}

// recordOutcome tells the risk engine how an attempt's authorization ended.
func (s *Service) recordOutcome(ctx context.Context, tx pgx.Tx, attemptID string, approved bool) error {
	if s.cfg.Risk == nil {
		return nil
	}
	return s.cfg.Risk.RecordOutcome(ctx, tx, attemptID, approved)
}
