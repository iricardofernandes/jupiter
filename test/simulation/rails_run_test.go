//go:build simulation

package simulation_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/payments"
)

// crashes ends a background run part-way, as a process that dies would: a run marked
// with a crash point has its context cancelled when it reaches its k-th statement, and
// whatever it had not committed is lost.
type crashes struct{}

type crashKey struct{}

type crashPoint struct {
	left   atomic.Int64
	cancel context.CancelFunc
	hit    atomic.Bool
}

func (crashes) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if p, ok := ctx.Value(crashKey{}).(*crashPoint); ok && p.left.Add(-1) == 0 {
		p.hit.Store(true)
		p.cancel()
	}
	return ctx
}

func (crashes) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// railCards is the test card rail, losing requests and answers as the seed decides.
type railCards struct {
	rails *rails
	inner payments.Rail
}

func (c *railCards) fault(kind string, call func() payments.Result) payments.Result {
	r := c.rails
	if !r.faulty {
		return call()
	}
	switch roll := r.rng.IntN(100); {
	case roll < 3:
		r.record("card %s: request lost", kind)
		return payments.Result{Outcome: payments.Unknown}
	case roll < 6:
		call()
		r.record("card %s: answer lost", kind)
		return payments.Result{Outcome: payments.Unknown}
	}
	res := call()
	r.record("card %s: %s", kind, res.Outcome)
	return res
}

func (c *railCards) Authorize(ctx context.Context, req payments.AuthorizeRequest) payments.Result {
	return c.fault("authorize", func() payments.Result { return c.inner.Authorize(ctx, req) })
}

func (c *railCards) Capture(ctx context.Context, req payments.OperationRequest) payments.Result {
	return c.fault("capture", func() payments.Result { return c.inner.Capture(ctx, req) })
}

func (c *railCards) Void(ctx context.Context, req payments.OperationRequest) payments.Result {
	return c.fault("void", func() payments.Result { return c.inner.Void(ctx, req) })
}

func (c *railCards) Refund(ctx context.Context, req payments.OperationRequest) payments.Result {
	return c.fault("refund", func() payments.Result { return c.inner.Refund(ctx, req) })
}

func (c *railCards) Query(ctx context.Context, key string) payments.Result {
	if c.rails.faulty && c.rails.rng.IntN(100) < 3 {
		c.rails.record("card query: lost")
		return payments.Result{Outcome: payments.Unknown}
	}
	return c.inner.Query(ctx, key)
}

func (r *rails) afterPhase() {
	if r.crash {
		r.crash = false
		r.stats.crashes++
		runtime.Goexit()
	}
}

type railResponse struct {
	status  int
	body    map[string]any
	raw     []byte
	crashed bool
}

// send runs one request on its own goroutine, so one that crashes between phases dies
// alone, and waits for it and for whatever it set off at the Pix bank.
func (r *rails) send(method, path, idempotencyKey string, body any) railResponse {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequestWithContext(r.t.Context(), method, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+r.key)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.handler.ServeHTTP(rec, req)
	}()
	<-done
	r.pix.Close()
	r.stats.requests++
	if r.crash {
		r.crash = false
	}
	if rec.Body.Len() == 0 {
		return railResponse{crashed: true}
	}
	out := railResponse{status: rec.Code, raw: rec.Body.Bytes()}
	_ = json.Unmarshal(out.raw, &out.body)
	return out
}

// get reads an object, which must be there.
func (r *rails) get(path string) map[string]any {
	resp := r.send(http.MethodGet, path, "", nil)
	if resp.status != http.StatusOK {
		r.t.Fatalf("GET %s: %d %s", path, resp.status, resp.raw)
	}
	return resp.body
}

// run starts n scenarios, interleaves them with the worker's background work and the
// rails' days, drains with the faults off, and checks everything agrees.
func (r *rails) run(n int) [32]byte {
	started := 0
	for step := 0; ; step++ {
		active := r.active()
		if started == n && len(active) == 0 {
			break
		}
		// A scenario waiting on the background or a payer takes its next step after the
		// next background run, not before.
		ready := readyOf(active)
		if started == n && len(ready) == 0 {
			r.wake(10 * time.Minute)
			continue
		}
		switch roll := r.rng.IntN(100); {
		case started < n && (roll < 20 || len(active) == 0):
			r.scenarios = append(r.scenarios, r.newScenario(started))
			started++
		case roll < 85 && len(ready) > 0:
			r.step(ready[r.rng.IntN(len(ready))])
		default:
			r.wake(time.Duration(1+r.rng.IntN(30)) * time.Minute)
		}
		if step%400 == 399 {
			r.check(false)
		}
	}
	r.drain()
	r.check(true)
	for _, sc := range r.scenarios {
		r.stats.outcomes[sc.kind+" "+sc.final(r)]++
	}
	return sha256.Sum256(r.trace)
}

func readyOf(scenarios []*scenario) []*scenario {
	var out []*scenario
	for _, sc := range scenarios {
		if !sc.idle || sc.pending != nil {
			out = append(out, sc)
		}
	}
	return out
}

// wake runs the background, after which every waiting scenario looks again.
func (r *rails) wake(advance time.Duration) {
	r.background(advance)
	for _, sc := range r.scenarios {
		sc.idle = false
	}
}

func (r *rails) active() []*scenario {
	var out []*scenario
	for _, sc := range r.scenarios {
		if !sc.done {
			out = append(out, sc)
		}
	}
	return out
}

// medPollEvery is how often the worker polls the banks for MED claims.
const medPollEvery = 5 * time.Minute

// task is one of the worker's background loops.
type task struct {
	name string
	run  func(context.Context) error
}

func (r *rails) tasks() []task {
	pool := r.pool
	count := func(f func(context.Context) (int, error)) func(context.Context) error {
		return func(ctx context.Context) error { _, err := f(ctx); return err }
	}
	return []task{
		{"ledger.apply_queued", func(ctx context.Context) error { _, err := r.ledger.ApplyQueued(ctx, pool, 10_000); return err }},
		{"ledger.expire_due", func(ctx context.Context) error { _, err := r.ledger.ExpireDue(ctx, pool, 10_000); return err }},
		{"api.complete_abandoned", count(r.api.CompleteAbandoned)},
		{"payments.resolve", count(func(ctx context.Context) (int, error) { return r.payments.Resolve(ctx, pool) })},
		{"payments.expire_authorizations", count(func(ctx context.Context) (int, error) { return r.payments.ExpireAuthorizations(ctx, pool) })},
		{"pix.reconcile", count(func(ctx context.Context) (int, error) { return r.pixConn.Reconcile(ctx, r.payments) })},
		{"payments.expire_pix_charges", count(func(ctx context.Context) (int, error) { return r.payments.ExpirePixCharges(ctx, pool) })},
		{"payments.return_unmatched_pix", count(func(ctx context.Context) (int, error) { return r.payments.ReturnUnmatchedPix(ctx, pool) })},
		{"payments.resolve_payouts", count(func(ctx context.Context) (int, error) { return r.payments.ResolvePayouts(ctx, pool) })},
		{"bank.collection", func(ctx context.Context) error {
			_, err := r.bankConn.Remit(ctx, r.payments)
			_, importErr := r.bankConn.ImportReturns(ctx, pool, r.payments)
			return errors.Join(err, importErr)
		}},
		{"receivables.advance", func(ctx context.Context) error {
			err := r.receiv.Advance(ctx, pool)
			_, recoverErr := r.receiv.Recover(ctx, pool)
			return errors.Join(err, recoverErr)
		}},
		{"disputes.advance", func(ctx context.Context) error { _, err := r.disputes.Advance(ctx, pool); return err }},
		{"disputes.reconcile_med", count(func(ctx context.Context) (int, error) { return r.disputes.ReconcileMED(ctx, pool) })},
		{"disputes.reconcile_cases", count(func(ctx context.Context) (int, error) { return r.disputes.ReconcileCases(ctx, pool) })},
		{"reconciliation.run", func(ctx context.Context) error { _, err := r.recon.RunDue(ctx); return err }},
	}
}

// background moves the clock on and does what the worker and the rails would in that
// time: each of the worker's loops once, some crashing part-way, then the banks' and the
// SLC's own work.
func (r *rails) background(advance time.Duration) {
	before := r.clock.Now().In(brasilia).YearDay()
	// The worker polls the banks for MED claims every 5 minutes; a claim must be held
	// within 30 of the bank's notice, even when the notice is lost.
	for ; advance > medPollEvery; advance -= medPollEvery {
		r.clock.Advance(medPollEvery)
		r.runTask(task{"disputes.reconcile_med", func(ctx context.Context) error { _, err := r.disputes.ReconcileMED(ctx, r.pool); return err }})
	}
	r.clock.Advance(advance)
	if r.clock.Now().In(brasilia).YearDay() != before {
		r.stats.days++
	}
	for _, t := range r.tasks() {
		r.runTask(t)
	}
	r.pix.Tick(r.t.Context())
	r.bank.Tick()
	r.slc.Tick()
	r.pix.Close()
}

func (r *rails) runTask(t task) {
	ctx, cancel := context.WithCancel(r.t.Context())
	defer cancel()
	var point *crashPoint
	if r.faulty && r.rng.IntN(100) < 4 {
		point = &crashPoint{cancel: cancel}
		point.left.Store(int64(1 + r.rng.IntN(30)))
		ctx = context.WithValue(ctx, crashKey{}, point)
	}
	err := t.run(ctx)
	r.pix.Close()
	switch {
	case point != nil && point.hit.Load():
		r.stats.backgroundCrashes++
		r.record("background %s: crashed", t.name)
	case err != nil && r.faulty:
		r.stats.backgroundErrors++
		r.record("background %s: failed", t.name)
	case err != nil:
		r.t.Fatalf("background %s failed with the rails behaving: %v", t.name, err)
	}
}

var brasilia = time.FixedZone("BRT", -3*60*60)

// drain turns the faults off and runs the background day after day: what is late
// arrives, what is unknown is resolved, every receivable settles and every dispute
// closes.
func (r *rails) drain() {
	r.faulty = false
	r.record("draining")
	for range 6 * 24 {
		r.background(10 * time.Minute)
	}
	for range 400 {
		r.background(12 * time.Hour)
		if r.settled() {
			break
		}
	}
	for range 8 {
		r.background(24 * time.Hour)
	}
}

// settled is whether nothing is open: no outcome unknown, no request unfinished, no unit
// unsettled, no dispute undecided.
func (r *rails) settled() bool {
	var open int
	err := r.pool.QueryRow(r.t.Context(), `SELECT
		(SELECT count(*) FROM payments.attempts WHERE status IN ('authenticating', 'authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown')) +
		(SELECT count(*) FROM payments.refunds WHERE status IN ('pending', 'refund_unknown')) +
		(SELECT count(*) FROM payments.payouts WHERE status IN ('sending', 'unknown')) +
		(SELECT count(*) FROM payments.pix_received WHERE status IN ('unmatched', 'returning')) +
		(SELECT count(*) FROM api.idempotency_keys WHERE response_status IS NULL AND recovery_point <> 'started') +
		(SELECT count(*) FROM receivables.units WHERE (settled_on IS NULL AND value > 0) OR version > registered_version) +
		(SELECT count(*) FROM disputes.disputes WHERE status IN ('needs_response', 'under_review') OR pending_action <> '')`).Scan(&open)
	if err != nil {
		r.t.Fatal(err)
	}
	return open == 0
}

func (r *rails) fatalf(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("seed %d: "+format, append([]any{r.seed}, args...)...)
}
