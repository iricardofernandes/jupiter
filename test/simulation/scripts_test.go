//go:build simulation

package simulation_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
)

// script is one payment's life, chosen from the seed when it starts. It advances one
// request at a time; a request that dies, or answers 409 or 500, is retried later with
// the same Idempotency-Key, and a finished request is sometimes sent again to check it
// answers the same.
type script struct {
	id        int
	key       string
	intent    string
	amount    int64
	method    string
	card      string // a card number to save in the vault first, and then pay with
	manual    bool
	capture   int64 // 0 captures everything
	cancel    bool
	refunds   []int64
	retry     bool // after a decline, pay again with a good card
	authPass  bool
	pending   *pendingRequest
	done      bool
	waiting   bool // on the resolver or the completer, not on its own next request
	lastReply map[string][]byte
	sequence  int
}

type pendingRequest struct {
	method, path, key string
	body              any
}

type intentView struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	AmountReceived   int64  `json:"amount_received"`
	AmountRefunded   int64  `json:"amount_refunded"`
	AmountCapturable int64  `json:"amount_capturable"`
}

func (s *sim) newScript(n int) *script {
	r := s.rng
	merchant := r.IntN(len(s.keys))
	sc := &script{id: n, key: s.keys[merchant], lastReply: map[string][]byte{}}
	live := r.IntN(100) < 15
	sc.amount = 100 + r.Int64N(100_000)
	if r.IntN(10) == 0 {
		sc.amount = sc.amount/100*100 + int64(91+r.IntN(3)) // a magic amount
	}
	switch roll := r.IntN(100); {
	case roll < 70:
		sc.method = payments.TestCardVisa
	case roll < 80:
		sc.method = payments.TestCardDeclined
	case roll < 85:
		sc.method = payments.TestCardInsufficientFunds
	default:
		sc.method = payments.TestCardAuthenticationRequired
	}
	if r.IntN(10) < 3 {
		sc.card = savedCards[sc.method]
	}
	if live {
		// Live mode takes saved cards only, and has no authentication step yet (phase 6).
		sc.key, sc.card = s.liveKeys[merchant], liveCards[sc.method]
		s.stats.live++
	}
	sc.manual = r.IntN(2) == 0
	sc.authPass = r.IntN(5) != 0
	sc.retry = r.IntN(2) == 0 && !live
	switch roll := r.IntN(10); {
	case roll < 2:
		sc.cancel = true
	case roll < 5:
		sc.capture = 1 + r.Int64N(sc.amount)
	}
	if r.IntN(2) == 0 {
		for range 1 + r.IntN(3) {
			sc.refunds = append(sc.refunds, 1+r.Int64N(sc.amount/2+1))
		}
	}
	s.record("script %d: %d %s saved=%t manual=%t cancel=%t capture=%d refunds=%v", n, sc.amount, sc.method, sc.card != "", sc.manual, sc.cancel, sc.capture, sc.refunds)
	return sc
}

// step sends the script's next request, or resends its pending one.
func (s *sim) step(sc *script) {
	if sc.pending == nil {
		sc.pending = s.next(sc)
		if sc.pending == nil {
			return
		}
	}
	p := sc.pending
	s.crash = s.rng.IntN(100) < 5
	resp := s.send(sc.key, p.method, p.path, p.key, p.body)
	// Identifiers come from the wall clock, so the trace names the intent instead.
	path := strings.ReplaceAll(p.path, sc.intent, "{intent}")
	if resp.crashed || resp.status == http.StatusConflict || resp.status >= 500 {
		s.record("script %d: %s %s crashed=%t status=%d", sc.id, p.method, path, resp.crashed, resp.status)
		return
	}
	s.record("script %d: %s %s → %d", sc.id, p.method, path, resp.status)
	if previous, ok := sc.lastReply[p.key]; ok && !bytes.Equal(previous, resp.body) {
		s.t.Fatalf("script %d: %s answered differently to the same Idempotency-Key:\n%s\n%s", sc.id, p.key, previous, resp.body)
	}
	sc.lastReply[p.key] = resp.body
	if s.rng.IntN(100) < 5 {
		s.stats.duplicates++
		return // leave it pending: the next step sends it again and must get the same answer
	}
	sc.pending = nil
	if p.path == "/v1/payment_methods" {
		var pm struct{ ID string }
		if err := json.Unmarshal(resp.body, &pm); err != nil || resp.status != http.StatusOK {
			s.t.Fatalf("script %d: saving a card answered %d %s", sc.id, resp.status, resp.body)
		}
		sc.method, sc.card = pm.ID, ""
		return
	}
	if sc.intent == "" && p.method == http.MethodPost && p.path == "/v1/payment_intents" {
		var v struct {
			ID    string `json:"id"`
			Error struct {
				PaymentIntent struct{ ID string } `json:"payment_intent"`
			} `json:"error"`
		}
		_ = json.Unmarshal(resp.body, &v)
		sc.intent = v.ID
		if sc.intent == "" {
			sc.intent = v.Error.PaymentIntent.ID
		}
		if sc.intent == "" {
			s.t.Fatalf("script %d: creating the intent answered %d %s", sc.id, resp.status, resp.body)
		}
	}
}

// liveCards are the card numbers the network's issuer treats as each test payment method.
var liveCards = map[string]string{
	payments.TestCardVisa:                   "4242424242424242",
	payments.TestCardDeclined:               "4000000000000002",
	payments.TestCardInsufficientFunds:      "4000000000009995",
	payments.TestCardAuthenticationRequired: "5555555555554444",
}

// savedCards are the test card numbers that behave as each test payment method.
var savedCards = map[string]string{
	payments.TestCardVisa:                   "4242424242424242",
	payments.TestCardDeclined:               "4000000000000002",
	payments.TestCardInsufficientFunds:      "4000000000009995",
	payments.TestCardAuthenticationRequired: "4000002500003155",
}

func (s *sim) request(sc *script, path string, body any) *pendingRequest {
	sc.sequence++
	return &pendingRequest{method: http.MethodPost, path: path, body: body, key: fmt.Sprintf("script-%d-%d", sc.id, sc.sequence)}
}

// next decides the script's next request from the intent's current status.
func (s *sim) next(sc *script) *pendingRequest {
	sc.waiting = false
	if sc.card != "" {
		return s.request(sc, "/v1/payment_methods", map[string]any{"type": "card", "card": map[string]any{
			"number": sc.card, "exp_month": 12, "exp_year": 2030, "cvc": "123",
		}})
	}
	if sc.intent == "" {
		body := map[string]any{"amount": sc.amount, "currency": "brl", "payment_method": sc.method, "confirm": true}
		if sc.manual {
			body["capture_method"] = "manual"
		}
		return s.request(sc, "/v1/payment_intents", body)
	}
	it := s.view(sc)
	pi := "/v1/payment_intents/" + sc.intent
	switch it.Status {
	case "processing":
		sc.waiting = true
		return nil
	case "requires_action":
		outcome := "failed"
		if sc.authPass {
			outcome = "succeeded"
		}
		return s.request(sc, "/v1/test_helpers/payment_intents/"+sc.intent+"/complete_action", map[string]any{"outcome": outcome})
	case "requires_payment_method":
		if sc.retry {
			sc.retry = false
			return s.request(sc, pi+"/confirm", map[string]any{"payment_method": payments.TestCardVisa})
		}
		if !sc.cancel {
			sc.done = true
			return nil
		}
		sc.cancel = false
		return s.request(sc, pi+"/cancel", nil)
	case "requires_capture":
		if sc.cancel {
			sc.cancel = false
			return s.request(sc, pi+"/cancel", nil)
		}
		body := map[string]any{}
		if sc.capture > 0 && sc.capture <= it.AmountCapturable {
			body["amount_to_capture"] = sc.capture
		}
		return s.request(sc, pi+"/capture", body)
	case "succeeded":
		if len(sc.refunds) == 0 {
			sc.done = true
			return nil
		}
		amount := sc.refunds[0]
		sc.refunds = sc.refunds[1:]
		if left := it.AmountReceived - it.AmountRefunded; amount > left {
			amount = left
		}
		if amount <= 0 {
			return s.next(sc)
		}
		return s.request(sc, "/v1/refunds", map[string]any{"payment_intent": sc.intent, "amount": amount})
	default:
		sc.done = true
		return nil
	}
}

func (s *sim) view(sc *script) intentView {
	resp := s.send(sc.key, http.MethodGet, "/v1/payment_intents/"+sc.intent, "", nil)
	var it intentView
	if err := json.Unmarshal(resp.body, &it); err != nil || resp.status != http.StatusOK {
		s.t.Fatalf("script %d: reading %s: %d %s", sc.id, sc.intent, resp.status, resp.body)
	}
	return it
}

// run starts n scripts, interleaves them with the background work of a worker, drains,
// checks every invariant, and returns a digest of the trace.
func (s *sim) run(n int) [32]byte {
	started := 0
	for step := 0; ; step++ {
		active := s.active()
		if started == n {
			if len(active) == 0 {
				break
			}
			// Everything left waits on background work: run it until nothing is open,
			// then let the scripts take their next steps.
			if allWaiting(active) {
				s.drain()
				for _, sc := range active {
					sc.waiting = false
				}
				continue
			}
		}
		switch roll := s.rng.IntN(100); {
		case started < n && (roll < 25 || len(active) == 0):
			s.scripts = append(s.scripts, s.newScript(started))
			started++
		case roll < 85 && len(active) > 0:
			s.step(active[s.rng.IntN(len(active))])
		case roll < 92:
			advance := 2 * time.Minute
			if s.rng.IntN(100) == 0 {
				// Now and then, long enough for every uncaptured authorization to expire, but
				// short of the ledger's backstop a day later: an outage that long is an alert,
				// covered by its own test.
				advance = 10*24*time.Hour + time.Hour
				s.stats.expiryJumps++
			}
			s.background(advance)
		default:
			s.clock.Advance(time.Duration(1+s.rng.IntN(30)) * time.Second)
		}
		if step%500 == 499 {
			s.check(false)
		}
	}
	s.drain()
	s.check(true)
	for _, sc := range s.scripts {
		s.stats.outcomes[s.view(sc).Status]++
	}
	return sha256.Sum256(s.trace)
}

func allWaiting(scripts []*script) bool {
	for _, sc := range scripts {
		if !sc.waiting || sc.pending != nil {
			return false
		}
	}
	return true
}

// active lists scripts that have work left, including those waiting for the resolver.
func (s *sim) active() []*script {
	var out []*script
	for _, sc := range s.scripts {
		if !sc.done {
			out = append(out, sc)
		}
	}
	return out
}

// background does what the worker would: expire authorizations, resolve unknown
// outcomes, finish abandoned requests, apply batched balances.
func (s *sim) background(advance time.Duration) {
	ctx := s.t.Context()
	s.clock.Advance(advance)
	s.stats.resolverRuns++
	expired, err := s.payments.ExpireAuthorizations(ctx, s.pool)
	if err != nil {
		s.t.Fatalf("ExpireAuthorizations: %v", err)
	}
	if _, err := s.ledger.ExpireDue(ctx, s.pool, 10_000); err != nil {
		s.t.Fatalf("ledger ExpireDue: %v", err)
	}
	resolved, err := s.payments.Resolve(ctx, s.pool)
	if err != nil {
		s.t.Fatalf("Resolve: %v", err)
	}
	completed, err := s.api.CompleteAbandoned(ctx)
	if err != nil {
		s.t.Fatalf("CompleteAbandoned: %v", err)
	}
	if _, err := s.ledger.ApplyQueued(ctx, s.pool, 10_000); err != nil {
		s.t.Fatalf("ApplyQueued: %v", err)
	}
	forwarded, err := s.acquirer.RetryForwards(ctx, 1000)
	if err != nil {
		s.t.Fatalf("RetryForwards: %v", err)
	}
	s.record("background: expired %d, resolved %d, completed %d, forwarded %d", expired, resolved, completed, forwarded)
}

func (s *sim) drain() {
	for range 20 {
		s.background(10 * time.Minute)
		var open int
		err := s.pool.QueryRow(s.t.Context(), `SELECT
			(SELECT count(*) FROM payments.intents WHERE status = 'processing') +
			(SELECT count(*) FROM payments.refunds WHERE status IN ('pending', 'refund_unknown')) +
			(SELECT count(*) FROM api.idempotency_keys WHERE response_status IS NULL AND recovery_point <> 'started') +
			(SELECT count(*) FROM acquirer.exchanges WHERE next_forward_at IS NOT NULL)`).Scan(&open)
		if err != nil {
			s.t.Fatal(err)
		}
		if open == 0 {
			return
		}
	}
	s.t.Fatal("work still open after three hours of background work: something never reaches a final state")
}

// check verifies the ledger's invariants, that payments agree with the ledger and,
// once drained, that the rail and Jupiter agree about every authorization.
func (s *sim) check(final bool) {
	ctx := s.t.Context()
	s.stats.checks++
	if _, err := s.ledger.ApplyQueued(ctx, s.pool, 100_000); err != nil {
		s.t.Fatal(err)
	}
	report, err := s.ledger.Check(ctx, s.pool, ledger.CheckOptions{ClearingGrace: 1000 * time.Hour, ExpiryGrace: 1000 * time.Hour})
	if err != nil {
		s.t.Fatal(err)
	}
	violations, err := s.payments.Check(ctx, s.pool)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, v := range report.Violations {
		s.t.Errorf("ledger: %+v", v)
	}
	for _, v := range violations {
		s.t.Errorf("payments: %+v", v)
	}
	if final {
		s.checkRail()
		s.checkNetwork()
	}
	if s.t.Failed() {
		s.t.FailNow()
	}
}

// checkRail compares the rail's memory with Jupiter's, in both directions: what the rail
// approved Jupiter knows about, and what Jupiter recorded the rail approved.
func (s *sim) checkRail() {
	checks := []struct{ what, query string }{
		{"authorizations approved on the rail that Jupiter neither captured nor holds", `
			SELECT count(*) FROM payments.test_rail r JOIN payments.attempts a ON a.id = r.key
			WHERE r.kind = 'authorize' AND r.status = 'approved' AND a.status NOT IN ('captured', 'authorized')`},
		{"attempts Jupiter voided whose authorization the rail did not reverse", `
			SELECT count(*) FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id AND NOT i.livemode
			LEFT JOIN payments.test_rail r ON r.key = a.id
			WHERE a.status = 'voided' AND coalesce(r.status, '') <> 'voided'`},
		{"captures that differ between the rail and Jupiter", `
			SELECT count(*) FROM (SELECT a.* FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id AND NOT i.livemode) a
			FULL JOIN (SELECT authorization_key, sum(amount) AS amount FROM payments.test_rail
			           WHERE kind = 'capture' AND status = 'approved' GROUP BY authorization_key) c ON c.authorization_key = a.id
			WHERE coalesce(c.amount, 0) <> CASE WHEN a.status = 'captured' THEN a.amount_captured ELSE 0 END`},
		{"refunds that differ between the rail and Jupiter", `
			SELECT count(*) FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id AND NOT i.livemode
			LEFT JOIN (SELECT authorization_key, sum(amount) AS amount FROM payments.test_rail
			           WHERE kind = 'refund' AND status = 'approved' GROUP BY authorization_key) r ON r.authorization_key = a.id
			LEFT JOIN (SELECT attempt_id, sum(amount) AS amount FROM payments.refunds
			           WHERE status = 'succeeded' GROUP BY attempt_id) j ON j.attempt_id = a.id
			WHERE coalesce(r.amount, 0) <> coalesce(j.amount, 0)`},
		{"captures, voids or refunds the rail refused as breaking its rules", `
			SELECT count(*) FROM payments.test_rail WHERE kind <> 'authorize' AND status = 'declined'`},
		{"attempts still in flight or unknown after draining", `
			SELECT count(*) FROM payments.attempts WHERE status LIKE '%unknown' OR status IN ('authorizing', 'capturing', 'voiding')`},
	}
	for _, c := range checks {
		var n int
		if err := s.pool.QueryRow(s.t.Context(), c.query).Scan(&n); err != nil {
			s.t.Fatalf("%s: %v", c.what, err)
		}
		if n > 0 {
			s.t.Errorf("%d %s", n, c.what)
		}
	}
}

// checkNetwork compares the issuer's books with Jupiter's for live payments: every hold
// the issuer keeps is an authorization Jupiter still holds, every capture Jupiter
// recorded the network completed for the same amount and nothing else, refunds agree,
// and no reversal or advice is still waiting to be acknowledged.
func (s *sim) checkNetwork() {
	ctx := s.t.Context()
	jupiter := map[string]struct {
		status           string
		captured, refund int64
	}{}
	rows, err := s.pool.Query(ctx, `SELECT a.network_transaction_id, a.status, a.amount_captured,
			coalesce((SELECT sum(amount) FROM payments.refunds r WHERE r.attempt_id = a.id AND r.status = 'succeeded'), 0)::bigint
		FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
		WHERE i.livemode AND a.network_transaction_id <> ''`)
	if err != nil {
		s.t.Fatal(err)
	}
	for rows.Next() {
		var nti, status string
		var captured, refunded int64
		if err := rows.Scan(&nti, &status, &captured, &refunded); err != nil {
			s.t.Fatal(err)
		}
		jupiter[nti] = struct {
			status           string
			captured, refund int64
		}{status, captured, refunded}
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	for _, h := range s.network.Holds() {
		if j, ok := jupiter[h.NetworkTransactionID]; !ok || j.status != "authorized" {
			s.t.Errorf("the issuer holds %d under %s, which Jupiter has as %q: a reversal left a hold behind", h.Amount, h.NetworkTransactionID, j.status)
		}
	}
	completed := map[string]cardnetwork.Completion{}
	for _, c := range s.network.Completions() {
		completed[c.NetworkTransactionID] = c
		j, ok := jupiter[c.NetworkTransactionID]
		if !ok || j.status != "captured" || j.captured != c.Completed || j.refund != c.Refunded {
			s.t.Errorf("the network completed %d and refunded %d under %s; Jupiter has it %q, captured %d, refunded %d",
				c.Completed, c.Refunded, c.NetworkTransactionID, j.status, j.captured, j.refund)
		}
	}
	for nti, j := range jupiter {
		if j.status == "captured" && completed[nti].Completed != j.captured {
			s.t.Errorf("Jupiter captured %d under %s without the network completing it", j.captured, nti)
		}
	}
	var captured int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
		WHERE i.livemode AND a.status = 'captured' AND a.network_transaction_id = ''`).Scan(&captured); err != nil || captured > 0 {
		s.t.Errorf("%d live payments captured without an approved authorization (%v)", captured, err)
	}
	var waiting int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM acquirer.exchanges WHERE next_forward_at IS NOT NULL").Scan(&waiting); err != nil || waiting > 0 {
		s.t.Errorf("%d reversals or advices never acknowledged (%v)", waiting, err)
	}
}
