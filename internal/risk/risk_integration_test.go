//go:build integration

package risk_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/risk"
	"github.com/iricardofernandes/jupiter/internal/risk/migrations"
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
		Brand: "visa", BIN: "42424242", IP: ip, CardFingerprint: card, MerchantFingerprint: merchantFingerprint(card),
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

// merchantFingerprint stands for the fingerprint a merchant sees of a card.
func merchantFingerprint(card string) string {
	sum := sha256.Sum256([]byte(card))
	return hex.EncodeToString(sum[:8])
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
		if _, err := e.svc.AddListItem(ctx, tx, e.owner, "allow", "card_fingerprint", merchantFingerprint("card-vip")); err != nil {
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
			`amount in 1..100000000`:   "the operator .. is not allowed",
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

// An allow entry sets aside the merchant's own rules and the platform's milder actions,
// but neither the platform's blocks nor the card-testing throttle: an entry for a BIN, or
// for an address the merchant itself reports, must not switch off what protects the
// networks and the other merchants.
func TestAnAllowEntryDoesNotSwitchOffThePlatform(t *testing.T) {
	e := newEnv(t)
	err := postgres.InTx(t.Context(), e.pool, func(tx pgx.Tx) error {
		if _, err := e.svc.AddListItem(t.Context(), tx, e.owner, "allow", "bin", "42424242"); err != nil {
			return err
		}
		_, err := e.svc.CreateRule(t.Context(), tx, e.owner, risk.Block, `amount > 1`, "Everything")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := e.decide(e.input("card-a", "", 2_000_000), true); d.Action != risk.Allow || fired(d, "large_amount") {
		t.Fatalf("an allowed BIN skips the merchant's rules and a review: %+v", d)
	}
	for range 10 {
		e.decide(e.input("card-b", "", 1000), true)
	}
	if d := e.decide(e.input("card-b", "", 1000), true); d.Action != risk.Block || !fired(d, "card_velocity") {
		t.Fatalf("an allowed BIN still meets card velocity: %+v", d)
	}
	for i := range 25 {
		e.decide(e.input(fmt.Sprintf("probe-%d", i), fmt.Sprintf("198.51.100.%d", i), 100), i%5 == 0)
		e.clock.Advance(10 * time.Second)
	}
	blocked := 0
	for i := range 10 {
		if d := e.decide(e.input(fmt.Sprintf("probe-x%d", i), fmt.Sprintf("192.0.2.%d", i), 100), false); fired(d, "card_testing_throttle") {
			blocked++
		}
	}
	if blocked < 7 {
		t.Fatalf("%d of 10 card-testing attempts on an allowed BIN were blocked", blocked)
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

// A rule created before the language refused what it uses is skipped, and the log says
// so, rather than stop applying unseen.
func TestARuleNoLongerAllowedIsSkipped(t *testing.T) {
	e := newEnv(t)
	_, err := e.pool.Exec(t.Context(), `INSERT INTO risk.rules (id, merchant_id, livemode, action, expression, description, created_at)
		VALUES ('rr_old', $1, false, 'block', 'let x = bin + bin; x == ""', 'Old', now())`, e.owner.Merchant.String())
	if err != nil {
		t.Fatal(err)
	}
	d := e.decide(e.input("card-a", "", 1000), true)
	if d.Action != risk.Allow || len(d.Rules) != 1 || d.Rules[0].ID != "rr_old" || !strings.Contains(d.Rules[0].Description, "no longer allowed") {
		t.Fatalf("decision = %+v", d)
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

// An address counts one way however it is written, an IPv6 one by its /64; the address
// the vault saw counts when the merchant reports none, or another; and list entries
// match either.
func TestAddressesCountByTheCustomer(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		in := e.input(fmt.Sprintf("v6-%d", i), fmt.Sprintf("2001:db8:1:2::%x", i+1), 1000)
		e.decide(in, true)
	}
	if d := e.decide(e.input("v6-new", "2001:db8:1:2:ffff::1", 1000), true); !fired(d, "ip_many_cards") || d.Features.IPCards24h != 5 {
		t.Fatalf("a sixth card from the same /64: %+v", d)
	}
	for i := range 5 {
		in := e.input(fmt.Sprintf("seen-%d", i), "", 1000)
		in.CardIP = "198.51.100.20"
		e.decide(in, true)
	}
	hidden := e.input("seen-new", "203.0.113.200", 1000)
	hidden.CardIP = "::ffff:198.51.100.20"
	if d := e.decide(hidden, true); !fired(d, "ip_many_cards") || d.Features.CardIP != hidden.CardIP {
		t.Fatalf("the address the vault saw, the merchant reporting another: %+v", d)
	}

	err := postgres.InTx(t.Context(), e.pool, func(tx pgx.Tx) error {
		if _, err := e.svc.AddListItem(t.Context(), tx, e.owner, "block", "ip", "::ffff:192.0.2.77"); err != nil {
			return err
		}
		for kind, value := range map[string]string{"ip": "192.0.2.300", "bin": "4242", "card_fingerprint": "not-hex"} {
			if _, err := e.svc.AddListItem(t.Context(), tx, e.owner, "block", kind, value); !errors.Is(err, risk.ErrInvalid) {
				return fmt.Errorf("%s %q: %w", kind, value, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := e.input("card-z", "", 1000)
	listed.CardIP = "192.0.2.77"
	if d := e.decide(listed, true); d.Action != risk.Block || !strings.Contains(d.Rules[0].Description, "ip 192.0.2.77 is on the block list") {
		t.Fatalf("an entry for the address the vault saw: %+v", d)
	}
}

func TestListEntriesArePaged(t *testing.T) {
	e := newEnv(t)
	err := postgres.InTx(t.Context(), e.pool, func(tx pgx.Tx) error {
		for i := range 5 {
			if _, err := e.svc.AddListItem(t.Context(), tx, e.owner, "block", "bin", fmt.Sprintf("4000000%d", i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first, more, err := e.svc.ListItems(t.Context(), e.pool, e.owner, page.Request{Limit: 3})
	if err != nil || len(first) != 3 || !more || first[0].Value != "40000004" {
		t.Fatalf("the first page: %+v, %t, %v", first, more, err)
	}
	rest, more, err := e.svc.ListItems(t.Context(), e.pool, e.owner, page.Request{Limit: 3, StartingAfter: first[2].ID})
	if err != nil || len(rest) != 2 || more {
		t.Fatalf("the second page: %+v, %t, %v", rest, more, err)
	}
}

// Entries stored before values were written one way are rewritten as attempts are matched;
// one that could never match goes, and so does one that becomes another's twin.
func TestOldListEntriesAreMadeCanonical(t *testing.T) {
	e := newEnv(t)
	m := e.owner.Merchant.String()
	for i, value := range []string{"::FFFF:192.0.2.9", "2001:DB8:0:0::1", "2001:db8::1", "not an address"} {
		if _, err := e.pool.Exec(t.Context(), `INSERT INTO risk.list_items (id, merchant_id, livemode, list, kind, value, created_at)
			VALUES ($1, $2, false, 'block', 'ip', $3, now())`, fmt.Sprintf("rli_old%d", i), m, value); err != nil {
			t.Fatal(err)
		}
	}
	up, err := migrations.FS.ReadFile("00002_canonical_list_values.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(t.Context(), strings.Split(string(up), "-- +goose Down")[0]); err != nil {
		t.Fatal(err)
	}
	items, _, err := e.svc.ListItems(t.Context(), e.pool, e.owner, page.Request{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, item := range items {
		values = append(values, item.Value)
	}
	slices.Sort(values)
	if !slices.Equal(values, []string{"192.0.2.9", "2001:db8::1"}) {
		t.Fatalf("entries after the migration: %v", values)
	}
}
