package acquirer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/acquirer/db"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

const (
	firstRetry = 5 * time.Second
	lastRetry  = 5 * time.Minute
)

// forwardLater hands an exchange to store-and-forward: its reversal or advice is
// repeated by RetryForwards until the network acknowledges it.
func (c *Connector) forwardLater(ctx context.Context, ex db.AcquirerExchange, state string) payments.Result {
	err := c.update(ctx, ex.Key, func(e *db.AcquirerExchange) bool {
		if e.State != stateSending {
			return false
		}
		e.State = state
		e.NextForwardAt = timestamptz(c.cfg.Now().Add(firstRetry))
		return true
	})
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "handing an exchange to store-and-forward", "key", ex.Key, "error", err)
	}
	return payments.Result{Outcome: payments.Unknown}
}

// forwardNow sends a reversal at once, after a timeout, instead of waiting for the loop.
func (c *Connector) forwardNow(ctx context.Context, key string) {
	var claimed []db.AcquirerExchange
	err := postgres.InTx(ctx, c.cfg.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		ex, err := q.LockExchange(ctx, key)
		if err != nil || !ex.NextForwardAt.Valid || ex.NextForwardAt.Time.After(c.cfg.Now()) {
			return err
		}
		ex, err = c.claim(ctx, q, ex)
		claimed = []db.AcquirerExchange{ex}
		return err
	})
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "claiming a reversal", "key", key, "error", err)
		return
	}
	for _, ex := range claimed {
		c.forward(ctx, ex)
	}
}

// RetryForwards sends every reversal and advice that is due, and returns how many it
// sent. Exchanges are claimed with SKIP LOCKED, so several workers share the work, and
// for long enough that one taken by a worker that dies is taken up again.
func (c *Connector) RetryForwards(ctx context.Context, batch int32) (int, error) {
	var claimed []db.AcquirerExchange
	err := postgres.InTx(ctx, c.cfg.Pool, func(tx pgx.Tx) error {
		claimed = nil
		q := db.New(tx)
		due, err := q.ExchangesToForward(ctx, db.ExchangesToForwardParams{Now: timestamptz(c.cfg.Now()), MaxCount: batch})
		if err != nil {
			return err
		}
		for _, ex := range due {
			if ex, err = c.claim(ctx, q, ex); err != nil {
				return err
			}
			claimed = append(claimed, ex)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("claiming reversals and advices: %w", err)
	}
	for _, ex := range claimed {
		c.forward(ctx, ex)
	}
	return len(claimed), nil
}

func (c *Connector) claim(ctx context.Context, q *db.Queries, ex db.AcquirerExchange) (db.AcquirerExchange, error) {
	stan, err := q.NextSTAN(ctx)
	if err != nil {
		return ex, err
	}
	ex.ForwardStan, ex.ForwardAttempts = text(stan), ex.ForwardAttempts+1
	ex.NextForwardAt = timestamptz(c.cfg.Now().Add(2 * c.cfg.Timeout))
	return ex, save(ctx, q, ex, c.cfg.Now())
}

// forward sends one repeat of an exchange's reversal or advice and applies the answer.
func (c *Connector) forward(ctx context.Context, ex db.AcquirerExchange) {
	m, err := c.forwarded(ctx, ex)
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "building a reversal or advice", "key", ex.Key, "error", err)
		return
	}
	resp, sendErr := c.send(m)
	err = c.update(ctx, ex.Key, func(e *db.AcquirerExchange) bool {
		if e.State != ex.State || e.ForwardStan != ex.ForwardStan {
			return false
		}
		if sendErr != nil {
			e.NextForwardAt = timestamptz(c.cfg.Now().Add(backoff(int(e.ForwardAttempts))))
			return true
		}
		switch e.State {
		case stateReversing:
			e.State = stateReversed
			if resp.ResponseCode != cardnet.Approved {
				// The network refused the reversal, such as of a refund it already cleared: the
				// original stands, and reconciliation must account for it.
				e.State, e.ResponseCode, e.DeclineCode = stateDeclined, resp.ResponseCode, "reversal_refused"
				c.cfg.Logger.ErrorContext(ctx, "the card network refused a reversal", "key", e.Key, "response_code", resp.ResponseCode)
			}
		case stateAdvising:
			e.State, e.ResponseCode = stateAcknowledged, resp.ResponseCode
			if resp.ResponseCode != cardnet.Approved {
				e.State, e.DeclineCode = stateDeclined, declineCode(resp.ResponseCode)
			}
		}
		stopForwarding(e)
		return true
	})
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "recording a reversal or advice", "key", ex.Key, "error", err)
	}
	if sendErr != nil {
		c.cfg.Logger.WarnContext(ctx, "the card network did not acknowledge; will repeat", "message", m, "attempt", ex.ForwardAttempts, "error", sendErr)
	}
}

// forwarded builds the message store-and-forward repeats: a reversal advice (0420, then
// 0421) naming the message it reverses, or a completion advice repeat (0221).
func (c *Connector) forwarded(ctx context.Context, ex db.AcquirerExchange) (cardnet.Message, error) {
	target := ex
	if ex.Kind == kindVoid || ex.Kind == kindCapture {
		auth, err := db.New(c.cfg.Pool).GetExchange(ctx, ex.AuthorizationKey.String)
		if err != nil {
			return cardnet.Message{}, fmt.Errorf("reading the authorization of %s: %w", ex.Key, err)
		}
		target = auth
	}
	m := c.base(ex)
	m.STAN, m.TransmissionDateTime = ex.ForwardStan.String, cardnet.TransmissionTime(c.cfg.Now())
	m.OriginalData = c.original(target)
	if target.NetworkTransactionID != "" {
		m.Private = &cardnet.PrivateData{NetworkTransactionID: target.NetworkTransactionID}
	}
	switch {
	case ex.State == stateAdvising:
		m.MTI = cardnet.CompletionAdviceRepeat
		if target.Installments.Valid {
			m.Installments = fmt.Sprintf("%02d", target.Installments.Int32)
		}
	case ex.ForwardAttempts <= 1:
		m.MTI = cardnet.ReversalAdvice
		m.Amount = target.Amount
	default:
		m.MTI = cardnet.ReversalAdviceRepeat
		m.Amount = target.Amount
	}
	if ex.State != stateAdvising && ex.State != stateReversing {
		return cardnet.Message{}, errors.New("acquirer: nothing to forward in state " + ex.State)
	}
	return m, nil
}

func stopForwarding(e *db.AcquirerExchange) {
	e.NextForwardAt.Valid = false
}

func backoff(attempts int) time.Duration {
	d := firstRetry
	for range attempts - 1 {
		d *= 2
		if d >= lastRetry {
			return lastRetry
		}
	}
	return d
}
