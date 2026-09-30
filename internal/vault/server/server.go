// Package server is the card vault: it stores card numbers encrypted, one data key per
// card, and hands them back in the clear only to Jupiter's API over mTLS.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/card"
	"github.com/iricardofernandes/jupiter/internal/vault/kms"
	"github.com/iricardofernandes/jupiter/internal/vault/server/db"
	"github.com/iricardofernandes/jupiter/internal/vault/server/migrations"
)

const (
	dataKeySize        = 32
	defaultCVCTTL      = 30 * time.Minute
	defaultMaxCVCs     = 100_000
	defaultClaimWindow = time.Hour
	defaultPublicRate  = 2
	defaultPublicBurst = 20
	// fingerprintSize is how much of the HMAC the rest of Jupiter sees: 128 bits keep
	// collisions out of reach and say no more than needed.
	fingerprintSize = 16
)

var ErrKeyInUse = errors.New("vault: cards are still wrapped under this key")

type Config struct {
	Pool   *pgxpool.Pool
	KMS    kms.KMS
	Now    func() time.Time
	Logger *slog.Logger
	// CVCTTL is how long a security code waits in memory for its authorization.
	CVCTTL time.Duration
	// MaxCVCs bounds how many security codes are held at once.
	MaxCVCs int
	// ClaimWindow is how long a token made from a web page waits for its merchant's
	// server to claim it before it is deleted.
	ClaimWindow time.Duration
	// PublicRate and PublicBurst limit how fast one address can make tokens on the
	// public route.
	PublicRate  float64
	PublicBurst int
}

type Service struct {
	cfg     Config
	cvcs    *cvcs
	limiter *limiter
}

func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.CVCTTL == 0 {
		cfg.CVCTTL = defaultCVCTTL
	}
	if cfg.MaxCVCs == 0 {
		cfg.MaxCVCs = defaultMaxCVCs
	}
	if cfg.ClaimWindow == 0 {
		cfg.ClaimWindow = defaultClaimWindow
	}
	if cfg.PublicRate == 0 {
		cfg.PublicRate = defaultPublicRate
	}
	if cfg.PublicBurst == 0 {
		cfg.PublicBurst = defaultPublicBurst
	}
	return &Service{
		cfg: cfg, cvcs: newCVCs(cfg.CVCTTL, cfg.MaxCVCs, cfg.Now),
		limiter: newLimiter(cfg.PublicRate, cfg.PublicBurst, cfg.Now),
	}
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "vault", migrations.FS)
}

// Tokenize stores a card for an owner, idempotently by request key.
func (s *Service) Tokenize(ctx context.Context, r vault.TokenizeRequest) (vault.Card, error) {
	if r.Owner == "" || r.RequestKey == "" {
		return vault.Card{}, fmt.Errorf("%w: owner and request_key are required", errInvalidRequest)
	}
	// A repeated request is answered from what the first one stored, before the card is
	// checked again: a card that expired since, or a KMS that fails now, must not turn a
	// retry into an error the first request did not get.
	if row, err := s.repeated(ctx, r); !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return vault.Card{}, err
		}
		s.cvcs.put(row.Token, r.Card.CVC)
		return cardOf(row), nil
	}
	row, err := s.store(ctx, r.Card, func(p *db.InsertCardParams) {
		p.Owner, p.RequestKey = text(r.Owner), text(r.RequestKey)
	})
	if err != nil {
		return vault.Card{}, err
	}
	if row.Token == "" {
		// Another request with the same key stored its card first.
		if row, err = s.repeated(ctx, r); err != nil {
			return vault.Card{}, err
		}
	}
	s.cvcs.put(row.Token, r.Card.CVC)
	return cardOf(row), nil
}

// repeated answers a tokenization whose request key is already stored: with the same
// card, the first token; with another, a conflict.
func (s *Service) repeated(ctx context.Context, r vault.TokenizeRequest) (db.VaultCard, error) {
	row, err := db.New(s.cfg.Pool).CardByRequestKey(ctx, db.CardByRequestKeyParams{Owner: text(r.Owner), RequestKey: text(r.RequestKey)})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.VaultCard{}, err
	}
	if err != nil {
		return db.VaultCard{}, fmt.Errorf("reading the first card for a repeated request key: %w", err)
	}
	number, err := card.ParseNumber(r.Card.Number)
	if err != nil {
		return db.VaultCard{}, vault.ErrConflict
	}
	fingerprint, err := s.fingerprint(ctx, number.String())
	if err != nil {
		return db.VaultCard{}, err
	}
	if !hmac.Equal(fingerprint, row.Fingerprint) || int(row.ExpMonth) != r.Card.ExpMonth || int(row.ExpYear) != r.Card.ExpYear {
		return db.VaultCard{}, vault.ErrConflict
	}
	return row, nil
}

// TokenizePublic stores a card a web page sent with a merchant's publishable key. The
// vault cannot check the key, which lives in Jupiter's database; the token stays
// unclaimed, and Jupiter checks the key when the merchant's server claims it.
func (s *Service) TokenizePublic(ctx context.Context, publishableKey string, c vault.CardData) (vault.Card, error) {
	expires := s.cfg.Now().UTC().Add(s.cfg.ClaimWindow)
	row, err := s.store(ctx, c, func(p *db.InsertCardParams) {
		p.PublishableKey = text(publishableKey)
		p.ClaimExpiresAt = pgtype.Timestamptz{Time: expires, Valid: true}
	})
	if err != nil {
		return vault.Card{}, err
	}
	s.cvcs.put(row.Token, c.CVC)
	return cardOf(row), nil
}

// store validates, encrypts and inserts a card. It returns a zero row when the owner's
// request key is already taken.
func (s *Service) store(ctx context.Context, c vault.CardData, set func(*db.InsertCardParams)) (db.VaultCard, error) {
	number, err := card.ParseNumber(c.Number)
	if err != nil {
		return db.VaultCard{}, err
	}
	if err := card.CheckExpiry(c.ExpMonth, c.ExpYear, s.cfg.Now()); err != nil {
		return db.VaultCard{}, err
	}
	if err := card.CheckCVC(number, c.CVC); err != nil {
		return db.VaultCard{}, err
	}
	token := vault.TokenPrefix.FromUUID(uuid.NewV4()).String()
	fingerprint, err := s.fingerprint(ctx, number.String())
	if err != nil {
		return db.VaultCard{}, err
	}
	keyID := s.cfg.KMS.ActiveKey()
	encrypted, wrapped, err := s.encrypt(ctx, token, keyID, number.String())
	if err != nil {
		return db.VaultCard{}, err
	}
	params := db.InsertCardParams{
		Token: token, Fingerprint: fingerprint, Brand: number.Brand(), Bin: number.BIN(), Last4: number.Last4(),
		ExpMonth: int32(c.ExpMonth), ExpYear: int32(c.ExpYear), //nolint:gosec // bounded by CheckExpiry
		EncryptedNumber: encrypted, WrappedKey: wrapped, KeyID: keyID,
		CreatedAt: pgtype.Timestamptz{Time: s.cfg.Now().UTC(), Valid: true},
	}
	set(&params)
	row, err := db.New(s.cfg.Pool).InsertCard(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.VaultCard{}, nil
	}
	if err != nil {
		return db.VaultCard{}, fmt.Errorf("storing card: %w", err)
	}
	return row, nil
}

// encrypt seals the number under a new data key and wraps the data key with the KMS.
// Both are bound to the token, so a ciphertext moved to another row does not open.
func (s *Service) encrypt(ctx context.Context, token, keyID, number string) (encrypted, wrapped []byte, err error) {
	dataKey := make([]byte, dataKeySize)
	defer clear(dataKey)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, nil, err
	}
	box, err := secretbox.New(dataKey)
	if err != nil {
		return nil, nil, err
	}
	if encrypted, err = box.Seal([]byte(number), []byte(token)); err != nil {
		return nil, nil, err
	}
	if wrapped, err = s.cfg.KMS.Wrap(ctx, keyID, dataKey, []byte(token)); err != nil {
		return nil, nil, fmt.Errorf("wrapping data key: %w", err)
	}
	return encrypted, wrapped, nil
}

func (s *Service) decrypt(ctx context.Context, row db.VaultCard) (string, error) {
	dataKey, err := s.cfg.KMS.Unwrap(ctx, row.KeyID, row.WrappedKey, []byte(row.Token))
	if err != nil {
		return "", fmt.Errorf("unwrapping the data key of %s: %w", row.Token, err)
	}
	defer clear(dataKey)
	box, err := secretbox.New(dataKey)
	if err != nil {
		return "", err
	}
	number, err := box.Open(row.EncryptedNumber, []byte(row.Token))
	if err != nil {
		return "", fmt.Errorf("decrypting %s: %w", row.Token, err)
	}
	defer clear(number)
	return string(number), nil
}

func (s *Service) fingerprint(ctx context.Context, number string) ([]byte, error) {
	mac, err := s.cfg.KMS.MAC(ctx, []byte(number))
	if err != nil {
		return nil, fmt.Errorf("fingerprinting: %w", err)
	}
	return mac, nil
}

func (s *Service) Card(ctx context.Context, token string) (vault.Card, error) {
	row, err := s.get(ctx, token)
	if err != nil {
		return vault.Card{}, err
	}
	return cardOf(row), nil
}

// Claim hands an unclaimed token to owner; claiming one owner already holds is a no-op.
func (s *Service) Claim(ctx context.Context, token, owner string) (vault.Card, error) {
	if owner == "" {
		return vault.Card{}, fmt.Errorf("%w: owner is required", errInvalidRequest)
	}
	row, err := db.New(s.cfg.Pool).ClaimCard(ctx, db.ClaimCardParams{
		Owner: text(owner), Token: token, Now: pgtype.Timestamptz{Time: s.cfg.Now().UTC(), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if row, err = s.get(ctx, token); err != nil {
			return vault.Card{}, err
		}
		if row.Owner.String != owner {
			return vault.Card{}, vault.ErrNotFound
		}
	} else if err != nil {
		return vault.Card{}, fmt.Errorf("claiming card: %w", err)
	}
	return cardOf(row), nil
}

// Detokenize returns the card to its owner, with the security code the first time.
func (s *Service) Detokenize(ctx context.Context, token, owner string) (vault.CardData, error) {
	row, err := s.get(ctx, token)
	if err != nil {
		return vault.CardData{}, err
	}
	if owner == "" || row.Owner.String != owner {
		return vault.CardData{}, vault.ErrNotFound
	}
	number, err := s.decrypt(ctx, row)
	if err != nil {
		return vault.CardData{}, err
	}
	return vault.CardData{Number: number, ExpMonth: int(row.ExpMonth), ExpYear: int(row.ExpYear), CVC: s.cvcs.take(token)}, nil
}

func (s *Service) get(ctx context.Context, token string) (db.VaultCard, error) {
	if _, err := vault.TokenPrefix.Parse(token); err != nil {
		return db.VaultCard{}, vault.ErrNotFound
	}
	row, err := db.New(s.cfg.Pool).GetCard(ctx, token)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.VaultCard{}, vault.ErrNotFound
	}
	if err != nil {
		return db.VaultCard{}, fmt.Errorf("reading card: %w", err)
	}
	return row, nil
}

// PurgeUnclaimed deletes web-page tokens whose claim window closed, and forgets
// security codes past their time.
func (s *Service) PurgeUnclaimed(ctx context.Context) (int64, error) {
	s.cvcs.sweep()
	n, err := db.New(s.cfg.Pool).PurgeUnclaimed(ctx, pgtype.Timestamptz{Time: s.cfg.Now().UTC(), Valid: true})
	if err != nil {
		return 0, fmt.Errorf("purging unclaimed cards: %w", err)
	}
	return n, nil
}

func cardOf(row db.VaultCard) vault.Card {
	c := vault.Card{
		Token: row.Token, Owner: row.Owner.String, Brand: row.Brand, BIN: row.Bin, Last4: row.Last4,
		ExpMonth: int(row.ExpMonth), ExpYear: int(row.ExpYear),
		Fingerprint: hex.EncodeToString(row.Fingerprint[:fingerprintSize]), CreatedAt: row.CreatedAt.Time,
	}
	if !row.Owner.Valid {
		c.PublishableKey = row.PublishableKey.String
	}
	return c
}

func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
