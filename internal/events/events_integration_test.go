//go:build integration

package events_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/pkg/webhook"
)

var server *postgrestest.Server

func TestMain(m *testing.M) { os.Exit(postgrestest.Main(m, &server, events.Migrate, jobs.Migrate)) }

var merchantPrefix = id.MustPrefix("mch")

type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

type harness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *events.Service
	clock  *clock
	owner  events.Owner
	client *jobs.Client
}

func render(e events.Event, apiVersion string) ([]byte, error) {
	return json.Marshal(map[string]any{"id": e.ID.String(), "type": e.Type, "api_version": apiVersion})
}

func newHarness(t *testing.T, allowPrivate bool, configure ...func(*events.Config)) *harness {
	t.Helper()
	pool := server.Pool(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	inserter, err := jobs.NewInserter(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{}
	cfg := events.Config{
		Box: box, Jobs: inserter, Render: render, Now: c.Now,
		AllowPrivateNetworks: allowPrivate, RetryBase: 20 * time.Millisecond,
	}
	for _, f := range configure {
		f(&cfg)
	}
	svc := events.New(cfg)
	return &harness{t: t, pool: pool, svc: svc, clock: c, owner: events.Owner{Merchant: merchantPrefix.New()}}
}

// startWorker runs deliveries in the background until the test ends.
func (h *harness) startWorker() {
	h.t.Helper()
	workers := river.NewWorkers()
	h.svc.RegisterWorkers(workers, h.pool)
	client, err := jobs.NewWorker(h.pool, jobs.WorkerConfig{Workers: workers, FetchPollInterval: 20 * time.Millisecond})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- jobs.Run(client)(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			h.t.Errorf("worker: %v", err)
		}
	})
	h.client = client
}

func (h *harness) inTx(fn func(pgx.Tx) error) {
	h.t.Helper()
	if err := postgres.InTx(h.t.Context(), h.pool, fn); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) endpoint(url string, enabled ...string) (events.Endpoint, string) {
	h.t.Helper()
	var e events.Endpoint
	var secret string
	h.inTx(func(tx pgx.Tx) error {
		var err error
		e, secret, err = h.svc.CreateEndpoint(h.t.Context(), tx, h.owner, events.EndpointSpec{URL: url, EnabledEvents: enabled}, "2026-09-30")
		return err
	})
	return e, secret
}

func (h *harness) publish() events.Event {
	h.t.Helper()
	var e events.Event
	h.inTx(func(tx pgx.Tx) error {
		var err error
		e, err = h.svc.Publish(h.t.Context(), tx, h.owner, events.TypeAPIKeyCreated, events.ObjectRef{ID: "key_x", Type: "api_key"})
		return err
	})
	return e
}

func (h *harness) waitForDeliveries(eventID string, want func([]events.Delivery) bool) []events.Delivery {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		deliveries, err := h.svc.Deliveries(h.t.Context(), h.pool, eventID)
		if err != nil {
			h.t.Fatal(err)
		}
		if want(deliveries) {
			return deliveries
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("deliveries of %s never reached the expected state: %+v", eventID, deliveries)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func succeeded(n int) func([]events.Delivery) bool {
	return func(ds []events.Delivery) bool {
		count := 0
		for _, d := range ds {
			if d.Succeeded {
				count++
			}
		}
		return count >= n
	}
}

// receiver is a merchant's webhook handler, written the way the documentation tells
// merchants to write one: verify the signature, then drop events already processed.
type receiver struct {
	mu        sync.Mutex
	secret    string
	now       func() time.Time
	failFirst int
	received  int
	processed map[string]int
	requests  []capturedRequest
}

type capturedRequest struct {
	body   []byte
	header string
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, _ := io.ReadAll(req.Body)
	header := req.Header.Get(webhook.SignatureHeader)
	r.requests = append(r.requests, capturedRequest{body: body, header: header})
	r.received++
	if r.received <= r.failFirst {
		http.Error(w, "temporarily broken", http.StatusInternalServerError)
		return
	}
	if err := webhook.Verify(body, header, r.secret, webhook.DefaultTolerance, r.now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var event struct{ ID string }
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.processed[event.ID]++
	if r.processed[event.ID] > 1 {
		r.processed[event.ID] = 1 // already handled: acknowledge without acting again
	}
	w.WriteHeader(http.StatusOK)
}

func newReceiver(now func() time.Time) (*receiver, *httptest.Server) {
	r := &receiver{now: now, processed: map[string]int{}}
	return r, httptest.NewServer(r)
}

func TestAnEventExistsOnlyIfItsTransactionCommits(t *testing.T) {
	h := newHarness(t, true)
	_, srv := newReceiver(time.Now)
	defer srv.Close()
	h.endpoint(srv.URL, "*")

	rollback := errors.New("rollback")
	err := postgres.InTx(t.Context(), h.pool, func(tx pgx.Tx) error {
		if _, err := h.svc.Publish(t.Context(), tx, h.owner, events.TypeAPIKeyCreated, events.ObjectRef{ID: "key_x", Type: "api_key"}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var eventsCount, jobsCount int
	if err := h.pool.QueryRow(t.Context(), "SELECT count(*) FROM events.events WHERE type = 'api_key.created'").Scan(&eventsCount); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(t.Context(), "SELECT count(*) FROM river.river_job").Scan(&jobsCount); err != nil {
		t.Fatal(err)
	}
	// The endpoint's own webhook_endpoint.created event and its delivery job committed;
	// nothing of the rolled-back transaction did.
	if eventsCount != 0 || jobsCount != 1 {
		t.Fatalf("after rollback: %d api_key events, %d jobs; want 0 and 1", eventsCount, jobsCount)
	}
}

func TestOnlySubscribedEnabledEndpointsOfTheSameOwnerReceiveAnEvent(t *testing.T) {
	h := newHarness(t, true)
	all, _ := h.endpoint("https://example.com/all", "*")
	keys, _ := h.endpoint("https://example.com/keys", events.TypeAPIKeyCreated)
	h.endpoint("https://example.com/other", events.TypeAPIKeyRevoked)
	disabled, _ := h.endpoint("https://example.com/disabled", "*")
	deleted, _ := h.endpoint("https://example.com/deleted", "*")
	status := events.Disabled
	h.inTx(func(tx pgx.Tx) error {
		if _, err := h.svc.UpdateEndpoint(t.Context(), tx, h.owner, disabled.ID, events.EndpointUpdate{Status: &status}); err != nil {
			return err
		}
		return h.svc.DeleteEndpoint(t.Context(), tx, h.owner, deleted.ID)
	})
	liveOwner := events.Owner{Merchant: h.owner.Merchant, Livemode: true}
	h.inTx(func(tx pgx.Tx) error {
		_, _, err := h.svc.CreateEndpoint(t.Context(), tx, liveOwner, events.EndpointSpec{URL: "https://example.com/live", EnabledEvents: []string{"*"}}, "2026-09-30")
		return err
	})

	e := h.publish()
	var targets []string
	rows, err := h.pool.Query(t.Context(), "SELECT args->>'endpoint_id' FROM river.river_job WHERE args->>'event_id' = $1 ORDER BY 1", e.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	targets, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{all.ID.String(), keys.ID.String()}
	if strings.Join(targets, ",") != strings.Join(sortStrings(want), ",") {
		t.Fatalf("deliveries enqueued for %v, want %v", targets, want)
	}
}

func sortStrings(s []string) []string {
	if s[0] > s[1] {
		s[0], s[1] = s[1], s[0]
	}
	return s
}

// A receiver verifies signatures, rejects a replay
// outside the tolerance and deduplicates by event id. Deliveries fail twice first, so
// the retry path runs too.
func TestDeliveryIsSignedRetriedAndDeduplicated(t *testing.T) {
	h := newHarness(t, true)
	recv, srv := newReceiver(time.Now)
	defer srv.Close()
	endpoint, secret := h.endpoint(srv.URL, events.TypeAPIKeyCreated)
	recv.secret = secret
	recv.failFirst = 2
	h.startWorker()

	e := h.publish()
	deliveries := h.waitForDeliveries(e.ID.String(), succeeded(1))
	if len(deliveries) != 3 || deliveries[0].ResponseStatus != 500 || !deliveries[2].Succeeded {
		t.Fatalf("deliveries = %+v, want two failures then a success", deliveries)
	}

	h.inTx(func(tx pgx.Tx) error {
		n, err := h.svc.Resend(t.Context(), tx, h.owner, e.ID, &endpoint.ID)
		if n != 1 {
			t.Errorf("Resend enqueued %d deliveries, want 1", n)
		}
		return err
	})
	h.waitForDeliveries(e.ID.String(), succeeded(2))

	recv.mu.Lock()
	defer recv.mu.Unlock()
	if recv.processed[e.ID.String()] != 1 {
		t.Fatalf("event processed %d times, want once despite two successful deliveries", recv.processed[e.ID.String()])
	}
	captured := recv.requests[len(recv.requests)-1]
	if err := webhook.Verify(captured.body, captured.header, secret, webhook.DefaultTolerance, time.Now().Add(6*time.Minute)); !errors.Is(err, webhook.ErrTimestampOutsideTolerance) {
		t.Fatalf("replaying a captured delivery six minutes later: %v, want ErrTimestampOutsideTolerance", err)
	}
	var body map[string]string
	if err := json.Unmarshal(captured.body, &body); err != nil || body["api_version"] != "2026-09-30" {
		t.Fatalf("body = %s, want the endpoint's API version", captured.body)
	}
}

func TestRollingASecretSignsWithBothUntilTheOverlapEnds(t *testing.T) {
	h := newHarness(t, true)
	recv, srv := newReceiver(h.clock.Now)
	defer srv.Close()
	endpoint, oldSecret := h.endpoint(srv.URL, events.TypeAPIKeyCreated)
	recv.secret = oldSecret
	h.startWorker()

	var newSecret string
	h.inTx(func(tx pgx.Tx) error {
		var err error
		newSecret, err = h.svc.RollSecret(t.Context(), tx, h.owner, endpoint.ID, time.Hour)
		return err
	})
	e := h.publish()
	h.waitForDeliveries(e.ID.String(), succeeded(1))
	recv.mu.Lock()
	during := recv.requests[len(recv.requests)-1]
	recv.mu.Unlock()
	for _, secret := range []string{oldSecret, newSecret} {
		if err := webhook.Verify(during.body, during.header, secret, webhook.DefaultTolerance, h.clock.Now()); err != nil {
			t.Errorf("during the overlap, a secret failed to verify: %v", err)
		}
	}

	h.clock.Advance(time.Hour + time.Second)
	recv.mu.Lock()
	recv.secret = newSecret
	recv.mu.Unlock()
	e = h.publish()
	h.waitForDeliveries(e.ID.String(), succeeded(1))
	recv.mu.Lock()
	after := recv.requests[len(recv.requests)-1]
	recv.mu.Unlock()
	if strings.Count(after.header, "v1=") != 1 {
		t.Fatalf("after the overlap header = %q, want one signature", after.header)
	}
	if err := webhook.Verify(after.body, after.header, oldSecret, webhook.DefaultTolerance, h.clock.Now()); !errors.Is(err, webhook.ErrSignatureMismatch) {
		t.Fatalf("the old secret still verifies after the overlap: %v", err)
	}
}

func TestDeliveryRefusesPrivateAddresses(t *testing.T) {
	h := newHarness(t, false)
	recv, srv := newReceiver(time.Now)
	defer srv.Close()
	_, recv.secret = h.endpoint(srv.URL, events.TypeAPIKeyCreated)
	h.startWorker()

	e := h.publish()
	deliveries := h.waitForDeliveries(e.ID.String(), func(ds []events.Delivery) bool { return len(ds) > 0 })
	if deliveries[0].Succeeded || !strings.Contains(deliveries[0].Error, "not publicly routable") {
		t.Fatalf("delivery to %s = %+v, want refused as private", srv.URL, deliveries[0])
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if recv.received != 0 {
		t.Fatalf("the private receiver got %d requests", recv.received)
	}
}

func TestEndpointValidation(t *testing.T) {
	h := newHarness(t, true)
	live := events.Owner{Merchant: h.owner.Merchant, Livemode: true}
	tests := []struct {
		name  string
		owner events.Owner
		spec  events.EndpointSpec
	}{
		{"relative url", h.owner, events.EndpointSpec{URL: "/hooks", EnabledEvents: []string{"*"}}},
		{"ftp url", h.owner, events.EndpointSpec{URL: "ftp://example.com", EnabledEvents: []string{"*"}}},
		{"credentials in url", h.owner, events.EndpointSpec{URL: "https://user:pass@example.com", EnabledEvents: []string{"*"}}}, //nolint:gosec // a fake credential the test expects to be refused
		{"http in live mode", live, events.EndpointSpec{URL: "http://example.com", EnabledEvents: []string{"*"}}},
		{"no events", h.owner, events.EndpointSpec{URL: "https://example.com"}},
		{"unknown event", h.owner, events.EndpointSpec{URL: "https://example.com", EnabledEvents: []string{"charge.succeeded"}}},
	}
	for _, tt := range tests {
		err := postgres.InTx(t.Context(), h.pool, func(tx pgx.Tx) error {
			_, _, err := h.svc.CreateEndpoint(t.Context(), tx, tt.owner, tt.spec, "2026-09-30")
			return err
		})
		if !errors.Is(err, events.ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", tt.name, err)
		}
	}
}

func TestSecretsAreStoredSealed(t *testing.T) {
	h := newHarness(t, true)
	_, secret := h.endpoint("https://example.com", "*")
	var sealed []byte
	if err := h.pool.QueryRow(t.Context(), "SELECT sealed FROM events.endpoint_secrets").Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), secret) || strings.Contains(string(sealed), strings.TrimPrefix(secret, "whsec_")) {
		t.Fatal("the stored secret is readable")
	}
}

// A merchant has so many endpoints in a mode, each a delivery of every event; deleting
// one makes room.
func TestEndpointsAreCapped(t *testing.T) {
	h := newHarness(t, true)
	var first events.Endpoint
	for i := range events.MaxEndpoints {
		e, _ := h.endpoint(fmt.Sprintf("https://example.com/hooks/%d", i), "*")
		if i == 0 {
			first = e
		}
	}
	create := func() error {
		return postgres.InTx(t.Context(), h.pool, func(tx pgx.Tx) error {
			_, _, err := h.svc.CreateEndpoint(t.Context(), tx, h.owner, events.EndpointSpec{URL: "https://example.com/one-more", EnabledEvents: []string{"*"}}, "2026-09-30")
			return err
		})
	}
	if err := create(); !errors.Is(err, events.ErrInvalid) {
		t.Fatalf("one endpoint past the cap: %v", err)
	}
	h.inTx(func(tx pgx.Tx) error {
		return h.svc.DeleteEndpoint(t.Context(), tx, h.owner, first.ID)
	})
	if err := create(); err != nil {
		t.Fatalf("after deleting one: %v", err)
	}
}

// An endpoint that has taken no delivery for three days is disabled when an event's last
// attempt to it fails; one that answered recently is not.
func TestAnEndpointFailingForDaysIsDisabled(t *testing.T) {
	h := newHarness(t, true, func(c *events.Config) { c.MaxAttempts = 1 })
	recv, srv := newReceiver(time.Now)
	defer srv.Close()
	failing, _ := h.endpoint(srv.URL, events.TypeAPIKeyCreated)
	recv.failFirst = 1000
	h.startWorker()

	e := h.publish()
	h.waitForDeliveries(e.ID.String(), func(ds []events.Delivery) bool { return len(ds) == 1 })
	if got := h.status(failing.ID); got != events.Enabled {
		t.Fatalf("a new endpoint failing an event: %s", got)
	}
	h.clock.Advance(4 * 24 * time.Hour)
	e = h.publish()
	h.waitForDeliveries(e.ID.String(), func(ds []events.Delivery) bool { return len(ds) == 1 })
	deadline := time.Now().Add(5 * time.Second)
	for h.status(failing.ID) != events.Disabled {
		if time.Now().After(deadline) {
			t.Fatal("an endpoint that took nothing for four days stayed enabled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) status(endpointID id.ID) events.Status {
	h.t.Helper()
	e, err := h.svc.Endpoint(h.t.Context(), h.pool, h.owner, endpointID)
	if err != nil {
		h.t.Fatal(err)
	}
	return e.Status
}
