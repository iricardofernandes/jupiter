package acquirer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// TokenVault is where network tokens are kept: in the vault, beside the card.
type TokenVault interface {
	ReadCard(ctx context.Context, token, owner string) (vault.CardData, error)
	StoreNetworkToken(ctx context.Context, token, owner string, nt vault.NetworkToken) error
}

const (
	provisioningStale = 5 * time.Minute
	eventsTolerance   = 5 * time.Minute
)

// ProvisionNetworkTokens asks the network for a token for up to batch live cards that
// have none, and returns how many it provisioned. A card the network will not tokenize
// is marked failed and keeps being paid with its number.
func (c *Connector) ProvisionNetworkTokens(ctx context.Context, p *payments.Service, batch int32) (int, error) {
	if c.cfg.Tokens == nil || c.cfg.NetworkURL == "" {
		return 0, nil
	}
	claimed, err := p.ClaimForNetworkTokens(ctx, c.cfg.Pool, batch, provisioningStale)
	if err != nil {
		return 0, err
	}
	provisioned := 0
	var failures []error
	for _, candidate := range claimed {
		reference, err := c.provision(ctx, candidate.Card)
		var refused *refusalError
		switch {
		case err == nil:
			provisioned++
		case errors.As(err, &refused) && refused.status < http.StatusInternalServerError:
			c.cfg.Logger.WarnContext(ctx, "the network will not tokenize a card", "payment_method", candidate.PaymentMethod, "error", err)
		default:
			// Left provisioning: it is claimed again once stale, which spaces the retries.
			c.cfg.Logger.WarnContext(ctx, "provisioning a network token", "payment_method", candidate.PaymentMethod, "error", err)
			continue
		}
		if err := p.SetNetworkToken(ctx, c.cfg.Pool, candidate, reference, err == nil); err != nil {
			failures = append(failures, err)
		}
	}
	return provisioned, errors.Join(failures...)
}

// refusalError is an answer from the network other than 200.
type refusalError struct {
	path   string
	status int
}

func (e *refusalError) Error() string {
	return fmt.Sprintf("the network answered %s with %d", e.path, e.status)
}

func (c *Connector) provision(ctx context.Context, ref payments.CardReference) (string, error) {
	card, err := c.cfg.Tokens.ReadCard(ctx, ref.Token, ref.Owner)
	if err != nil {
		return "", err
	}
	var out struct {
		Reference string `json:"token_reference"`
		Token     string `json:"token"`
		ExpMonth  int    `json:"exp_month"`
		ExpYear   int    `json:"exp_year"`
	}
	if err := c.networkCall(ctx, "/v1/tokens", map[string]any{
		"token_requestor_id": c.cfg.AcquirerID, "pan": card.Number, "exp_month": card.ExpMonth, "exp_year": card.ExpYear,
	}, &out); err != nil {
		return "", err
	}
	nt := vault.NetworkToken{Number: out.Token, ExpMonth: out.ExpMonth, ExpYear: out.ExpYear, Reference: out.Reference}
	if err := c.cfg.Tokens.StoreNetworkToken(ctx, ref.Token, ref.Owner, nt); err != nil {
		return "", err
	}
	return out.Reference, nil
}

// cryptogram asks the network for a one-time cryptogram for a payment with a token.
func (c *Connector) cryptogram(ctx context.Context, reference string, amount int64) (string, error) {
	var out struct {
		Cryptogram string `json:"cryptogram"`
	}
	err := c.networkCall(ctx, "/v1/tokens/"+reference+"/cryptograms", map[string]any{"amount": amount}, &out)
	return out.Cryptogram, err
}

func (c *Connector) networkCall(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.NetworkURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &refusalError{path: path, status: resp.StatusCode}
	}
	return json.Unmarshal(answer, out)
}

// EventsPath is where the network sends token events, on Jupiter's public address.
const EventsPath = "/network/v1/token-events"

// Handler takes the network's token events (a card behind a token was replaced, or the
// token was suspended) and, when d is set, its dispute events.
func (c *Connector) Handler(p *payments.Service, d *disputes.Service) http.Handler {
	mux := http.NewServeMux()
	if d != nil {
		mux.HandleFunc("POST "+DisputeEventsPath, c.disputeEvents(d))
	}
	mux.HandleFunc("POST "+EventsPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		if err != nil {
			http.Error(w, "unreadable", http.StatusBadRequest)
			return
		}
		if err := cardnet.VerifyEvent(body, r.Header.Get(cardnet.EventSignatureHeader), c.cfg.EventsSecret, eventsTolerance, c.cfg.Now()); err != nil {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var e cardnet.TokenEvent
		if err := json.Unmarshal(body, &e); err != nil || !e.Valid() {
			http.Error(w, "not a token event", http.StatusBadRequest)
			return
		}
		if _, err := p.UpdateFromNetworkToken(r.Context(), c.cfg.Pool, e); err != nil {
			c.cfg.Logger.ErrorContext(r.Context(), "applying a token event", "type", e.Type, "reference", e.Reference, "error", err)
			http.Error(w, "not applied", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
