//go:build load

// Package load measures the authorization path end to end: a merchant's POST
// /v1/payment_intents over HTTP, through the risk engine, the vault and ISO 8583 to the
// card network simulator, to a hold in the ledger, at rising concurrency until it
// saturates. docs/benchmarks/authorization-path.md records the results.
package load_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/authentication"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/risk"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/internal/vault/vaulttest"
	"github.com/iricardofernandes/jupiter/pkg/loadgen"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, risk.Migrate, acquirer.Migrate, authentication.Migrate},
		vaulttest.Template:           {vaulttest.Migrate},
	}))
}

// Each level makes this many payments, after a tenth as many to warm up.
const perLevel = 2000

// cardVelocity is how often the platform's rules let one card pay in an hour, less one.
const cardVelocity = 9

func levels() []int {
	if v := os.Getenv("LOAD_LEVELS"); v != "" {
		var out []int
		for f := range strings.SplitSeq(v, ",") {
			n, err := strconv.Atoi(f)
			if err == nil && n > 0 {
				out = append(out, n)
			}
		}
		return out
	}
	return []int{1, 2, 4, 8, 16, 32, 64, 128, 256}
}

// The authorization path with every payment at one merchant, and spread over eight:
// what one merchant's traffic can reach, and what the platform's can.
func TestAuthorizationPath(t *testing.T) {
	for _, merchants := range []int{1, 8} {
		t.Run(fmt.Sprintf("merchants=%d", merchants), func(t *testing.T) {
			st := newStack(t, merchants)
			stop := st.profile(t, fmt.Sprintf("authorize-%d-merchants", merchants))
			defer stop()
			t.Logf("| Concurrency | Payments/s | p50 | p95 | p99 | Max | Errors |")
			t.Logf("|---|---|---|---|---|---|---|")
			for _, c := range levels() {
				st.nextLevel()
				st.run(t, c, perLevel/10)
				diagnose := st.diagnose(t)
				r := st.run(t, c, perLevel)
				t.Logf("| %d | %.0f | %v | %v | %v | %v | %d |", c, r.throughput(), r.percentile(50), r.percentile(95), r.percentile(99), r.percentile(100), r.errors)
				diagnose(perLevel)
			}
		})
	}
}

type stack struct {
	pool    *pgxpool.Pool
	clock   *offsetClock
	url     string
	keys    []string
	cards   [][]string // payment methods saved at each merchant
	client  *http.Client
	payment atomic.Int64
	// statements counts what the API sends the database, by statement, with LOAD_DIAGNOSE.
	statements *statementCounter
}

type statementCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	name, _, _ := strings.Cut(strings.TrimPrefix(data.SQL, "-- name: "), "\n")
	c.mu.Lock()
	c.counts[strings.Fields(name + " ?")[0]]++
	c.mu.Unlock()
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *statementCounter) take() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.counts
	c.counts = map[string]int{}
	return out
}

// offsetClock is the wall clock, moved forward between levels so that one level's
// payments are out of the next one's velocity windows.
type offsetClock struct{ offset atomic.Int64 }

func (c *offsetClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *offsetClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

func newStack(t *testing.T, merchants int) *stack {
	t.Helper()
	url, drop, err := server.Database(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drop(context.WithoutCancel(t.Context())) })
	if conns := os.Getenv("LOAD_POOL_CONNS"); conns != "" {
		url += "&pool_max_conns=" + conns
	}
	st := &stack{clock: &offsetClock{}, statements: &statementCounter{counts: map[string]int{}}}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LOAD_DIAGNOSE") != "" {
		cfg.ConnConfig.Tracer = st.statements
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st.pool = pool

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
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent, Now: st.clock.Now})
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
	cards := vaulttest.Start(t, vaultPool, vaulttest.Options{}).Client

	network := cardnetwork.New(cardnetwork.Config{Now: st.clock.Now})
	if err := network.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(network.Close)
	connector, err := acquirer.New(acquirer.Config{Pool: pool, Addr: network.Addr(), Timeout: 2 * time.Second, Cards: cards, Now: st.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connector.Close() })

	ledgerService := ledger.New(ledger.WithClock(st.clock.Now))
	engine := risk.New(risk.Config{Now: st.clock.Now})
	paymentService := payments.New(payments.Config{
		Ledger: ledgerService, Events: eventService, Now: st.clock.Now, LiveRail: connector, Risk: engine,
		TestRail: payments.NewTestRail(pool, st.clock.Now, nil).WithCards(cards),
	})
	merchantService := merchant.New(st.clock.Now)
	a := api.New(api.Deps{
		Pool: pool, Merchants: merchantService, Events: eventService, Payments: paymentService, Vault: cards, Risk: engine, Box: box, Now: st.clock.Now,
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	st.url = srv.URL
	st.client = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 512, MaxConnsPerHost: 512}, Timeout: time.Minute}

	for i := range merchants {
		err := postgres.InTx(t.Context(), pool, func(tx pgx.Tx) error {
			_, keys, err := merchantService.Create(t.Context(), tx, fmt.Sprint("merchant ", i), api.CurrentVersion)
			for _, k := range keys {
				if strings.HasPrefix(k.Value, "sk_live_") {
					st.keys = append(st.keys, k.Value)
				}
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	st.saveCards(t)
	return st
}

// saveCards saves, at each merchant, enough cards that no level pays with one more often
// than the platform's velocity rule allows.
func (st *stack) saveCards(t *testing.T) {
	t.Helper()
	perMerchant := (perLevel+cardVelocity-1)/cardVelocity + 1
	rng := mrand.New(mrand.NewPCG(1, 2)) //nolint:gosec // card numbers for a benchmark
	st.cards = make([][]string, len(st.keys))
	for m, key := range st.keys {
		for range perMerchant {
			var pm struct {
				ID string `json:"id"`
			}
			status := st.post(t, key, "/v1/payment_methods", map[string]any{"type": "card", "card": map[string]any{
				"number": loadgen.RandomCardNumber(rng, "49"), "exp_month": 12, "exp_year": 2030,
			}}, &pm)
			if status != http.StatusOK || pm.ID == "" {
				t.Fatalf("saving a card answered %d", status)
			}
			st.cards[m] = append(st.cards[m], pm.ID)
		}
	}
}

func (st *stack) post(t *testing.T, key, path string, body, out any) int {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, st.url+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := st.client.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(data, out)
	}
	return resp.StatusCode
}

// nextLevel moves the clock two hours on: the previous level's payments no longer count
// towards any card's velocity.
func (st *stack) nextLevel() { st.clock.Advance(2 * time.Hour) }

type result struct {
	elapsed   time.Duration
	latencies []time.Duration
	errors    int
}

func (r result) throughput() float64 { return float64(len(r.latencies)) / r.elapsed.Seconds() }

func (r result) percentile(p int) time.Duration {
	sorted := slices.Clone(r.latencies)
	slices.Sort(sorted)
	if len(sorted) == 0 {
		return 0
	}
	return sorted[(len(sorted)-1)*p/100].Round(10 * time.Microsecond)
}

// run authorizes n payments, concurrency at a time, spread over the merchants in turn,
// each with the next of the merchant's cards, and returns how long each took.
func (st *stack) run(t *testing.T, concurrency, n int) result {
	t.Helper()
	jobs := make(chan int)
	latencies := make([]time.Duration, n)
	ok := make([]bool, n)
	var wg sync.WaitGroup
	began := time.Now()
	for range concurrency {
		wg.Go(func() {
			for i := range jobs {
				seq := int(st.payment.Add(1))
				m := seq % len(st.keys)
				card := st.cards[m][(seq/len(st.keys))%len(st.cards[m])]
				var it struct {
					Status string `json:"status"`
				}
				start := time.Now()
				status := st.post(t, st.keys[m], "/v1/payment_intents", map[string]any{
					"amount": 1000 + seq%9000, "currency": "brl", "payment_method": card, "confirm": true, "capture_method": "manual",
				}, &it)
				latencies[i] = time.Since(start)
				ok[i] = status == http.StatusOK && it.Status == "requires_capture"
			}
		})
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	r := result{elapsed: time.Since(began)}
	for i, l := range latencies {
		if ok[i] {
			r.latencies = append(r.latencies, l)
		} else {
			r.errors++
		}
	}
	return r
}

// diagnose, with LOAD_DIAGNOSE set, samples what the database's sessions wait on while a
// level runs, and counts its commits and statements per payment.
func (st *stack) diagnose(t *testing.T) func(payments int) {
	t.Helper()
	if os.Getenv("LOAD_DIAGNOSE") == "" {
		return func(int) {}
	}
	ctx := context.WithoutCancel(t.Context())
	admin, err := pgxpool.New(ctx, st.pool.Config().ConnString()+"&pool_max_conns=2")
	if err != nil {
		t.Fatal(err)
	}
	commits := func() (n int64) {
		_ = admin.QueryRow(ctx, "SELECT xact_commit FROM pg_stat_database WHERE datname = current_database()").Scan(&n)
		return n
	}
	before := commits()
	st.statements.take()
	waits := map[string]int{}
	samples := 0
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
			rows, err := admin.Query(ctx, `SELECT coalesce(wait_event_type, 'CPU') || ':' || coalesce(wait_event, '-') || ' ' || left(regexp_replace(query, '\s+', ' ', 'g'), 70)
				FROM pg_stat_activity WHERE datname = current_database() AND state = 'active' AND pid <> pg_backend_pid()`)
			if err != nil {
				continue
			}
			for rows.Next() {
				var w string
				if rows.Scan(&w) == nil {
					waits[w]++
				}
			}
			rows.Close()
			samples++
		}
	}()
	return func(payments int) {
		close(done)
		<-finished
		_ = admin.QueryRow(ctx, "SELECT pg_stat_clear_snapshot()").Scan(new(any))
		time.Sleep(1100 * time.Millisecond) // the statistics' flush
		statements := st.statements.take()
		total := 0
		for _, n := range statements {
			total += n
		}
		t.Logf("   %.1f commits and %.1f statements a payment: %v", float64(commits()-before)/float64(payments), float64(total)/float64(payments), perPayment(statements, payments))
		t.Logf("   active sessions by wait, per sample:")
		type kv struct {
			k string
			v int
		}
		var top []kv
		for k, v := range waits {
			top = append(top, kv{k, v})
		}
		slices.SortFunc(top, func(a, b kv) int { return b.v - a.v })
		for _, e := range top[:min(12, len(top))] {
			t.Logf("   %5.2f %s", float64(e.v)/float64(max(samples, 1)), e.k)
		}
		admin.Close()
	}
}

func perPayment(counts map[string]int, payments int) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	slices.SortFunc(all, func(a, b kv) int { return b.v - a.v })
	var b strings.Builder
	for _, e := range all {
		fmt.Fprintf(&b, "%s %.1f, ", e.k, float64(e.v)/float64(payments))
	}
	return b.String()
}

// profile writes a CPU profile of the run to LOAD_PROFILE_DIR, if set.
func (st *stack) profile(t *testing.T, name string) func() {
	t.Helper()
	dir := os.Getenv("LOAD_PROFILE_DIR")
	if dir == "" {
		return func() {}
	}
	f, err := os.Create(filepath.Join(dir, name+".cpu.pprof")) //nolint:gosec // a directory the developer chose
	if err != nil {
		t.Fatal(err)
	}
	runtime.SetMutexProfileFraction(5)
	runtime.SetBlockProfileRate(int(time.Millisecond))
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	return func() {
		pprof.StopCPUProfile()
		_ = f.Close()
		for _, p := range []string{"mutex", "block"} {
			out, err := os.Create(filepath.Join(dir, name+"."+p+".pprof")) //nolint:gosec // a directory the developer chose
			if err != nil {
				t.Fatal(err)
			}
			_ = pprof.Lookup(p).WriteTo(out, 0)
			_ = out.Close()
		}
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
	}
}
