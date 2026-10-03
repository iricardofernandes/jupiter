package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/iricardofernandes/jupiter/internal/events/db"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/pkg/webhook"
)

const (
	deliveryTimeout     = 10 * time.Second
	maxDeliveryAttempts = 12
	maxRetryDelay       = 12 * time.Hour
	maxErrorLength      = 500
)

var errPrivateAddress = errors.New("events: webhook address is not publicly routable")

type DeliveryArgs struct {
	EventID    string `json:"event_id"`
	EndpointID string `json:"endpoint_id"`
}

func (DeliveryArgs) Kind() string { return "webhook_delivery" }

// InsertOpts puts deliveries in a queue of their own, so that slow endpoints wait on
// each other and not on the rest of the background.
func (DeliveryArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: maxDeliveryAttempts, Queue: jobs.QueueWebhooks}
}

// RegisterWorkers adds the webhook delivery worker to workers.
func (s *Service) RegisterWorkers(workers *river.Workers, pool *pgxpool.Pool) {
	river.AddWorker(workers, &deliveryWorker{service: s, pool: pool})
}

type deliveryWorker struct {
	river.WorkerDefaults[DeliveryArgs]
	service *Service
	pool    *pgxpool.Pool
}

// NextRetry doubles the delay after each failed attempt, up to a cap: with the default
// base of 30 seconds, twelve attempts span about 17 hours.
func (w *deliveryWorker) NextRetry(job *river.Job[DeliveryArgs]) time.Time {
	delay := w.service.cfg.RetryBase << min(job.Attempt-1, 20)
	return w.service.cfg.Now().Add(min(delay, maxRetryDelay))
}

func (w *deliveryWorker) Timeout(*river.Job[DeliveryArgs]) time.Duration {
	return deliveryTimeout + 5*time.Second
}

func (w *deliveryWorker) Work(ctx context.Context, job *river.Job[DeliveryArgs]) error {
	s, q := w.service, db.New(w.pool)
	eventRow, err := q.GetEventByID(ctx, job.Args.EventID)
	if err != nil {
		return fmt.Errorf("loading event %s: %w", job.Args.EventID, err)
	}
	endpointRow, err := q.GetEndpointByID(ctx, job.Args.EndpointID)
	if err != nil {
		return fmt.Errorf("loading endpoint %s: %w", job.Args.EndpointID, err)
	}
	if endpointRow.DeletedAt.Valid || Status(endpointRow.Status) != Enabled {
		return river.JobCancel(errors.New("endpoint deleted or disabled"))
	}
	event, err := eventFromRow(eventRow)
	if err != nil {
		return err
	}
	body, err := s.cfg.Render(event, endpointRow.ApiVersion)
	if err != nil {
		return river.JobCancel(fmt.Errorf("rendering %s: %w", event.ID, err))
	}
	secrets, err := s.activeSecrets(ctx, q, endpointRow.ID)
	if err != nil {
		return err
	}
	if !s.inFlight.take(endpointRow.MerchantID) {
		return river.JobSnooze(busySnooze)
	}
	defer s.inFlight.release(endpointRow.MerchantID)

	began := s.cfg.Now()
	status, sendErr := s.send(ctx, endpointRow.Url, body, webhook.Sign(body, began, secrets...))
	record := db.InsertDeliveryParams{
		ID: deliveryPrefix.New().String(), EventID: eventRow.ID, EndpointID: endpointRow.ID,
		Attempt: int32(job.Attempt), Succeeded: sendErr == nil, //nolint:gosec // attempts are capped
		DurationMs:  int32(s.cfg.Now().Sub(began).Milliseconds()), //nolint:gosec // bounded by the delivery timeout
		AttemptedAt: pgtype.Timestamptz{Time: began.UTC(), Valid: true},
	}
	if status != 0 {
		record.ResponseStatus = pgtype.Int4{Int32: int32(status), Valid: true} //nolint:gosec // an HTTP status code
	}
	if sendErr != nil {
		record.Error = truncate(sendErr.Error(), maxErrorLength)
	}
	if err := q.InsertDelivery(ctx, record); err != nil {
		if sendErr == nil {
			// The endpoint has the event: failing the job would send it again. Only the
			// log of the attempt is lost.
			s.cfg.Logger.ErrorContext(ctx, "recording a delivery the endpoint took", "event", eventRow.ID, "endpoint", endpointRow.ID, "error", err)
			return nil
		}
		return fmt.Errorf("recording delivery: %w", err)
	}
	if sendErr != nil && job.Attempt >= job.MaxAttempts {
		s.disableIfFailing(ctx, q, endpointRow.ID)
	}
	return sendErr
}

// A merchant has at most maxInFlight deliveries running at once in a process; more wait
// busySnooze, not counted as an attempt. Its slow endpoints then keep its own deliveries
// waiting, not the other merchants'.
const (
	maxInFlight = 4
	busySnooze  = 5 * time.Second
)

type inFlight struct {
	mu      sync.Mutex
	running map[string]int
}

func (f *inFlight) take(merchantID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[merchantID] >= maxInFlight {
		return false
	}
	f.running[merchantID]++
	return true
}

func (f *inFlight) release(merchantID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[merchantID]--; f.running[merchantID] <= 0 {
		delete(f.running, merchantID)
	}
}

// failingFor is how long an endpoint may take no delivery before it is disabled.
const failingFor = 3 * 24 * time.Hour

// disableIfFailing disables an endpoint that has taken no delivery for failingFor, when
// an event's last attempt to it fails: its deliveries would only keep the queue busy.
// The merchant enables it again once it answers.
func (s *Service) disableIfFailing(ctx context.Context, q *db.Queries, endpointID string) {
	since := pgtype.Timestamptz{Time: s.cfg.Now().Add(-failingFor).UTC(), Valid: true}
	delivered, err := q.DeliveredSince(ctx, db.DeliveredSinceParams{EndpointID: endpointID, Since: since})
	if err == nil && !delivered {
		var n int64
		if n, err = q.DisableFailingEndpoint(ctx, db.DisableFailingEndpointParams{ID: endpointID, Since: since}); n > 0 {
			s.cfg.Logger.WarnContext(ctx, "a webhook endpoint that took no delivery for three days was disabled", "endpoint", endpointID)
		}
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(ctx, "checking a failing webhook endpoint", "endpoint", endpointID, "error", err)
	}
}

func (s *Service) send(ctx context.Context, url string, body []byte, signature string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Jupiter-Webhooks/1")
	req.Header.Set(webhook.SignatureHeader, signature)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("endpoint answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// deliveryClient refuses to connect to addresses inside private networks, checked
// after DNS resolution so a hostname cannot smuggle one in, and never follows
// redirects.
func deliveryClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !isPublic(ap.Addr()) {
				return fmt.Errorf("%w: %s", errPrivateAddress, ap.Addr())
			}
			return nil
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the default transport's type is fixed
	transport.DialContext = dialer.DialContext
	transport.Proxy = nil
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func isPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Ranges IsGlobalUnicast accepts that must still not be reachable: shared address
// space, benchmarking, reserved IPv4, and the IPv6 prefixes that translate to IPv4 and
// could reach a private address through NAT64 or 6to4.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence, which
// PostgreSQL would refuse to store.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type Delivery struct {
	Endpoint       string
	Attempt        int
	Succeeded      bool
	ResponseStatus int
	Error          string
	AttemptedAt    time.Time
}

// Deliveries lists every attempt to deliver an event, oldest first.
func (s *Service) Deliveries(ctx context.Context, q db.DBTX, eventID string) ([]Delivery, error) {
	rows, err := db.New(q).ListDeliveries(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("listing deliveries: %w", err)
	}
	out := make([]Delivery, len(rows))
	for i, row := range rows {
		out[i] = Delivery{
			Endpoint: row.EndpointID, Attempt: int(row.Attempt), Succeeded: row.Succeeded,
			ResponseStatus: int(row.ResponseStatus.Int32), Error: row.Error, AttemptedAt: row.AttemptedAt.Time,
		}
	}
	return out, nil
}
