package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/vault"
)

// Vault is the card vault as the API uses it; *vault.Client in production.
type Vault interface {
	Tokenize(ctx context.Context, r vault.TokenizeRequest) (vault.Card, error)
	Card(ctx context.Context, token string) (vault.Card, error)
	Claim(ctx context.Context, token, owner string) (vault.Card, error)
}

var errVaultUnavailable = &Error{
	Status: http.StatusServiceUnavailable, Type: openapi.ApiError, Code: "vault_unavailable",
	Message: "Cards cannot be saved right now. The request can be retried with the same Idempotency-Key.",
}

func (a *API) paymentMethodOperations() []operation {
	return []operation{{
		name: "create_payment_method", scope: merchant.ScopePaymentMethodsWrite, prepare: a.tokenizeCardNumber,
		phases: []phase{{point: pointStarted, foreign: a.claimCard, atomic: a.createPaymentMethod}},
	}}
}

func (a *API) CreatePaymentMethod(w http.ResponseWriter, r *http.Request, _ openapi.CreatePaymentMethodParams) {
	a.serve(w, r, "create_payment_method", "")
}

func (a *API) GetPaymentMethod(w http.ResponseWriter, r *http.Request, methodID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopePaymentMethodsRead)
	if !ok {
		return
	}
	parsed, err := payments.PaymentMethodPrefix.Parse(methodID)
	if err != nil {
		a.writeError(w, r, notFound("payment_method", methodID))
		return
	}
	pm, err := a.deps.Payments.PaymentMethod(r.Context(), a.deps.Pool, paymentsOwner(p), parsed)
	if err != nil {
		a.fail(w, r, paymentsError(err, methodID))
		return
	}
	a.writeJSON(w, r, paymentMethodJSON(pm))
}

// tokenizeCardNumber sends a card number to the vault before anything is recorded and
// replaces it with the vault's token, so the number never reaches Jupiter's database,
// not even in the stored request of an Idempotency-Key. The vault's request key comes
// from the Idempotency-Key, so a retry gets the same token, the same rewritten body, and
// with it the first response.
func (a *API) tokenizeCardNumber(ctx context.Context, req *request, idempotencyKey string) error {
	var body openapi.CreatePaymentMethodRequest
	if err := decode(req.body, &body, false); err != nil {
		return err
	}
	c := body.Card
	switch {
	case body.Type != "card":
		return invalidRequest("parameter_invalid", "type", "type must be card.")
	case c.Token != nil && (c.Number != nil || c.ExpMonth != nil || c.ExpYear != nil || c.Cvc != nil):
		return invalidRequest("parameter_invalid", "card", "Send either card[token] or the card's number and expiry, not both.")
	case c.Token != nil:
		// Anything that is not a token, a card number sent in the wrong field included,
		// is refused before it is recorded, and never repeated back.
		if _, err := vault.TokenPrefix.Parse(*c.Token); err != nil {
			return invalidRequest("parameter_invalid", "card[token]", "card[token] must be a token from the vault, tok_….")
		}
		return nil
	case c.Number == nil || c.ExpMonth == nil || c.ExpYear == nil:
		return invalidRequest("parameter_missing", "card", "card needs number, exp_month and exp_year, or a token.")
	case a.deps.Vault == nil:
		return errVaultUnavailable
	}
	requestKey := "request/" + req.requestID
	if idempotencyKey != "" {
		requestKey = "idempotency/" + idempotencyKey
	}
	card, err := a.deps.Vault.Tokenize(ctx, vault.TokenizeRequest{
		Owner: payments.VaultOwner(paymentsOwner(req.principal)), RequestKey: requestKey,
		Card: vault.CardData{Number: *c.Number, ExpMonth: *c.ExpMonth, ExpYear: *c.ExpYear, CVC: deref(c.Cvc)},
	})
	if err != nil {
		return vaultError(err)
	}
	redacted, err := json.Marshal(openapi.CreatePaymentMethodRequest{Type: "card", Card: openapi.CardDetails{Token: &card.Token}})
	if err != nil {
		return err
	}
	clear(req.body)
	req.body = redacted
	return nil
}

// claimCard makes sure the token belongs to the caller. A token a web page made carries
// the publishable key it was made with, which must be the caller's, in the caller's
// mode; the vault then hands the token over for good.
func (a *API) claimCard(ctx context.Context, r *request) error {
	if a.deps.Vault == nil {
		return errVaultUnavailable
	}
	var body openapi.CreatePaymentMethodRequest
	if err := decode(r.body, &body, false); err != nil {
		return err
	}
	token := deref(body.Card.Token)
	if _, err := vault.TokenPrefix.Parse(token); err != nil {
		return vaultError(vault.ErrNotFound)
	}
	owner := payments.VaultOwner(paymentsOwner(r.principal))
	c, err := a.deps.Vault.Card(ctx, token)
	if err != nil {
		return vaultError(err)
	}
	if c.Owner == "" {
		mine, err := a.publishableKeyOf(ctx, r.principal, c.PublishableKey)
		if err != nil {
			return err
		}
		if !mine {
			return vaultError(vault.ErrNotFound)
		}
		if c, err = a.deps.Vault.Claim(ctx, token, owner); err != nil {
			return vaultError(err)
		}
	}
	if c.Owner != owner {
		return vaultError(vault.ErrNotFound)
	}
	r.scratch = c
	return nil
}

func (a *API) publishableKeyOf(ctx context.Context, p merchant.Principal, key string) (bool, error) {
	holder, err := a.deps.Merchants.Authenticate(ctx, a.deps.Pool, key)
	if errors.Is(err, merchant.ErrInvalidKey) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return holder.Kind == merchant.Publishable && holder.Merchant == p.Merchant && holder.Livemode == p.Livemode, nil
}

func (a *API) createPaymentMethod(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	c, ok := r.scratch.(vault.Card)
	if !ok {
		return outcome{}, errors.New("create_payment_method: the card was not read from the vault")
	}
	pm, err := a.deps.Payments.CreatePaymentMethod(ctx, tx, paymentsOwner(r.principal), c)
	if err != nil {
		return outcome{}, paymentsError(err, "")
	}
	return respond(paymentMethodJSON(pm))
}

// vaultError tells the merchant what was wrong with the card, never repeating it.
func vaultError(err error) error {
	var cardErr *vault.CardError
	switch {
	case errors.As(err, &cardErr):
		return &Error{
			Status: http.StatusPaymentRequired, Type: openapi.CardError, Code: cardErr.Code, Param: "card[" + cardErr.Param + "]",
			Message: fmt.Sprintf("The card's %s is invalid.", cardErr.Param),
		}
	case errors.Is(err, vault.ErrNotFound):
		return invalidRequest("resource_missing", "card[token]", "No such token, or it belongs to another account or mode.")
	case errors.Is(err, vault.ErrConflict):
		return errMismatch
	case errors.Is(err, vault.ErrUnavailable):
		return errors.Join(errVaultUnavailable, err)
	default:
		return err
	}
}

func paymentMethodJSON(pm payments.PaymentMethod) openapi.PaymentMethod {
	return openapi.PaymentMethod{
		Id: pm.ID.String(), Object: "payment_method", Livemode: pm.Owner.Livemode, Type: openapi.PaymentMethodType(pm.Type),
		Card: openapi.PaymentMethodCard{
			Brand: openapi.PaymentMethodCardBrand(pm.Card.Brand), Bin: pm.Card.BIN, Last4: pm.Card.Last4,
			ExpMonth: pm.Card.ExpMonth, ExpYear: pm.Card.ExpYear, Fingerprint: pm.Card.Fingerprint,
		},
		Created: pm.CreatedAt.Unix(),
	}
}
