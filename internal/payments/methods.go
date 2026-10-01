package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

var PaymentMethodPrefix = id.MustPrefix("pm")

type Card struct {
	Brand    string
	BIN      string
	Last4    string
	ExpMonth int
	ExpYear  int
	// Fingerprint is the same for the same card number across this merchant's payment
	// methods, and different at every other merchant, so merchants cannot match their
	// customers' cards with one another.
	Fingerprint string
}

type PaymentMethod struct {
	ID        id.ID
	Owner     Owner
	Type      string
	Card      Card
	CreatedAt time.Time
}

// VaultOwner is how the vault names an owner: tokens made in test mode are not usable
// in live mode, nor the other way around.
func VaultOwner(o Owner) string {
	mode := "test"
	if o.Livemode {
		mode = "live"
	}
	return o.Merchant.String() + "/" + mode
}

// CreatePaymentMethod saves a card the vault holds for owner. Saving the same token
// again returns the payment method it already backs.
func (s *Service) CreatePaymentMethod(ctx context.Context, tx pgx.Tx, owner Owner, c vault.Card) (PaymentMethod, error) {
	if c.Owner != VaultOwner(owner) {
		return PaymentMethod{}, fmt.Errorf("%w: the card belongs to another owner", ErrInvalid)
	}
	q := db.New(tx)
	row, err := q.InsertPaymentMethod(ctx, db.InsertPaymentMethodParams{
		ID: PaymentMethodPrefix.New().String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		VaultToken: c.Token, Brand: c.Brand, Bin: c.BIN, Last4: c.Last4,
		ExpMonth: int32(c.ExpMonth), ExpYear: int32(c.ExpYear), //nolint:gosec // the vault bounds both
		VaultFingerprint: c.Fingerprint, CreatedAt: ts(s.cfg.Now().UTC()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = q.GetPaymentMethodByToken(ctx, c.Token)
	}
	if err != nil {
		return PaymentMethod{}, fmt.Errorf("saving payment method: %w", err)
	}
	return methodFromRow(row)
}

func (s *Service) PaymentMethod(ctx context.Context, q db.DBTX, owner Owner, methodID id.ID) (PaymentMethod, error) {
	row, err := s.methodRow(ctx, q, owner, methodID.String())
	if err != nil {
		return PaymentMethod{}, err
	}
	return methodFromRow(row)
}

func (s *Service) methodRow(ctx context.Context, q db.DBTX, owner Owner, methodID string) (db.PaymentsPaymentMethod, error) {
	m, _ := ctx.Value(memoKey{}).(*memo)
	key := owner.Merchant.String() + "/" + strconv.FormatBool(owner.Livemode) + "/" + methodID
	if row, ok := m.method(key); ok {
		return row, nil
	}
	row, err := db.New(q).GetPaymentMethod(ctx, db.GetPaymentMethodParams{
		ID: methodID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.PaymentsPaymentMethod{}, fmt.Errorf("%w: %s", ErrNotFound, methodID)
	}
	if err == nil {
		m.remember(key, row)
	}
	return row, err
}

type memoKey struct{}

// memo keeps the payment methods one request reads: its phases would read the same one
// again and again.
type memo struct {
	mu      sync.Mutex
	methods map[string]db.PaymentsPaymentMethod
}

// WithMemo is a context for one request, in which each payment method is read once.
func WithMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, memoKey{}, &memo{methods: map[string]db.PaymentsPaymentMethod{}})
}

func (m *memo) method(key string) (db.PaymentsPaymentMethod, bool) {
	if m == nil {
		return db.PaymentsPaymentMethod{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.methods[key]
	return row, ok
}

// forget drops what the request read of payment methods, after it changed one.
func forget(ctx context.Context) {
	if m, _ := ctx.Value(memoKey{}).(*memo); m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		clear(m.methods)
	}
}

func (m *memo) remember(key string, row db.PaymentsPaymentMethod) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.methods[key] = row
}

// checkPaymentMethod accepts a saved payment method of the owner's, or in test mode a
// test one.
func (s *Service) checkPaymentMethod(ctx context.Context, q db.DBTX, owner Owner, pm string) error {
	if knownPaymentMethod(pm) && owner.Livemode {
		return fmt.Errorf("%w: %s is a test payment method, for test mode only", ErrInvalid, pm)
	}
	if pm == "" || knownPaymentMethod(pm) || pm == PaymentMethodPix || pm == PaymentMethodBoleto {
		return nil
	}
	if _, err := s.methodRow(ctx, q, owner, pm); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: unknown payment_method %q", ErrInvalid, pm)
		}
		return err
	}
	return nil
}

// cardFor names the vault token behind a saved payment method, for the rail; a test
// payment method has none.
func (s *Service) cardFor(ctx context.Context, q db.DBTX, owner Owner, pm string) (*CardReference, error) {
	if knownPaymentMethod(pm) {
		return nil, nil //nolint:nilnil // a test payment method has no card behind it
	}
	row, err := s.methodRow(ctx, q, owner, pm)
	if err != nil {
		return nil, err
	}
	return &CardReference{Token: row.VaultToken, Owner: VaultOwner(owner), NetworkToken: row.NetworkTokenStatus == "active"}, nil
}

// TokenizationCandidate is a live card waiting for a network token, claimed at ClaimedAt.
type TokenizationCandidate struct {
	PaymentMethod string
	Card          CardReference
	ClaimedAt     time.Time
}

// ClaimForNetworkTokens takes up to n live cards to provision network tokens for, and
// those whose provisioning stopped more than stale ago.
func (s *Service) ClaimForNetworkTokens(ctx context.Context, q db.DBTX, n int32, stale time.Duration) ([]TokenizationCandidate, error) {
	now := s.cfg.Now().UTC().Truncate(time.Microsecond)
	rows, err := db.New(q).ClaimPaymentMethodsToTokenize(ctx, db.ClaimPaymentMethodsToTokenizeParams{
		Now: ts(now), StaleBefore: ts(now.Add(-stale)), MaxCount: n,
	})
	if err != nil {
		return nil, fmt.Errorf("claiming cards to tokenize: %w", err)
	}
	out := make([]TokenizationCandidate, 0, len(rows))
	for _, r := range rows {
		merchant, err := id.Parse(r.MerchantID)
		if err != nil {
			return nil, err
		}
		out = append(out, TokenizationCandidate{
			PaymentMethod: r.ID, Card: CardReference{Token: r.VaultToken, Owner: VaultOwner(Owner{Merchant: merchant, Livemode: r.Livemode})},
			ClaimedAt: now,
		})
	}
	return out, nil
}

// SetNetworkToken records how provisioning a card's network token ended. Only the claim
// that is still current records it: one that outlived provisioningStale and was claimed
// again leaves it to the newer.
func (s *Service) SetNetworkToken(ctx context.Context, q db.DBTX, c TokenizationCandidate, reference string, active bool) error {
	status := "failed"
	if active {
		status = "active"
	}
	return db.New(q).SetNetworkToken(ctx, db.SetNetworkTokenParams{
		ID: c.PaymentMethod, NetworkTokenReference: reference, NetworkTokenStatus: status, Now: ts(s.cfg.Now().UTC()),
		ClaimedAt: ts(c.ClaimedAt),
	})
}

// UpdateFromNetworkToken applies what the card network says of a token: the card behind
// it was replaced (a new last four and expiry), or the token was suspended. An event
// older than the last one applied is ignored.
func (s *Service) UpdateFromNetworkToken(ctx context.Context, q db.DBTX, e cardnet.TokenEvent) (bool, error) {
	if !e.Valid() {
		return false, fmt.Errorf("%w: not a token event", ErrInvalid)
	}
	n, err := db.New(q).UpdateCardFromNetworkToken(ctx, db.UpdateCardFromNetworkTokenParams{
		NetworkTokenReference: e.Reference, Last4: e.Last4, ExpMonth: int32(e.ExpMonth), ExpYear: int32(e.ExpYear), //nolint:gosec // an expiry
		NetworkTokenStatus: e.Status, Now: ts(s.cfg.Now().UTC()), OccurredAt: ts(e.OccurredAt),
	})
	return n > 0, err
}

// checkStoredCredential enforces the stored credential rules: a card is stored for
// merchant-initiated payments by a customer-initiated one that says so, and a
// merchant-initiated payment is made only on a card stored that way.
func (s *Service) checkStoredCredential(ctx context.Context, q db.DBTX, owner Owner, row db.PaymentsIntent, offSession bool) error {
	stores := row.SetupFutureUsage == SetupOffSession
	switch {
	case !offSession && !stores:
		return nil
	case offSession && stores:
		return fmt.Errorf("%w: a payment cannot both store the card (setup_future_usage) and be made off_session", ErrInvalid)
	case knownPaymentMethod(row.PaymentMethod):
		return fmt.Errorf("%w: setup_future_usage and off_session need a saved payment method", ErrInvalid)
	}
	if !offSession {
		return nil
	}
	first, err := s.firstTransaction(ctx, q, owner, row.PaymentMethod)
	if err != nil {
		return err
	}
	if first == "" {
		return fmt.Errorf("%w: %s has not been set up for off-session payments: confirm a payment with it and setup_future_usage=off_session first", ErrInvalid, row.PaymentMethod)
	}
	return nil
}

// firstTransaction is the network transaction id of the payment that stored the card.
func (s *Service) firstTransaction(ctx context.Context, q db.DBTX, owner Owner, pm string) (string, error) {
	row, err := s.methodRow(ctx, q, owner, pm)
	if err != nil {
		return "", err
	}
	return row.NetworkTransactionID, nil
}

// schemeOf names the card scheme whose rules apply to a payment method.
func (s *Service) schemeOf(ctx context.Context, q db.DBTX, owner Owner, pm string) (string, error) {
	switch pm {
	case TestCardMastercard:
		return "mastercard", nil
	case TestCardVisa, TestCardDeclined, TestCardInsufficientFunds, TestCardAuthenticationRequired:
		return "visa", nil
	}
	row, err := s.methodRow(ctx, q, owner, pm)
	if err != nil {
		return "", err
	}
	return row.Brand, nil
}

func methodFromRow(row db.PaymentsPaymentMethod) (PaymentMethod, error) {
	methodID, err := PaymentMethodPrefix.Parse(row.ID)
	if err != nil {
		return PaymentMethod{}, err
	}
	merchantID, err := id.Parse(row.MerchantID)
	if err != nil {
		return PaymentMethod{}, err
	}
	return PaymentMethod{
		ID: methodID, Owner: Owner{Merchant: merchantID, Livemode: row.Livemode}, Type: row.Type,
		Card: Card{
			Brand: row.Brand, BIN: row.Bin, Last4: row.Last4, ExpMonth: int(row.ExpMonth), ExpYear: int(row.ExpYear),
			Fingerprint: merchantFingerprint(row.VaultFingerprint, row.MerchantID),
		},
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// merchantFingerprint derives a merchant's view of the vault's fingerprint. The vault's
// fingerprint is the key, so only someone who has it, which no merchant does, can link
// two merchants' fingerprints.
func merchantFingerprint(vaultFingerprint, merchantID string) string {
	m := hmac.New(sha256.New, []byte(vaultFingerprint))
	m.Write([]byte(merchantID))
	return hex.EncodeToString(m.Sum(nil))[:16]
}
