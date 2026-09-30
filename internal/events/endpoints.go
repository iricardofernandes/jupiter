package events

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/events/db"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
)

type Status string

const (
	Enabled  Status = "enabled"
	Disabled Status = "disabled"
)

// MaxSecretOverlap bounds how long a rolled secret keeps signing beside its successor.
const MaxSecretOverlap = 7 * 24 * time.Hour

type Endpoint struct {
	ID            id.ID
	Owner         Owner
	URL           string
	Description   string
	EnabledEvents []string
	Status        Status
	APIVersion    string
	CreatedAt     time.Time
}

type EndpointSpec struct {
	URL           string
	Description   string
	EnabledEvents []string
}

// EndpointUpdate changes only the fields that are set.
type EndpointUpdate struct {
	URL           *string
	Description   *string
	EnabledEvents []string
	Status        *Status
}

// CreateEndpoint returns the endpoint and its signing secret, which is shown only here.
func (s *Service) CreateEndpoint(ctx context.Context, tx pgx.Tx, owner Owner, spec EndpointSpec, apiVersion string) (Endpoint, string, error) {
	e := Endpoint{
		ID: EndpointPrefix.New(), Owner: owner, URL: spec.URL, Description: spec.Description,
		EnabledEvents: spec.EnabledEvents, Status: Enabled, APIVersion: apiVersion, CreatedAt: s.cfg.Now().UTC(),
	}
	if err := e.validate(); err != nil {
		return Endpoint{}, "", err
	}
	q := db.New(tx)
	if err := q.InsertEndpoint(ctx, db.InsertEndpointParams{
		ID: e.ID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Url: e.URL,
		Description: e.Description, EnabledEvents: e.EnabledEvents, Status: string(e.Status),
		ApiVersion: apiVersion, CreatedAt: timestamptz(e.CreatedAt),
	}); err != nil {
		return Endpoint{}, "", fmt.Errorf("creating endpoint: %w", err)
	}
	secret, err := s.addSecret(ctx, q, e.ID)
	if err != nil {
		return Endpoint{}, "", err
	}
	if _, err := s.Publish(ctx, tx, owner, TypeWebhookEndpointCreated, e.ref()); err != nil {
		return Endpoint{}, "", err
	}
	return e, secret, nil
}

func (s *Service) Endpoint(ctx context.Context, q db.DBTX, owner Owner, endpointID id.ID) (Endpoint, error) {
	row, err := db.New(q).GetEndpoint(ctx, db.GetEndpointParams{
		ID: endpointID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, fmt.Errorf("%w: %s", ErrNotFound, endpointID)
	}
	if err != nil {
		return Endpoint{}, err
	}
	return endpointFromRow(row)
}

func (s *Service) Endpoints(ctx context.Context, q db.DBTX, owner Owner, r page.Request) ([]Endpoint, bool, error) {
	rows, err := db.New(q).ListEndpoints(ctx, db.ListEndpointsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing endpoints: %w", err)
	}
	out := make([]Endpoint, 0, len(rows))
	for _, row := range rows {
		e, err := endpointFromRow(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

// UpdateEndpoint locks the endpoint while it merges the update, so concurrent partial
// updates of different fields do not overwrite each other.
func (s *Service) UpdateEndpoint(ctx context.Context, tx pgx.Tx, owner Owner, endpointID id.ID, u EndpointUpdate) (Endpoint, error) {
	locked, err := db.New(tx).LockEndpoint(ctx, db.LockEndpointParams{
		ID: endpointID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, fmt.Errorf("%w: %s", ErrNotFound, endpointID)
	}
	if err != nil {
		return Endpoint{}, err
	}
	e, err := endpointFromRow(locked)
	if err != nil {
		return Endpoint{}, err
	}
	if u.URL != nil {
		e.URL = *u.URL
	}
	if u.Description != nil {
		e.Description = *u.Description
	}
	if u.EnabledEvents != nil {
		e.EnabledEvents = u.EnabledEvents
	}
	if u.Status != nil {
		e.Status = *u.Status
	}
	if invalid := e.validate(); invalid != nil {
		return Endpoint{}, invalid
	}
	row, err := db.New(tx).UpdateEndpoint(ctx, db.UpdateEndpointParams{
		Url: e.URL, Description: e.Description, EnabledEvents: e.EnabledEvents, Status: string(e.Status),
		ID: e.ID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
	})
	if err != nil {
		return Endpoint{}, fmt.Errorf("updating endpoint: %w", err)
	}
	if _, err := s.Publish(ctx, tx, owner, TypeWebhookEndpointUpdated, e.ref()); err != nil {
		return Endpoint{}, err
	}
	return endpointFromRow(row)
}

func (s *Service) DeleteEndpoint(ctx context.Context, tx pgx.Tx, owner Owner, endpointID id.ID) error {
	_, err := db.New(tx).DeleteEndpoint(ctx, db.DeleteEndpointParams{
		Now: timestamptz(s.cfg.Now().UTC()), ID: endpointID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, endpointID)
	}
	if err != nil {
		return fmt.Errorf("deleting endpoint: %w", err)
	}
	_, err = s.Publish(ctx, tx, owner, TypeWebhookEndpointDeleted, ObjectRef{ID: endpointID.String(), Type: "webhook_endpoint"})
	return err
}

// RollSecret issues a new signing secret. Current secrets keep signing beside it until
// overlap has passed, so receivers can switch without dropping a delivery.
func (s *Service) RollSecret(ctx context.Context, tx pgx.Tx, owner Owner, endpointID id.ID, overlap time.Duration) (string, error) {
	if overlap < 0 || overlap > MaxSecretOverlap {
		return "", fmt.Errorf("%w: overlap must be between 0 and %v", ErrInvalid, MaxSecretOverlap)
	}
	if _, err := s.Endpoint(ctx, tx, owner, endpointID); err != nil {
		return "", err
	}
	q := db.New(tx)
	now := s.cfg.Now().UTC()
	if err := q.ExpireSecrets(ctx, db.ExpireSecretsParams{
		ExpiresAt: timestamptz(now.Add(overlap)), EndpointID: endpointID.String(), Now: timestamptz(now),
	}); err != nil {
		return "", fmt.Errorf("expiring secrets: %w", err)
	}
	return s.addSecret(ctx, q, endpointID)
}

func (s *Service) addSecret(ctx context.Context, q *db.Queries, endpointID id.ID) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating secret: %w", err)
	}
	secret := "whsec_" + base64.RawURLEncoding.EncodeToString(raw)
	secretID := secretPrefix.New()
	sealed, err := s.cfg.Box.Seal([]byte(secret), secretAD(endpointID.String(), secretID.String()))
	if err != nil {
		return "", err
	}
	if err := q.InsertSecret(ctx, db.InsertSecretParams{
		ID: secretID.String(), EndpointID: endpointID.String(), Sealed: sealed, CreatedAt: timestamptz(s.cfg.Now().UTC()),
	}); err != nil {
		return "", fmt.Errorf("storing secret: %w", err)
	}
	return secret, nil
}

func (s *Service) activeSecrets(ctx context.Context, q *db.Queries, endpointID string) ([]string, error) {
	rows, err := q.ActiveSecrets(ctx, db.ActiveSecretsParams{EndpointID: endpointID, Now: timestamptz(s.cfg.Now().UTC())})
	if err != nil {
		return nil, fmt.Errorf("loading secrets: %w", err)
	}
	secrets := make([]string, 0, len(rows))
	for _, row := range rows {
		plain, err := s.cfg.Box.Open(row.Sealed, secretAD(endpointID, row.ID))
		if err != nil {
			return nil, fmt.Errorf("opening secret %s: %w", row.ID, err)
		}
		secrets = append(secrets, string(plain))
	}
	return secrets, nil
}

func secretAD(endpointID, secretID string) []byte {
	return []byte(endpointID + "/" + secretID)
}

func (e Endpoint) ref() ObjectRef {
	return ObjectRef{ID: e.ID.String(), Type: "webhook_endpoint"}
}

func (e Endpoint) validate() error {
	u, err := url.Parse(e.URL)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return fmt.Errorf("%w: url must be an absolute http or https URL", ErrInvalid)
	case e.Owner.Livemode && u.Scheme != "https":
		return fmt.Errorf("%w: live mode endpoints must use https", ErrInvalid)
	case u.User != nil:
		return fmt.Errorf("%w: url must not contain credentials", ErrInvalid)
	case len(e.URL) > 2048:
		return fmt.Errorf("%w: url is longer than 2048 characters", ErrInvalid)
	case len(e.Description) > 500:
		return fmt.Errorf("%w: description is longer than 500 characters", ErrInvalid)
	case e.Status != Enabled && e.Status != Disabled:
		return fmt.Errorf("%w: status must be enabled or disabled", ErrInvalid)
	case len(e.EnabledEvents) == 0:
		return fmt.Errorf("%w: enabled_events must name at least one event type or \"*\"", ErrInvalid)
	case len(e.EnabledEvents) > len(Types)+1:
		return fmt.Errorf("%w: enabled_events has more entries than there are event types", ErrInvalid)
	}
	for _, t := range e.EnabledEvents {
		if t != "*" && !slices.Contains(Types, t) {
			return fmt.Errorf("%w: unknown event type %q in enabled_events", ErrInvalid, t)
		}
	}
	return nil
}

func endpointFromRow(row db.EventsEndpoint) (Endpoint, error) {
	endpointID, err := EndpointPrefix.Parse(row.ID)
	if err != nil {
		return Endpoint{}, err
	}
	merchantID, err := id.Parse(row.MerchantID)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{
		ID: endpointID, Owner: Owner{Merchant: merchantID, Livemode: row.Livemode}, URL: row.Url,
		Description: row.Description, EnabledEvents: row.EnabledEvents, Status: Status(row.Status),
		APIVersion: row.ApiVersion, CreatedAt: row.CreatedAt.Time,
	}, nil
}
