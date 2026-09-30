package merchant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/merchant/db"
	"github.com/iricardofernandes/jupiter/internal/merchant/migrations"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

var (
	MerchantPrefix = id.MustPrefix("mch")
	KeyPrefix      = id.MustPrefix("key")
)

var (
	ErrInvalid     = errors.New("merchant: invalid request")
	ErrInvalidKey  = errors.New("merchant: invalid API key")
	ErrNotFound    = errors.New("merchant: not found")
	ErrUnknownKind = errors.New("merchant: unknown key kind")
)

type Kind string

const (
	Secret      Kind = "secret"
	Publishable Kind = "publishable"
	Restricted  Kind = "restricted"
)

type Scope string

const (
	ScopeAPIKeysRead          Scope = "api_keys:read" //nolint:gosec // a scope name, not a credential
	ScopeAPIKeysWrite         Scope = "api_keys:write"
	ScopeWebhookEndpointsRead Scope = "webhook_endpoints:read"
	ScopeWebhookEndpointWrite Scope = "webhook_endpoints:write"
	ScopeEventsRead           Scope = "events:read"
	ScopeEventsWrite          Scope = "events:write"
	ScopePaymentIntentsRead   Scope = "payment_intents:read"
	ScopePaymentIntentsWrite  Scope = "payment_intents:write"
	ScopeRefundsRead          Scope = "refunds:read"
	ScopeRefundsWrite         Scope = "refunds:write"
)

// AllScopes is what a secret key holds. A restricted key holds a subset; a publishable
// key holds none, because it is embedded in merchants' web pages.
var AllScopes = []Scope{
	ScopeAPIKeysRead, ScopeAPIKeysWrite,
	ScopeWebhookEndpointsRead, ScopeWebhookEndpointWrite,
	ScopeEventsRead, ScopeEventsWrite,
	ScopePaymentIntentsRead, ScopePaymentIntentsWrite,
	ScopeRefundsRead, ScopeRefundsWrite,
}

type Merchant struct {
	ID         id.ID
	Name       string
	APIVersion string
	CreatedAt  time.Time
}

type Key struct {
	ID        id.ID
	Merchant  id.ID
	Livemode  bool
	Kind      Kind
	Name      string
	Last4     string
	Scopes    []Scope
	CreatedAt time.Time
	RevokedAt time.Time
}

// IssuedKey carries the key's secret value, which exists only in this response.
type IssuedKey struct {
	Key
	Value string
}

type Principal struct {
	Merchant   id.ID
	Key        id.ID
	Livemode   bool
	Kind       Kind
	Scopes     []Scope
	APIVersion string
}

// Can reports whether the key may act with scope. A secret key may do everything,
// including what later versions add, so its stored scopes never go stale.
func (p Principal) Can(scope Scope) bool {
	return p.Kind == Secret || slices.Contains(p.Scopes, scope)
}

type Service struct {
	now func() time.Time
}

func New(now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{now: now}
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "merchant", migrations.FS)
}

// Create registers a merchant pinned to apiVersion and issues its secret and
// publishable keys for both modes.
func (s *Service) Create(ctx context.Context, tx pgx.Tx, name, apiVersion string) (Merchant, []IssuedKey, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 || apiVersion == "" {
		return Merchant{}, nil, fmt.Errorf("%w: a merchant needs a name of up to 200 characters and an API version", ErrInvalid)
	}
	m := Merchant{ID: MerchantPrefix.New(), Name: name, APIVersion: apiVersion, CreatedAt: s.now().UTC()}
	q := db.New(tx)
	if err := q.InsertMerchant(ctx, db.InsertMerchantParams{
		ID: m.ID.String(), Name: m.Name, ApiVersion: m.APIVersion, CreatedAt: timestamptz(m.CreatedAt),
	}); err != nil {
		return Merchant{}, nil, fmt.Errorf("creating merchant: %w", err)
	}
	var keys []IssuedKey
	for _, livemode := range []bool{false, true} {
		for _, kind := range []Kind{Secret, Publishable} {
			k, err := s.issue(ctx, q, m.ID, livemode, kind, "", nil)
			if err != nil {
				return Merchant{}, nil, err
			}
			keys = append(keys, k)
		}
	}
	return m, keys, nil
}

func (s *Service) Get(ctx context.Context, q db.DBTX, merchantID id.ID) (Merchant, error) {
	row, err := db.New(q).GetMerchant(ctx, merchantID.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return Merchant{}, fmt.Errorf("%w: %s", ErrNotFound, merchantID)
	}
	if err != nil {
		return Merchant{}, err
	}
	return Merchant{ID: merchantID, Name: row.Name, APIVersion: row.ApiVersion, CreatedAt: row.CreatedAt.Time}, nil
}

// CreateRestrictedKey issues a key limited to scopes, in the principal's mode.
func (s *Service) CreateRestrictedKey(ctx context.Context, tx pgx.Tx, p Principal, name string, scopes []Scope) (IssuedKey, error) {
	if len(scopes) == 0 || len(scopes) > len(AllScopes) {
		return IssuedKey{}, fmt.Errorf("%w: a restricted key needs between 1 and %d scopes", ErrInvalid, len(AllScopes))
	}
	if len(name) > maxKeyName {
		return IssuedKey{}, fmt.Errorf("%w: name is longer than %d characters", ErrInvalid, maxKeyName)
	}
	for _, scope := range scopes {
		if !slices.Contains(AllScopes, scope) {
			return IssuedKey{}, fmt.Errorf("%w: unknown scope %q", ErrInvalid, scope)
		}
		// A key cannot hand out more than it holds.
		if !p.Can(scope) {
			return IssuedKey{}, fmt.Errorf("%w: scope %q is not held by the calling key", ErrInvalid, scope)
		}
	}
	return s.issue(ctx, db.New(tx), p.Merchant, p.Livemode, Restricted, name, slices.Compact(slices.Sorted(slices.Values(scopes))))
}

func (s *Service) issue(ctx context.Context, q *db.Queries, merchantID id.ID, livemode bool, kind Kind, name string, scopes []Scope) (IssuedKey, error) {
	value, err := newKeyValue(kind, livemode)
	if err != nil {
		return IssuedKey{}, err
	}
	if kind == Secret {
		scopes = AllScopes
	}
	if scopes == nil {
		scopes = []Scope{}
	}
	k := IssuedKey{
		Key: Key{
			ID: KeyPrefix.New(), Merchant: merchantID, Livemode: livemode, Kind: kind,
			Name: name, Last4: value[len(value)-4:], Scopes: scopes, CreatedAt: s.now().UTC(),
		},
		Value: value,
	}
	hash := sha256.Sum256([]byte(value))
	err = q.InsertKey(ctx, db.InsertKeyParams{
		ID: k.ID.String(), MerchantID: merchantID.String(), Livemode: livemode, Kind: string(kind),
		Name: name, Hash: hash[:], Last4: k.Last4, Scopes: scopeStrings(scopes), CreatedAt: timestamptz(k.CreatedAt),
	})
	if err != nil {
		return IssuedKey{}, fmt.Errorf("issuing key: %w", err)
	}
	return k, nil
}

func (s *Service) Authenticate(ctx context.Context, q db.DBTX, value string) (Principal, error) {
	if _, _, err := parseKeyValue(value); err != nil {
		return Principal{}, err
	}
	hash := sha256.Sum256([]byte(value))
	row, err := db.New(q).AuthenticateKey(ctx, hash[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidKey
	}
	if err != nil {
		return Principal{}, fmt.Errorf("authenticating: %w", err)
	}
	merchantID, err := MerchantPrefix.Parse(row.MerchantID)
	if err != nil {
		return Principal{}, err
	}
	keyID, err := KeyPrefix.Parse(row.ID)
	if err != nil {
		return Principal{}, err
	}
	return Principal{
		Merchant: merchantID, Key: keyID, Livemode: row.Livemode, Kind: Kind(row.Kind),
		Scopes: toScopes(row.Scopes), APIVersion: row.ApiVersion,
	}, nil
}

func (s *Service) ListKeys(ctx context.Context, q db.DBTX, p Principal, r page.Request) ([]Key, bool, error) {
	rows, err := db.New(q).ListKeys(ctx, db.ListKeysParams{
		MerchantID: p.Merchant.String(), Livemode: p.Livemode,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing keys: %w", err)
	}
	keys := make([]Key, 0, len(rows))
	for _, row := range rows {
		k, err := keyFromRow(db.MerchantApiKey{
			ID: row.ID, MerchantID: row.MerchantID, Livemode: row.Livemode, Kind: row.Kind, Name: row.Name,
			Last4: row.Last4, Scopes: row.Scopes, CreatedAt: row.CreatedAt, RevokedAt: row.RevokedAt,
		})
		if err != nil {
			return nil, false, err
		}
		keys = append(keys, k)
	}
	items, more := page.Trim(keys, r)
	return items, more, nil
}

func (s *Service) GetKey(ctx context.Context, q db.DBTX, p Principal, keyID id.ID) (Key, error) {
	row, err := db.New(q).GetKey(ctx, db.GetKeyParams{ID: keyID.String(), MerchantID: p.Merchant.String(), Livemode: p.Livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, fmt.Errorf("%w: %s", ErrNotFound, keyID)
	}
	if err != nil {
		return Key{}, err
	}
	return keyFromRow(db.MerchantApiKey{
		ID: row.ID, MerchantID: row.MerchantID, Livemode: row.Livemode, Kind: row.Kind, Name: row.Name,
		Last4: row.Last4, Scopes: row.Scopes, CreatedAt: row.CreatedAt, RevokedAt: row.RevokedAt,
	})
}

// RevokeKey is idempotent: revoking a revoked key returns it unchanged, with changed
// false, so concurrent revocations report the change exactly once.
func (s *Service) RevokeKey(ctx context.Context, tx pgx.Tx, p Principal, keyID id.ID) (k Key, changed bool, err error) {
	row, err := db.New(tx).RevokeKey(ctx, db.RevokeKeyParams{
		Now: timestamptz(s.now().UTC()), ID: keyID.String(), MerchantID: p.Merchant.String(), Livemode: p.Livemode,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		k, err = s.GetKey(ctx, tx, p, keyID)
		return k, false, err
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("revoking key: %w", err)
	}
	k, err = keyFromRow(db.MerchantApiKey{
		ID: row.ID, MerchantID: row.MerchantID, Livemode: row.Livemode, Kind: row.Kind, Name: row.Name,
		Last4: row.Last4, Scopes: row.Scopes, CreatedAt: row.CreatedAt, RevokedAt: row.RevokedAt,
	})
	return k, true, err
}

func keyFromRow(row db.MerchantApiKey) (Key, error) {
	keyID, err := KeyPrefix.Parse(row.ID)
	if err != nil {
		return Key{}, err
	}
	merchantID, err := MerchantPrefix.Parse(row.MerchantID)
	if err != nil {
		return Key{}, err
	}
	return Key{
		ID: keyID, Merchant: merchantID, Livemode: row.Livemode, Kind: Kind(row.Kind), Name: row.Name,
		Last4: row.Last4, Scopes: toScopes(row.Scopes), CreatedAt: row.CreatedAt.Time, RevokedAt: row.RevokedAt.Time,
	}, nil
}

const maxKeyName = 100

var kindPrefixes = map[Kind]string{Secret: "sk", Publishable: "pk", Restricted: "rk"}

const (
	keyEntropyBytes = 32
	base62          = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// newKeyValue returns a key such as "sk_test_<43 base62 characters>".
func newKeyValue(kind Kind, livemode bool) (string, error) {
	prefix, ok := kindPrefixes[kind]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	mode := "test"
	if livemode {
		mode = "live"
	}
	raw := make([]byte, keyEntropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating key: %w", err)
	}
	n := new(big.Int).SetBytes(raw)
	var body []byte
	radix := big.NewInt(int64(len(base62)))
	for n.Sign() > 0 {
		var digit big.Int
		n.QuoRem(n, radix, &digit)
		body = append(body, base62[digit.Int64()])
	}
	for len(body) < 43 {
		body = append(body, '0')
	}
	slices.Reverse(body)
	return prefix + "_" + mode + "_" + string(body), nil
}

func parseKeyValue(value string) (Kind, bool, error) {
	parts := strings.SplitN(value, "_", 3)
	if len(parts) != 3 || len(parts[2]) != 43 {
		return "", false, ErrInvalidKey
	}
	for kind, prefix := range kindPrefixes {
		if parts[0] != prefix {
			continue
		}
		switch parts[1] {
		case "test":
			return kind, false, nil
		case "live":
			return kind, true, nil
		}
	}
	return "", false, ErrInvalidKey
}

func scopeStrings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

func toScopes(raw []string) []Scope {
	out := make([]Scope, len(raw))
	for i, s := range raw {
		out[i] = Scope(s)
	}
	return out
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
