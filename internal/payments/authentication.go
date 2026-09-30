package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
)

// Authenticator is 3-D Secure as payments uses it: it asks the card's directory server
// whether the cardholder is who they say, and the issuer answers at once
// (frictionless) or asks for a challenge.
type Authenticator interface {
	Authenticate(ctx context.Context, r AuthenticationRequest) Authentication
}

type AuthenticationRequest struct {
	// Key identifies the attempt; the authenticator keeps it with its own transaction.
	Key       string
	Intent    string
	Owner     Owner
	Amount    money.Amount
	Card      *CardReference
	ReturnURL string
}

type AuthenticationOutcome string

const (
	// Authenticated (Y) and Attempted (A) go on to the authorization, with the
	// authentication value; liability for fraud moves to the issuer.
	Authenticated AuthenticationOutcome = "authenticated"
	Attempted     AuthenticationOutcome = "attempted"
	// Challenge (C) sends the cardholder to the issuer's page.
	Challenge AuthenticationOutcome = "challenge"
	// NotAuthenticated (N) and Rejected (R) fail the attempt.
	NotAuthenticated AuthenticationOutcome = "not_authenticated"
	Rejected         AuthenticationOutcome = "rejected"
	// AuthenticationUnavailable (U) goes on without authentication, and liability stays
	// with the merchant.
	AuthenticationUnavailable AuthenticationOutcome = "unavailable"
	// AuthenticationError is no answer: the attempt stays authenticating for the resolver
	// to try again, and fails once it has waited GiveUpAfter.
	AuthenticationError AuthenticationOutcome = "error"
)

// Authentication is what 3-D Secure answered. For a challenge, ChallengeURL is where the
// customer goes; its result arrives later (CompleteChallenge).
type Authentication struct {
	Outcome             AuthenticationOutcome
	Version             string
	TransStatus         string
	ServerTransID       string
	DSTransID           string
	ACSTransID          string
	ECI                 string
	AuthenticationValue string
	ChallengeURL        string
}

func (a Authentication) proceeds() bool {
	return a.Outcome == Authenticated || a.Outcome == Attempted
}

// Authenticate sends an authenticating attempt to 3-D Secure. It runs outside any
// transaction.
func (s *Service) Authenticate(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID, returnURL string) (Authentication, error) {
	intent, attempt, err := s.current(ctx, db.New(q), owner, intentID)
	if err != nil {
		return Authentication{}, err
	}
	if !inStatus(attempt, attemptAuthenticating) {
		return Authentication{}, nil
	}
	if s.cfg.Authenticator == nil {
		// 3-D Secure was switched off since the attempt started.
		return Authentication{Outcome: AuthenticationUnavailable}, nil
	}
	amount, err := money.New(attempt.Amount, mustCurrency(intent.Currency))
	if err != nil {
		return Authentication{}, err
	}
	card, err := s.cardFor(ctx, q, owner, attempt.PaymentMethod)
	if err != nil {
		return Authentication{}, err
	}
	return s.cfg.Authenticator.Authenticate(ctx, AuthenticationRequest{
		Key: attempt.ID, Intent: intent.ID, Owner: owner, Amount: amount, Card: card, ReturnURL: returnURL,
	}), nil
}

// FinishAuthentication applies 3-D Secure's first answer. It changes nothing unless the
// attempt is still authenticating, so it may be repeated.
func (s *Service) FinishAuthentication(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, a Authentication) (Intent, Step, error) {
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
	if err != nil || a.Outcome == "" || a.Outcome == AuthenticationError || !inStatus(attempt, attemptAuthenticating) {
		return s.finish(ctx, q, row, err)
	}
	record(&attempt, a)
	step := StepDone
	switch a.Outcome {
	case Challenge:
		attempt.Status = string(attemptRequiresAction)
		row.NextAction, row.NextActionUrl = "redirect_to_url", a.ChallengeURL
		err = s.setStatus(ctx, tx, &row, RequiresAction)
	default:
		step, err = s.afterAuthentication(ctx, tx, &row, &attempt, a)
	}
	if err != nil {
		return Intent{}, StepDone, err
	}
	attempt.UpdatedAt = ts(s.cfg.Now().UTC())
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return Intent{}, StepDone, err
	}
	it, err := s.save(ctx, q, row)
	return it, step, err
}

// CompleteChallenge applies the result of a challenge, which the directory server
// reports on its own (the RReq). It returns the intent and whether to go on to the
// authorization.
func (s *Service) CompleteChallenge(ctx context.Context, tx pgx.Tx, serverTransID string, a Authentication) (Intent, Step, error) {
	q := db.New(tx)
	found, err := q.AttemptByServerTransaction(ctx, serverTransID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, StepDone, fmt.Errorf("%w: no attempt for 3-D Secure transaction %s", ErrNotFound, serverTransID)
	}
	if err != nil {
		return Intent{}, StepDone, err
	}
	intent, err := q.LockIntentByID(ctx, found.IntentID)
	if err != nil {
		return Intent{}, StepDone, err
	}
	attempt, err := q.LockAttempt(ctx, found.ID)
	if err != nil {
		return Intent{}, StepDone, err
	}
	if !inStatus(attempt, attemptRequiresAction) || intent.LatestAttempt.String != attempt.ID || Status(intent.Status) != RequiresAction {
		it, err := intentFromRow(intent)
		return it, StepDone, err
	}
	a.ServerTransID, a.ChallengeURL = attempt.ThreeDsServerTransID, ""
	record(&attempt, a)
	intent.NextAction, intent.NextActionUrl = "", ""
	step, err := s.afterAuthentication(ctx, tx, &intent, &attempt, a)
	if err != nil {
		return Intent{}, StepDone, err
	}
	attempt.UpdatedAt = ts(s.cfg.Now().UTC())
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return Intent{}, StepDone, err
	}
	it, err := s.save(ctx, q, intent)
	return it, step, err
}

// afterAuthentication goes on to the authorization, or fails the attempt.
func (s *Service) afterAuthentication(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt, a Authentication) (Step, error) {
	switch {
	case a.proceeds(), a.Outcome == AuthenticationUnavailable:
		attempt.Status, attempt.Authenticated = string(attemptAuthorizing), a.proceeds()
		// The authorization starts now: time spent authenticating does not count towards
		// giving up on it.
		attempt.UnknownSince = ts(s.cfg.Now().UTC())
		if Status(row.Status) != Processing {
			if err := s.setStatus(ctx, tx, row, Processing); err != nil {
				return StepDone, err
			}
		}
		return StepConfirm, nil
	default:
		attempt.Status, attempt.DeclineCode = string(attemptFailed), "authentication_failed"
		if err := s.recordOutcome(ctx, tx, attempt.ID, false); err != nil {
			return StepDone, err
		}
		return StepDone, s.fail(ctx, tx, row, "payment_intent_authentication_failure", "", "The cardholder failed 3-D Secure authentication.")
	}
}

// giveUpAuthentication fails an attempt 3-D Secure could not be reached for since it
// started, GiveUpAfter ago.
func (s *Service) giveUpAuthentication(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID) error {
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
	if err != nil || !inStatus(attempt, attemptAuthenticating) || !s.overdue(attempt) {
		return err
	}
	attempt.Status, attempt.DeclineCode = string(attemptFailed), "authentication_unavailable"
	attempt.UpdatedAt = ts(s.cfg.Now().UTC())
	if err := s.recordOutcome(ctx, tx, attempt.ID, false); err != nil {
		return err
	}
	if err := s.fail(ctx, tx, &row, "payment_intent_authentication_failure", "", "3-D Secure could not be reached to authenticate the cardholder."); err != nil {
		return err
	}
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return err
	}
	_, err = s.save(ctx, q, row)
	return err
}

func record(attempt *db.PaymentsAttempt, a Authentication) {
	attempt.ThreeDsVersion, attempt.ThreeDsStatus = a.Version, a.TransStatus
	if a.ServerTransID != "" {
		attempt.ThreeDsServerTransID = a.ServerTransID
	}
	if a.DSTransID != "" {
		attempt.DsTransID, attempt.AcsTransID = a.DSTransID, a.ACSTransID
	}
	attempt.AcsUrl, attempt.Eci, attempt.AuthenticationValue = a.ChallengeURL, a.ECI, a.AuthenticationValue
	attempt.LiabilityShift = a.proceeds() && a.AuthenticationValue != ""
}

// authenticationOf is what the rail forwards with the authorization.
func authenticationOf(attempt db.PaymentsAttempt) *AuthenticationData {
	if attempt.AuthenticationValue == "" {
		return nil
	}
	return &AuthenticationData{
		Version: attempt.ThreeDsVersion, ECI: attempt.Eci, Value: attempt.AuthenticationValue, DSTransID: attempt.DsTransID,
	}
}

// AuthenticationData is the 3-D Secure result an authorization carries to the issuer.
type AuthenticationData struct {
	Version   string
	ECI       string
	Value     string
	DSTransID string
}
