//go:build simulation

// Package simulation runs many payments at once against the whole stack, in one
// goroutine driven by a seeded random source: which payment moves next, what the rail
// answers, which requests die half-way and which are sent twice. Because every decision
// comes from the seed and the clock is virtual, a failing seed replays the same run.
package simulation_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/vaulttest"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, acquirer.Migrate},
		vaulttest.Template:           {vaulttest.Migrate},
	}))
}

// Phase 3 exit criterion: SIM_PAYMENTS seeded payments (10,000 in CI) with injected
// faults end with zero invariant violations. SIM_SEED replays a run. Since phase 4 some
// payments use cards saved in the vault, which fails now and then too; since phase 5
// some are live, over ISO 8583 to the card network simulator, which loses requests and
// answers, answers late and answers twice.
func TestSimulation(t *testing.T) {
	seed := envUint("SIM_SEED", 0)
	if seed == 0 {
		var b [8]byte
		_, _ = rand.Read(b[:])
		seed = binary.LittleEndian.Uint64(b[:])
	}
	payments := int(min(envUint("SIM_PAYMENTS", 300), 1_000_000))
	t.Logf("seed %d, %d payments; replay with SIM_SEED=%d SIM_PAYMENTS=%d make test-simulation", seed, payments, seed, payments)
	sim := newSim(t, seed)
	digest := sim.run(payments)
	t.Logf("trace digest %x; %s", digest, sim.stats)
	if path := os.Getenv("SIM_TRACE"); path != "" {
		if err := os.WriteFile(path, sim.trace, 0o600); err != nil { //nolint:gosec // a path the developer chose
			t.Fatal(err)
		}
	}
}

// A run's trace, every decision and every answer, must be the same for the same seed.
func TestSimulationIsDeterministic(t *testing.T) {
	const seed, payments = 42, 60
	first := newSim(t, seed).run(payments)
	second := newSim(t, seed).run(payments)
	if first != second {
		t.Fatalf("seed %d produced two different traces: %x and %x", seed, first, second)
	}
}

func envUint(name string, fallback uint64) uint64 {
	v, err := strconv.ParseUint(os.Getenv(name), 10, 64)
	if err != nil {
		return fallback
	}
	return v
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

type sim struct {
	t    *testing.T
	seed uint64
	rng  *mrand.Rand
	// mu guards what the network simulator's goroutines touch: the trace and the count
	// of network faults.
	mu       sync.Mutex
	pool     *pgxpool.Pool
	clock    *clock
	ledger   *ledger.Ledger
	payments *payments.Service
	api      *api.API
	handler  http.Handler
	keys     []string
	liveKeys []string
	network  *cardnetwork.Network
	acquirer *acquirer.Connector
	scripts  []*script
	trace    []byte
	crash    bool
	stats    stats
}

type stats struct {
	requests, crashes, duplicates, lostRequests, lostResponses, lostQueries, vaultFaults, networkFaults, resolverRuns, checks, expiryJumps, live int
	outcomes                                                                                                                                     map[string]int
}

func (s stats) String() string {
	return fmt.Sprintf("%d requests, %d crashed between phases, %d duplicates, rail lost %d requests, %d responses and %d queries, %d vault calls failed, %d live payments with %d network faults, %d background runs, %d jumps past authorization expiry, %d invariant checks, final statuses %v",
		s.requests, s.crashes, s.duplicates, s.lostRequests, s.lostResponses, s.lostQueries, s.vaultFaults, s.live, s.networkFaults, s.resolverRuns, s.expiryJumps, s.checks, s.outcomes)
}

func newSim(t *testing.T, seed uint64) *sim {
	t.Helper()
	url, drop, err := server.Database(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drop(context.WithoutCancel(t.Context())) })
	pool, err := postgres.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	s := &sim{
		// Deliberately a seeded, reproducible generator: the seed is what replays a run.
		t: t, seed: seed, rng: mrand.New(mrand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), pool: pool, //nolint:gosec // reproducibility is the point
		clock: &clock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
		stats: stats{outcomes: map[string]int{}},
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	inserter, err := jobs.NewInserter(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent, Now: s.clock.Now})
	s.ledger = ledger.New(ledger.WithClock(s.clock.Now))
	vaultURL, dropVault, err := server.DatabaseFrom(t.Context(), vaulttest.Template)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dropVault(context.WithoutCancel(t.Context())) })
	vaultPool, err := postgres.Connect(t.Context(), vaultURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vaultPool.Close)
	cards := &faultyVault{sim: s, inner: vaulttest.Start(t, vaultPool, vaulttest.Options{Now: s.clock.Now}).Client}
	s.startNetwork(t, pool, cards)
	s.payments = payments.New(payments.Config{
		Ledger: s.ledger, Events: eventService, Now: s.clock.Now, LiveRail: s.acquirer,
		TestRail: &faultyRail{sim: s, inner: payments.NewTestRail(pool, s.clock.Now, nil).WithCards(cards)},
	})
	merchants := merchant.New(s.clock.Now)
	s.api = api.New(api.Deps{
		Pool: pool, Merchants: merchants, Events: eventService, Payments: s.payments, Vault: cards, Box: box, Now: s.clock.Now,
		// A retry of a request that died finds its key locked until the completer runs;
		// waiting in real time for it would only slow the run down.
		IdempotencyWait: time.Nanosecond,
		AfterPhase: func(string) {
			if s.crash {
				s.crash = false
				s.stats.crashes++
				runtime.Goexit()
			}
		},
	})
	s.handler = s.api.Handler()
	for i := range 3 {
		err := postgres.InTx(t.Context(), pool, func(tx pgx.Tx) error {
			_, keys, err := merchants.Create(t.Context(), tx, fmt.Sprint("merchant ", i), api.CurrentVersion)
			for _, k := range keys {
				switch k.Value[:8] {
				case "sk_test_":
					s.keys = append(s.keys, k.Value)
				case "sk_live_":
					s.liveKeys = append(s.liveKeys, k.Value)
				}
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *sim) record(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trace = fmt.Appendf(s.trace, format+"\n", args...)
}

// faultyRail sits between Jupiter and the test rail and loses requests or responses as
// the seed decides, the way a network does.
type faultyRail struct {
	sim   *sim
	inner payments.Rail
}

func (f *faultyRail) fault(kind string, call func() payments.Result) payments.Result {
	switch roll := f.sim.rng.IntN(100); {
	case roll < 4:
		f.sim.stats.lostRequests++
		f.sim.record("rail %s: request lost", kind)
		return payments.Result{Outcome: payments.Unknown}
	case roll < 8:
		call()
		f.sim.stats.lostResponses++
		f.sim.record("rail %s: response lost", kind)
		return payments.Result{Outcome: payments.Unknown}
	}
	res := call()
	f.sim.record("rail %s: %s %s", kind, res.Outcome, res.DeclineCode)
	return res
}

func (f *faultyRail) Authorize(ctx context.Context, r payments.AuthorizeRequest) payments.Result {
	return f.fault("authorize", func() payments.Result { return f.inner.Authorize(ctx, r) })
}

func (f *faultyRail) Capture(ctx context.Context, r payments.OperationRequest) payments.Result {
	return f.fault("capture", func() payments.Result { return f.inner.Capture(ctx, r) })
}

func (f *faultyRail) Void(ctx context.Context, r payments.OperationRequest) payments.Result {
	return f.fault("void", func() payments.Result { return f.inner.Void(ctx, r) })
}

func (f *faultyRail) Refund(ctx context.Context, r payments.OperationRequest) payments.Result {
	return f.fault("refund", func() payments.Result { return f.inner.Refund(ctx, r) })
}

func (f *faultyRail) Query(ctx context.Context, key string) payments.Result {
	if f.sim.rng.IntN(100) < 4 {
		f.sim.stats.lostQueries++
		f.sim.record("rail query: lost")
		return payments.Result{Outcome: payments.Unknown}
	}
	res := f.inner.Query(ctx, key)
	f.sim.record("rail query: %s", res.Outcome)
	return res
}

// networkTimeout is the acquirer's: short, since every lost answer waits for it.
const networkTimeout = 200 * time.Millisecond

// startNetwork runs the card network simulator, which loses, delays and repeats
// messages as the seed decides, and the acquirer connector live payments go through.
func (s *sim) startNetwork(t *testing.T, pool *pgxpool.Pool, cards payments.CardSource) {
	t.Helper()
	s.network = cardnetwork.New(cardnetwork.Config{Now: s.clock.Now, LateAfter: 2 * networkTimeout, Faults: s.networkFault})
	if err := s.network.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.network.Close)
	connector, err := acquirer.New(acquirer.Config{Pool: pool, Addr: s.network.Addr(), Timeout: networkTimeout, Cards: cards, Now: s.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connector.Close() })
	s.acquirer = connector
}

// networkFault is drawn from the seed and the message, not from the run's generator:
// the network's goroutines answer it, and its STANs come from a sequence the run
// advances in its own order.
func (s *sim) networkFault(m cardnet.Message) cardnetwork.Fault {
	h := fnv.New64a()
	_ = binary.Write(h, binary.LittleEndian, s.seed)
	_, _ = h.Write([]byte(m.MTI + "/" + m.STAN))
	fault := cardnetwork.NoFault
	switch roll := h.Sum64() % 100; {
	case roll < 2:
		fault = cardnetwork.LoseRequest
	case roll < 4:
		fault = cardnetwork.LoseAnswer
	case roll < 5:
		fault = cardnetwork.AnswerLate
	case roll < 6:
		fault = cardnetwork.AnswerTwice
	}
	s.record("network %s: fault %d", m.MTI, fault)
	if fault != cardnetwork.NoFault {
		s.mu.Lock()
		s.stats.networkFaults++
		s.mu.Unlock()
	}
	return fault
}

// faultyVault fails calls to the vault as the seed decides: a request that never
// arrives, or, for tokenization, one whose answer is lost after the vault acted.
type faultyVault struct {
	sim   *sim
	inner *vault.Client
}

func (f *faultyVault) fail(call string, lostAfter bool) (lost, done bool) {
	switch roll := f.sim.rng.IntN(100); {
	case roll < 3:
		f.sim.stats.vaultFaults++
		f.sim.record("vault %s: unavailable", call)
		return true, false
	case lostAfter && roll < 6:
		f.sim.stats.vaultFaults++
		f.sim.record("vault %s: answer lost", call)
		return true, true
	}
	return false, false
}

func (f *faultyVault) Tokenize(ctx context.Context, r vault.TokenizeRequest) (vault.Card, error) {
	lost, after := f.fail("tokenize", true)
	if lost && !after {
		return vault.Card{}, vault.ErrUnavailable
	}
	c, err := f.inner.Tokenize(ctx, r)
	if lost {
		return vault.Card{}, vault.ErrUnavailable
	}
	return c, err
}

func (f *faultyVault) Card(ctx context.Context, token string) (vault.Card, error) {
	if lost, _ := f.fail("card", false); lost {
		return vault.Card{}, vault.ErrUnavailable
	}
	return f.inner.Card(ctx, token)
}

func (f *faultyVault) Claim(ctx context.Context, token, owner string) (vault.Card, error) {
	if lost, _ := f.fail("claim", false); lost {
		return vault.Card{}, vault.ErrUnavailable
	}
	return f.inner.Claim(ctx, token, owner)
}

func (f *faultyVault) Detokenize(ctx context.Context, token, owner string) (vault.CardData, error) {
	if lost, _ := f.fail("detokenize", false); lost {
		return vault.CardData{}, vault.ErrUnavailable
	}
	return f.inner.Detokenize(ctx, token, owner)
}

type response struct {
	status  int
	body    []byte
	crashed bool
}

// send runs one request on its own goroutine and waits for it, so a request that
// "crashes" between phases dies alone while the run stays in one logical thread.
func (s *sim) send(key, method, path, idempotencyKey string, body any) response {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequestWithContext(s.t.Context(), method, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+key)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handler.ServeHTTP(rec, req)
	}()
	<-done
	s.stats.requests++
	if s.crash {
		// Only operations with more than one phase can die between phases.
		s.crash = false
		return response{status: rec.Code, body: rec.Body.Bytes()}
	}
	if rec.Body.Len() == 0 {
		return response{crashed: true}
	}
	return response{status: rec.Code, body: rec.Body.Bytes()}
}
