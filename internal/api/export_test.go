package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/merchant"
)

type TestPhase = phase

// NewTestPhase builds a phase whose atomic step either names the next recovery point or,
// when next is empty, answers 200 with body.
func NewTestPhase(point string, foreign func(context.Context) error, atomic func(context.Context, pgx.Tx) (next string, body any, err error)) TestPhase {
	p := phase{point: point}
	if foreign != nil {
		p.foreign = func(ctx context.Context, _ *request) error { return foreign(ctx) }
	}
	p.atomic = func(ctx context.Context, tx pgx.Tx, _ *request) (outcome, error) {
		next, body, err := atomic(ctx, tx)
		if err != nil {
			return outcome{}, err
		}
		if next != "" {
			return proceed(next)
		}
		return respond(body)
	}
	return p
}

// RegisterTestOperation adds an operation and returns a handler that serves it behind the
// same authentication and versioning as the real API.
func (a *API) RegisterTestOperation(name string, phases ...TestPhase) http.Handler {
	a.operations[name] = operation{name: name, scope: merchant.ScopeEventsRead, phases: phases}
	return a.withRequestID(a.withRecovery(a.withAuthentication(a.withVersion(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.serve(w, r, name, "")
	})))))
}

var Fingerprint = fingerprint

func (a *API) SetAfterPhase(fn func(recoveryPoint string)) { a.afterPhase = fn }

func (a *API) SetIdempotencyTiming(lockTimeout, wait time.Duration) {
	a.idem.lockTimeout = lockTimeout
	a.idem.wait = wait
}

func NewClientError(code string) error {
	return invalidRequest(code, "", "test failure")
}
