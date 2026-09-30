package payments

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

var railReferencePrefix = id.MustPrefix("rail")

// Magic amounts: the last two minor digits of an authorization on an approving card.
const (
	// The rail approves but the response is lost: the attempt's outcome is unknown until
	// a query finds the approval.
	magicResponseLost = 91
	// The first request never reaches the rail: a query finds nothing, and a retry with
	// the same key is approved.
	magicRequestLost = 92
	// The rail answers late: the caller times out, the first query finds the request
	// still pending, and a later one finds it approved.
	magicLateResponse = 93
)

const (
	railApproved       = "approved"
	railDeclined       = "declined"
	railRequiresAction = "requires_action"
	railLost           = "lost"
	railPending        = "pending"
	railVoided         = "voided"
)

// TestRail is the rail test mode uses. It remembers every request by idempotency key in
// its own table, as a real counterparty would, and repeats its answer to a repeated
// request. Cards and magic amounts choose the answer.
type TestRail struct {
	pool   *pgxpool.Pool
	now    func() time.Time
	logger *slog.Logger
}

func NewTestRail(pool *pgxpool.Pool, now func() time.Time, logger *slog.Logger) *TestRail {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &TestRail{pool: pool, now: now, logger: logger}
}

func (t *TestRail) Authorize(ctx context.Context, r AuthorizeRequest) Result {
	return t.do(ctx, "authorize", r.Key, "", r.Amount.Minor(), func(_ *db.Queries, op *db.PaymentsTestRail, first bool) Result {
		switch {
		case op.Status == railVoided:
			// Reversed before it was ever approved: a late arrival is refused.
		case op.Status == railRequiresAction && r.Authenticated:
			op.Status = railApproved
		case op.Status == railLost:
			op.Status = railApproved
		case !first:
		default:
			decideAuthorization(op, r)
		}
		return authorizationAnswer(op, first)
	})
}

func decideAuthorization(op *db.PaymentsTestRail, r AuthorizeRequest) {
	switch r.PaymentMethod {
	case TestCardDeclined:
		op.Status, op.Detail = railDeclined, "generic_decline"
		return
	case TestCardInsufficientFunds:
		op.Status, op.Detail = railDeclined, "insufficient_funds"
		return
	case TestCardAuthenticationRequired:
		if !r.Authenticated {
			op.Status = railRequiresAction
			return
		}
	}
	switch r.Amount.Minor() % 100 {
	case magicRequestLost:
		op.Status = railLost
	case magicLateResponse:
		op.Status = railPending
	default:
		op.Status = railApproved
	}
}

func authorizationAnswer(op *db.PaymentsTestRail, first bool) Result {
	switch {
	case op.Status == railLost, op.Status == railPending:
		return Result{Outcome: Unknown}
	case first && op.Status == railApproved && op.Amount%100 == magicResponseLost:
		return Result{Outcome: Unknown}
	}
	return stored(op)
}

// Capture accepts one capture of an approved, unreversed authorization, for at most
// the authorized amount.
func (t *TestRail) Capture(ctx context.Context, r OperationRequest) Result {
	return t.do(ctx, "capture", r.Key, r.AuthorizationKey, r.Amount.Minor(), func(q *db.Queries, op *db.PaymentsTestRail, first bool) Result {
		if first {
			op.Status, op.Detail = t.checkCapture(ctx, q, r)
		}
		return stored(op)
	})
}

func (t *TestRail) checkCapture(ctx context.Context, q *db.Queries, r OperationRequest) (string, string) {
	auth, err := t.authorization(ctx, q, r.AuthorizationKey)
	if err != nil {
		return railDeclined, "authorization_not_found"
	}
	captured, err := q.RailApprovedTotal(ctx, db.RailApprovedTotalParams{AuthorizationKey: r.AuthorizationKey, Kind: "capture"})
	switch {
	case err != nil:
		return railDeclined, "processing_error"
	case auth.Status != railApproved:
		return railDeclined, "authorization_" + auth.Status
	case captured > 0:
		return railDeclined, "already_captured"
	case r.Amount.Minor() > auth.Amount:
		return railDeclined, "amount_exceeds_authorization"
	}
	return railApproved, ""
}

// Void reverses an authorization, even one the rail has not answered or not received
// yet: it is marked voided, so a late approval never happens.
func (t *TestRail) Void(ctx context.Context, r OperationRequest) Result {
	return t.do(ctx, "void", r.Key, r.AuthorizationKey, 0, func(q *db.Queries, op *db.PaymentsTestRail, first bool) Result {
		if first {
			op.Status, op.Detail = t.reverse(ctx, q, r)
		}
		return stored(op)
	})
}

func (t *TestRail) reverse(ctx context.Context, q *db.Queries, r OperationRequest) (string, string) {
	auth, err := t.authorization(ctx, q, r.AuthorizationKey)
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.InsertRailOperation(ctx, db.InsertRailOperationParams{
			Key: r.AuthorizationKey, Kind: "authorize", Status: railVoided, Reference: railReferencePrefix.New().String(),
			Detail: "reversed before it arrived", CreatedAt: pgtype.Timestamptz{Time: t.now().UTC(), Valid: true},
		})
		if err != nil {
			return railDeclined, "processing_error"
		}
		return railApproved, ""
	}
	if err != nil {
		return railDeclined, "processing_error"
	}
	captured, err := q.RailApprovedTotal(ctx, db.RailApprovedTotalParams{AuthorizationKey: r.AuthorizationKey, Kind: "capture"})
	if err != nil || captured > 0 {
		return railDeclined, "already_captured"
	}
	auth.Status = railVoided
	if err := q.SaveRailOperation(ctx, db.SaveRailOperationParams{
		Key: auth.Key, Status: auth.Status, Detail: auth.Detail, Calls: auth.Calls, Queries: auth.Queries,
	}); err != nil {
		return railDeclined, "processing_error"
	}
	return railApproved, ""
}

// Refund accepts refunds of a captured authorization up to what was captured.
func (t *TestRail) Refund(ctx context.Context, r OperationRequest) Result {
	return t.do(ctx, "refund", r.Key, r.AuthorizationKey, r.Amount.Minor(), func(q *db.Queries, op *db.PaymentsTestRail, first bool) Result {
		if !first {
			return stored(op)
		}
		// Refunds of one authorization are checked one at a time, so two cannot both fit.
		if err := q.LockRailKey(ctx, r.AuthorizationKey); err != nil {
			op.Status, op.Detail = railDeclined, "processing_error"
			return stored(op)
		}
		captured, err := q.RailApprovedTotal(ctx, db.RailApprovedTotalParams{AuthorizationKey: r.AuthorizationKey, Kind: "capture"})
		if err != nil {
			op.Status, op.Detail = railDeclined, "processing_error"
			return stored(op)
		}
		refunded, err := q.RailApprovedTotal(ctx, db.RailApprovedTotalParams{AuthorizationKey: r.AuthorizationKey, Kind: "refund"})
		switch {
		case err != nil:
			op.Status, op.Detail = railDeclined, "processing_error"
		case captured == 0:
			op.Status, op.Detail = railDeclined, "not_captured"
		case refunded+r.Amount.Minor() > captured:
			op.Status, op.Detail = railDeclined, "amount_exceeds_capture"
		default:
			op.Status = railApproved
		}
		return stored(op)
	})
}

func (t *TestRail) authorization(ctx context.Context, q *db.Queries, key string) (db.PaymentsTestRail, error) {
	if err := q.LockRailKey(ctx, key); err != nil {
		return db.PaymentsTestRail{}, err
	}
	return q.GetRailOperation(ctx, key)
}

func (t *TestRail) Query(ctx context.Context, key string) Result {
	var result Result
	err := postgres.InTx(ctx, t.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockRailKey(ctx, key); err != nil {
			return err
		}
		op, err := q.GetRailOperation(ctx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			result = Result{Outcome: NotFound}
			return nil
		}
		if err != nil {
			return err
		}
		op.Queries++
		switch op.Status {
		case railLost:
			result = Result{Outcome: NotFound}
		case railPending:
			// The late response lands between this query and the next.
			op.Status = railApproved
			result = Result{Outcome: Pending}
		default:
			result = stored(&op)
		}
		return q.SaveRailOperation(ctx, db.SaveRailOperationParams{
			Key: op.Key, Status: op.Status, Detail: op.Detail, Calls: op.Calls, Queries: op.Queries,
		})
	})
	if err != nil {
		t.logger.WarnContext(ctx, "test rail query failed", "key", key, "error", err)
		return Result{Outcome: Unknown}
	}
	return result
}

// do runs one request against the rail's memory, serialized per key: decide is told
// whether this is the first request with the key, may update the stored operation, and
// returns the answer.
func (t *TestRail) do(ctx context.Context, kind, key, authorizationKey string, amount int64, decide func(*db.Queries, *db.PaymentsTestRail, bool) Result) Result {
	var result Result
	err := postgres.InTx(ctx, t.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockRailKey(ctx, key); err != nil {
			return err
		}
		op, err := q.GetRailOperation(ctx, key)
		first := errors.Is(err, pgx.ErrNoRows)
		switch {
		case first:
			op = db.PaymentsTestRail{
				Key: key, Kind: kind, AuthorizationKey: authorizationKey, Amount: amount,
				Reference: railReferencePrefix.New().String(), Calls: 1,
				CreatedAt: pgtype.Timestamptz{Time: t.now().UTC(), Valid: true},
			}
		case err != nil:
			return err
		default:
			op.Calls++
		}
		result = decide(q, &op, first)
		if first {
			return q.InsertRailOperation(ctx, db.InsertRailOperationParams{
				Key: op.Key, Kind: op.Kind, AuthorizationKey: op.AuthorizationKey, Amount: op.Amount, Status: op.Status,
				Reference: op.Reference, Detail: op.Detail, CreatedAt: op.CreatedAt,
			})
		}
		return q.SaveRailOperation(ctx, db.SaveRailOperationParams{
			Key: op.Key, Status: op.Status, Detail: op.Detail, Calls: op.Calls, Queries: op.Queries,
		})
	})
	if err != nil {
		t.logger.WarnContext(ctx, "test rail request failed", "kind", kind, "key", key, "error", err)
		return Result{Outcome: Unknown}
	}
	return result
}

func stored(op *db.PaymentsTestRail) Result {
	switch op.Status {
	case railApproved:
		return Result{Outcome: Approved, Reference: op.Reference}
	case railDeclined:
		return Result{Outcome: Declined, Reference: op.Reference, DeclineCode: op.Detail}
	case railRequiresAction:
		return Result{Outcome: ActionRequired, Reference: op.Reference}
	case railVoided:
		return Result{Outcome: Declined, Reference: op.Reference, DeclineCode: "authorization_reversed"}
	default:
		return Result{Outcome: Unknown}
	}
}
