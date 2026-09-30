package events

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/iricardofernandes/jupiter/internal/events/db"
	"github.com/iricardofernandes/jupiter/internal/events/migrations"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
)

var (
	EventPrefix    = id.MustPrefix("evt")
	EndpointPrefix = id.MustPrefix("we")
	secretPrefix   = id.MustPrefix("whs")
	deliveryPrefix = id.MustPrefix("whd")
)

var (
	ErrInvalid  = errors.New("events: invalid request")
	ErrNotFound = errors.New("events: not found")
)

const (
	TypeWebhookEndpointCreated = "webhook_endpoint.created"
	TypeWebhookEndpointUpdated = "webhook_endpoint.updated"
	TypeWebhookEndpointDeleted = "webhook_endpoint.deleted"
	TypeAPIKeyCreated          = "api_key.created"
	TypeAPIKeyRevoked          = "api_key.revoked"

	TypePaymentIntentCreated        = "payment_intent.created"
	TypePaymentIntentProcessing     = "payment_intent.processing"
	TypePaymentIntentRequiresAction = "payment_intent.requires_action"
	TypePaymentIntentCapturable     = "payment_intent.amount_capturable_updated"
	TypePaymentIntentSucceeded      = "payment_intent.succeeded"
	TypePaymentIntentPaymentFailed  = "payment_intent.payment_failed"
	TypePaymentIntentCanceled       = "payment_intent.canceled"
	TypeRefundCreated               = "refund.created"
	TypeRefundUpdated               = "refund.updated"
)

// Types lists every event type an endpoint can subscribe to, besides "*".
var Types = []string{
	TypeWebhookEndpointCreated, TypeWebhookEndpointUpdated, TypeWebhookEndpointDeleted,
	TypeAPIKeyCreated, TypeAPIKeyRevoked,
	TypePaymentIntentCreated, TypePaymentIntentProcessing, TypePaymentIntentRequiresAction,
	TypePaymentIntentCapturable, TypePaymentIntentSucceeded, TypePaymentIntentPaymentFailed,
	TypePaymentIntentCanceled, TypeRefundCreated, TypeRefundUpdated,
}

// Owner scopes every object to a merchant and a mode; test and live data never mix.
type Owner struct {
	Merchant id.ID
	Livemode bool
}

type ObjectRef struct {
	ID   string
	Type string
}

// Event is thin: it names what changed, and receivers fetch the object's current state.
type Event struct {
	ID        id.ID
	Owner     Owner
	Type      string
	Object    ObjectRef
	CreatedAt time.Time
}

type Inserter interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Renderer turns an event into the JSON body a webhook delivers, in an API version.
type Renderer func(e Event, apiVersion string) ([]byte, error)

type Config struct {
	Box    *secretbox.Box
	Jobs   Inserter
	Render Renderer
	Now    func() time.Time
	// HTTPClient delivers webhooks. Leave it nil to get one that refuses private,
	// loopback and link-local addresses unless AllowPrivateNetworks is set.
	HTTPClient           *http.Client
	AllowPrivateNetworks bool
	// RetryBase is the delay before the first retry of a failed delivery; each later
	// retry doubles it.
	RetryBase time.Duration
}

type Service struct {
	cfg Config
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RetryBase == 0 {
		cfg.RetryBase = 30 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = deliveryClient(cfg.AllowPrivateNetworks)
	}
	return &Service{cfg: cfg}
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "events", migrations.FS)
}

// Publish records an event and enqueues its delivery to every subscribed endpoint, all
// in tx: the event exists exactly when the change that caused it commits.
func (s *Service) Publish(ctx context.Context, tx pgx.Tx, owner Owner, eventType string, object ObjectRef) (Event, error) {
	if !slices.Contains(Types, eventType) {
		return Event{}, fmt.Errorf("%w: unknown event type %q", ErrInvalid, eventType)
	}
	e := Event{ID: EventPrefix.New(), Owner: owner, Type: eventType, Object: object, CreatedAt: s.cfg.Now().UTC()}
	q := db.New(tx)
	if err := q.InsertEvent(ctx, db.InsertEventParams{
		ID: e.ID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		Type: e.Type, ObjectID: object.ID, ObjectType: object.Type, CreatedAt: timestamptz(e.CreatedAt),
	}); err != nil {
		return Event{}, fmt.Errorf("recording event: %w", err)
	}
	endpoints, err := q.SubscribedEndpoints(ctx, db.SubscribedEndpointsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Type: e.Type,
	})
	if err != nil {
		return Event{}, fmt.Errorf("finding subscribers: %w", err)
	}
	for _, endpointID := range endpoints {
		if err := s.enqueue(ctx, tx, e.ID.String(), endpointID); err != nil {
			return Event{}, err
		}
	}
	return e, nil
}

func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, eventID, endpointID string) error {
	if _, err := s.cfg.Jobs.InsertTx(ctx, tx, DeliveryArgs{EventID: eventID, EndpointID: endpointID}, nil); err != nil {
		return fmt.Errorf("enqueuing delivery of %s to %s: %w", eventID, endpointID, err)
	}
	return nil
}

func (s *Service) Event(ctx context.Context, q db.DBTX, owner Owner, eventID id.ID) (Event, error) {
	row, err := db.New(q).GetEvent(ctx, db.GetEventParams{ID: eventID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, fmt.Errorf("%w: %s", ErrNotFound, eventID)
	}
	if err != nil {
		return Event{}, err
	}
	return eventFromRow(row)
}

func (s *Service) Events(ctx context.Context, q db.DBTX, owner Owner, eventType string, r page.Request) ([]Event, bool, error) {
	rows, err := db.New(q).ListEvents(ctx, db.ListEventsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Type: eventType,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing events: %w", err)
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		e, err := eventFromRow(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

// Resend enqueues the event again, to one endpoint or to every endpoint subscribed now,
// and returns how many deliveries it enqueued.
func (s *Service) Resend(ctx context.Context, tx pgx.Tx, owner Owner, eventID id.ID, endpoint *id.ID) (int, error) {
	e, err := s.Event(ctx, tx, owner, eventID)
	if err != nil {
		return 0, err
	}
	q := db.New(tx)
	var targets []string
	if endpoint != nil {
		if _, lookupErr := s.Endpoint(ctx, tx, owner, *endpoint); lookupErr != nil {
			return 0, lookupErr
		}
		targets = []string{endpoint.String()}
	} else if targets, err = q.SubscribedEndpoints(ctx, db.SubscribedEndpointsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Type: e.Type,
	}); err != nil {
		return 0, err
	}
	for _, target := range targets {
		if err := s.enqueue(ctx, tx, e.ID.String(), target); err != nil {
			return 0, err
		}
	}
	return len(targets), nil
}

func eventFromRow(row db.EventsEvent) (Event, error) {
	eventID, err := EventPrefix.Parse(row.ID)
	if err != nil {
		return Event{}, err
	}
	merchantID, err := id.Parse(row.MerchantID)
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:        eventID,
		Owner:     Owner{Merchant: merchantID, Livemode: row.Livemode},
		Type:      row.Type,
		Object:    ObjectRef{ID: row.ObjectID, Type: row.ObjectType},
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
