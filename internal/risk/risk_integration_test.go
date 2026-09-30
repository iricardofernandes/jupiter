//go:build integration

package risk_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/risk"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Main(m, &server, risk.Migrate))
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	clock *clock
	svc   *risk.Service
	owner risk.Owner
	n     int
}

func newEnv(t *testing.T) *env {
	c := &clock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	return &env{
		t: t, pool: server.Pool(t), clock: c, svc: risk.New(risk.Config{Now: c.Now}),
		owner: risk.Owner{Merchant: id.MustPrefix("mch").New()},
	}
}

func (e *env) input(card, ip string, amount int64) risk.Input {
	e.n++
	return risk.Input{
		Owner: e.owner, Attempt: fmt.Sprintf("pa_%d", e.n), Intent: fmt.Sprintf("pi_%d", e.n), Amount: amount, Currency: "BRL",
		Brand: "visa", BIN: "42424242", IP: ip, CardFingerprint: card, MerchantFingerprint: "m" + card,
	}
}

// decide decides and, when the attempt goes on, records how its authorization ended.
func (e *env) decide(in risk.Input, approved bool) risk.Decision {
	e.t.Helper()
	var d risk.Decision
	err := postgres.InTx(e.t.Context(), e.pool, func(tx pgx.Tx) error {
		var err error
		if d, err = e.svc.Decide(e.t.Context(), tx, in); err != nil || d.Action == risk.Block {
			return err
		}
		return e.svc.RecordOutcome(e.t.Context(), tx, in.Attempt, approved)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func fired(d risk.Decision, ruleID string) bool {
	for _, r := range d.Rules {
		if r.ID == ruleID {
			return true
		}
	}
	return false
}

func TestAnOrdinaryPaymentIsAllowed(t *testing.T) {
	e := newEnv(t)
	d := e.decide(e.input("card-a", "10.0.0.1", 5000), true)
	if d.Action != risk.Allow || len(d.Rules) != 0 || d.Features.Amount != 5000 || d.Features.CardAttempts1h != 0 {
		t.Fatalf("decision = %+v", d)
	}
}

func TestVelocityRules(t *testing.T) {
	e := newEnv(t)
	for range 10 {
		e.decide(e.input("card-a", "", 1000), true)
	}
	d := e.decide(e.input("card-a", "", 1000), true)
	if d.Action != risk.Block || !fired(d, "card_velocity") || d.Features.CardAttempts1h != 10 {
		t.Fatalf("the eleventh attempt in an hour: %+v", d)
	}
	e.clock.Advance(2 * time.Hour)
	if d := e.decide(e.input("card-a", "", 1000), true); fired(d, "card_velocity") {
		t.Fatalf("an hour later the window still counts them: %+v", d.Features)
	}

	for i := range 5 {
		e.decide(e.input(fmt.Sprintf("card-%d", i), "203.0.113.9", 1000), true)
	}
	if d := e.decide(e.input("card-new", "203.0.113.9", 1000), true); d.Action != risk.Block || !fired(d, "ip_many_cards") {
		t.Fatalf("a sixth card from one address: %+v", d)
	}

	for range 3 {
		e.decide(e.input("card-declined", "", 1000), false)
	}
	if d := e.decide(e.input("card-declined", "", 1000), false); d.Action != risk.Request3DS || !fired(d, "card_declines") {
		t.Fatalf("a card declined three times: %+v", d)
	}
}

func TestListsAndMerchantRules(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	err := postgres.InTx(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := e.svc.AddListItem(ctx, tx, e.owner, "block", "bin", "42424242"); err != nil {
			return err
		}
		if _, err := e.svc.AddListItem(ctx, tx, e.owner, "allow", "card_fingerprint", "mcard-vip"); err != nil {
			return err
		}
		_, err := e.svc.CreateRule(ctx, tx, e.owner, risk.Review, `amount > 50000 && brand == "visa"`, "Large Visa payments")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := e.decide(e.input("card-a", "", 1000), true); d.Action != risk.Block || !strings.Contains(d.Rules[0].Description, "bin 42424242 is on the block list") {
		t.Fatalf("a blocked BIN: %+v", d)
	}
	vip := e.input("card-vip", "", 100000)
	if d := e.decide(vip, true); d.Action != risk.Allow || len(d.Rules) != 1 {
		t.Fatalf("an allowed card on a blocked BIN: %+v", d)
	}
	other := e.input("card-b", "", 100000)
	other.BIN = "55555555"
	if d := e.decide(other, true); d.Action != risk.Review || !strings.HasPrefix(d.Rules[0].ID, "rr_") {
		t.Fatalf("the merchant's own rule: %+v", d)
	}

	err = postgres.InTx(ctx, e.pool, func(tx pgx.Tx) error {
		for expression, want := range map[string]string{
			"amount +":          "does not compile",
			"amount":            "does not compile",
			"unknown_field > 1": "does not compile",
			// Nothing whose cost does not follow from its length.
			`len(brand) > 1`:           "functions are not allowed",
			`all(1..100000000, # > 0)`: "not allowed",
			`amount in 1..100000000`:   "ranges are not allowed",
		} {
			if _, err := e.svc.CreateRule(ctx, tx, e.owner, risk.Block, expression, ""); !errors.Is(err, risk.ErrInvalid) || !strings.Contains(err.Error(), want) {
				return fmt.Errorf("%q: %w", expression, err)
			}
		}
		if _, err := e.svc.CreateRule(ctx, tx, e.owner, "maybe", "amount > 1", ""); !errors.Is(err, risk.ErrInvalid) {
			return fmt.Errorf("a bad action: %w", err)
		}
		if _, err := e.svc.AddListItem(ctx, tx, e.owner, "grey", "ip", "1.2.3.4"); !errors.Is(err, risk.ErrInvalid) {
			return fmt.Errorf("a bad list: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A rule that fails when it runs is skipped, and the log says so; the other rules
// still decide.
func TestARuleThatFailsIsSkipped(t *testing.T) {
	e := newEnv(t)
	err := postgres.InTx(t.Context(), e.pool, func(tx pgx.Tx) error {
		if _, err := e.svc.CreateRule(t.Context(), tx, e.owner, risk.Block, `amount % (installments - installments) == 0`, "Broken"); err != nil {
			return err
		}
		_, err := e.svc.CreateRule(t.Context(), tx, e.owner, risk.Review, `amount > 50000`, "Large")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d := e.decide(e.input("card-a", "", 100000), true)
	if d.Action != risk.Review || len(d.Rules) != 2 {
		t.Fatalf("with a failing rule: %+v", d)
	}
	skipped := false
	for _, r := range d.Rules {
		skipped = skipped || strings.Contains(r.Description, "could not be evaluated")
	}
	if !skipped {
		t.Fatalf("the log does not show the failing rule: %+v", d.Rules)
	}
}

// Card testing: a burst of attempts, mostly declined, throttles the merchant, and the
// block explains itself with the numbers behind it.
func TestCardTestingThrottlesTheMerchant(t *testing.T) {
	e := newEnv(t)
	for i := range 25 {
		e.decide(e.input(fmt.Sprintf("probe-%d", i), fmt.Sprintf("198.51.100.%d", i), 100), i%5 == 0)
		e.clock.Advance(10 * time.Second)
	}
	blocked := 0
	var block risk.Decision
	for i := range 10 {
		d := e.decide(e.input(fmt.Sprintf("probe-x%d", i), fmt.Sprintf("192.0.2.%d", i), 100), false)
		if d.Action == risk.Block && fired(d, "card_testing_throttle") {
			blocked++
			block = d
		}
	}
	if blocked < 7 {
		t.Fatalf("%d of 10 attempts were blocked under card testing", blocked)
	}
	if !block.Features.Throttled || !strings.Contains(block.Rules[0].Description, "throttled for card testing") ||
		!strings.Contains(block.Rules[0].Description, "were declined") {
		t.Fatalf("the block does not explain itself: %+v", block)
	}
	// The throttle ends, and a merchant with ordinary traffic is never throttled.
	e.clock.Advance(40 * time.Minute)
	if d := e.decide(e.input("later", "", 100), true); d.Features.Throttled {
		t.Fatal("still throttled after the throttle ended")
	}
	calm := newEnv(t)
	for i := range 30 {
		if d := calm.decide(calm.input(fmt.Sprintf("c%d", i), "", 1000), true); d.Action != risk.Allow {
			t.Fatalf("an ordinary merchant: %+v", d)
		}
	}
}

func TestTheDecisionLog(t *testing.T) {
	e := newEnv(t)
	d := e.decide(e.input("card-a", "10.0.0.1", 2_000_000), true)
	got, err := e.svc.Decision(t.Context(), e.pool, e.owner, d.ID)
	if err != nil || got.Action != risk.Review || got.Features.Amount != 2_000_000 || !fired(got, "large_amount") {
		t.Fatalf("Decision = %+v, %v", got, err)
	}
	if _, err := e.svc.Decision(t.Context(), e.pool, risk.Owner{Merchant: id.MustPrefix("mch").New()}, d.ID); !errors.Is(err, risk.ErrNotFound) {
		t.Fatalf("another merchant read the decision: %v", err)
	}
}
