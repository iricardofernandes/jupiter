package acquirer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/acquirer/db"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

const (
	kindAuthorize = "authorize"
	kindCapture   = "capture"
	kindVoid      = "void"
	kindRefund    = "refund"

	stateSending      = "sending"
	stateApproved     = "approved"
	stateDeclined     = "declined"
	stateReversing    = "reversing"
	stateReversed     = "reversed"
	stateAdvising     = "advising"
	stateAcknowledged = "acknowledged"

	declineTimeout       = "issuer_timeout"
	declineNotConnected  = "network_unavailable"
	declinePartial       = "partial_approval_unsupported"
	declineNoAuthorizing = "authorization_not_found"
)

var _ payments.Rail = (*Connector)(nil)

// localTime is where Jupiter's merchants are, for DE 12 and 13.
var localTime = mustLocation("America/Sao_Paulo")

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone("BRT", -3*60*60)
	}
	return loc
}

// begin records an exchange before its message is sent, drawing its STAN and RRN. It
// returns the one already recorded under the key, with fresh false, when there is one.
func (c *Connector) begin(ctx context.Context, p db.InsertExchangeParams) (db.AcquirerExchange, bool, error) {
	var ex db.AcquirerExchange
	fresh := false
	err := postgres.InTx(ctx, c.cfg.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		stan, err := q.NextSTAN(ctx)
		if err != nil {
			return err
		}
		rrn, err := q.NextRRN(ctx)
		if err != nil {
			return err
		}
		now := c.cfg.Now()
		p.Stan, p.Rrn, p.TransmittedAt, p.Now = stan, rrnPrefix(now)+rrn, cardnet.TransmissionTime(now), timestamptz(now)
		ex, err = q.InsertExchange(ctx, p)
		fresh = err == nil
		if errors.Is(err, pgx.ErrNoRows) {
			ex, err = q.GetExchange(ctx, p.Key)
		}
		return err
	})
	return ex, fresh, err
}

// rrnPrefix is the RRN's first four characters: the last digit of the year and the day
// of the year, as retrieval reference numbers commonly begin.
func rrnPrefix(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%d%03d", t.Year()%10, t.YearDay())
}

func (c *Connector) base(ex db.AcquirerExchange) cardnet.Message {
	return cardnet.Message{
		MTI: ex.Mti, Amount: ex.Amount, TransmissionDateTime: ex.TransmittedAt, STAN: ex.Stan, RRN: ex.Rrn,
		AcquirerID: c.cfg.AcquirerID, TerminalID: c.cfg.TerminalID, MerchantID: ex.MerchantCode, Currency: cardnet.CurrencyBRL,
	}
}

// original is DE 90 naming ex's own message.
func (c *Connector) original(ex db.AcquirerExchange) string {
	return cardnet.OriginalData{
		MTI: ex.Mti, STAN: ex.Stan, TransmissionDateTime: ex.TransmittedAt, AcquirerID: c.cfg.AcquirerID,
	}.Encode()
}

func (c *Connector) Authorize(ctx context.Context, r payments.AuthorizeRequest) payments.Result {
	switch {
	case r.Card == nil:
		return declined("processing_error")
	case r.Amount.Currency() != money.BRL:
		return declined("currency_not_supported")
	}
	params := db.InsertExchangeParams{
		Key: r.Key, Kind: kindAuthorize, Mti: cardnet.AuthorizationRequest, Amount: r.Amount.Minor(),
		MerchantCode: merchantCode(r.Merchant),
	}
	if r.Installments != nil {
		params.Installments = pgtype.Int4{Int32: int32(r.Installments.Count), Valid: true} //nolint:gosec // at most 12
	}
	ex, fresh, err := c.begin(ctx, params)
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "recording an authorization", "key", r.Key, "error", err)
		return payments.Result{Outcome: payments.Unknown}
	}
	if !fresh {
		return c.answer(ctx, ex)
	}
	card, err := c.cfg.Cards.Detokenize(ctx, r.Card.Token, r.Card.Owner)
	if err != nil {
		return c.unsent(ctx, ex, err)
	}
	m := c.authorization(ex, r, card)
	resp, err := c.send(m)
	return c.finishRequest(ctx, ex, resp, err)
}

// unsent ends an exchange whose message was never sent: a card the vault no longer has
// is declined; a vault that did not answer leaves nothing behind, so the rail's query
// finds nothing and the payment is sent again.
func (c *Connector) unsent(ctx context.Context, ex db.AcquirerExchange, cause error) payments.Result {
	if errors.Is(cause, vault.ErrNotFound) {
		return c.decline(ctx, ex, "", "invalid_account")
	}
	c.cfg.Logger.WarnContext(ctx, "reading a card for an authorization", "key", ex.Key, "error", cause)
	if err := db.New(c.cfg.Pool).DeleteExchange(ctx, ex.Key); err != nil {
		c.cfg.Logger.ErrorContext(ctx, "forgetting an unsent authorization", "key", ex.Key, "error", err)
	}
	return payments.Result{Outcome: payments.Unknown}
}

func (c *Connector) authorization(ex db.AcquirerExchange, r payments.AuthorizeRequest, card vault.CardData) cardnet.Message {
	m := c.base(ex)
	local := c.cfg.Now().In(localTime)
	m.PAN, m.ProcessingCode, m.Expiry, m.MCC = card.Number, cardnet.ProcessingPurchase, expiry(card), defaultMCC
	m.LocalTime, m.LocalDate = local.Format("150405"), local.Format("0102")
	m.EntryMode = cardnet.EntryECommerce
	private := &cardnet.PrivateData{CVC: card.CVC}
	switch {
	case r.MerchantInitiated:
		m.EntryMode = cardnet.EntryCredentialOnFile
		private.StoredCredential, private.NetworkTransactionID = cardnet.StoredCredentialMerchant, r.FirstTransaction
	case r.StoresCredential:
		private.StoredCredential = cardnet.StoredCredentialInitial
	}
	if r.Installments != nil {
		m.Installments = fmt.Sprintf("%02d", r.Installments.Count)
		private.InstallmentFinancing = "M"
		if r.Installments.FinancedBy == payments.FinancedByIssuer {
			private.InstallmentFinancing = "I"
		}
	}
	m.Private = private
	return m
}

func expiry(card vault.CardData) string {
	return fmt.Sprintf("%02d%02d", card.ExpYear%100, card.ExpMonth)
}

// finishRequest applies the answer to a request (0100, 0200). A request that may have
// reached the network unanswered is reversed: Jupiter never leaves an authorization it
// cannot account for.
func (c *Connector) finishRequest(ctx context.Context, ex db.AcquirerExchange, resp cardnet.Message, err error) payments.Result {
	switch {
	case errors.Is(err, errNotSent):
		return c.decline(ctx, ex, "", declineNotConnected)
	case err != nil:
		return c.abandon(ctx, ex, declineTimeout)
	case resp.ResponseCode == cardnet.Approved && resp.Amount != ex.Amount:
		// Approved for another amount than asked, which only a partial approval may be.
		return c.abandon(ctx, ex, declinePartial)
	case resp.ResponseCode == cardnet.Approved:
		result := payments.Result{}
		err = c.update(ctx, ex.Key, func(e *db.AcquirerExchange) bool {
			if e.State != stateSending {
				result = c.result(*e)
				return false
			}
			e.State, e.ResponseCode, e.AuthorizationCode = stateApproved, resp.ResponseCode, resp.AuthorizationCode
			e.NetworkTransactionID = resp.NetworkTransactionID()
			result = c.result(*e)
			return true
		})
		if err != nil {
			c.cfg.Logger.ErrorContext(ctx, "recording an approval", "key", ex.Key, "error", err)
			return payments.Result{Outcome: payments.Unknown}
		}
		return result
	case resp.ResponseCode == cardnet.PartiallyApproved:
		// Jupiter never asks for partial approval; an issuer that grants one anyway holds
		// money nobody will capture, so it is released.
		return c.abandon(ctx, ex, declinePartial)
	default:
		return c.decline(ctx, ex, resp.ResponseCode, declineCode(resp.ResponseCode))
	}
}

func (c *Connector) decline(ctx context.Context, ex db.AcquirerExchange, rc, code string) payments.Result {
	result := declined(code)
	err := c.update(ctx, ex.Key, func(e *db.AcquirerExchange) bool {
		if e.State != stateSending {
			result = c.result(*e)
			return false
		}
		e.State, e.ResponseCode, e.DeclineCode = stateDeclined, rc, code
		return true
	})
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "recording a decline", "key", ex.Key, "error", err)
		return payments.Result{Outcome: payments.Unknown}
	}
	return result
}

// abandon gives up on a request and reverses it, now and until the network acknowledges.
func (c *Connector) abandon(ctx context.Context, ex db.AcquirerExchange, code string) payments.Result {
	err := c.update(ctx, ex.Key, func(e *db.AcquirerExchange) bool {
		if e.State != stateSending {
			return false
		}
		e.State, e.DeclineCode = stateReversing, code
		e.NextForwardAt = timestamptz(c.cfg.Now())
		return true
	})
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "recording a reversal", "key", ex.Key, "error", err)
		return payments.Result{Outcome: payments.Unknown}
	}
	c.forwardNow(ctx, ex.Key)
	return declined(code)
}

func (c *Connector) Capture(ctx context.Context, r payments.OperationRequest) payments.Result {
	auth, ok := c.approvedAuthorization(ctx, r.AuthorizationKey)
	if !ok {
		return c.repeatedOr(ctx, r.Key, declined(declineNoAuthorizing))
	}
	if r.Amount.Minor() > auth.Amount {
		return declined("invalid_amount")
	}
	ex, fresh, err := c.begin(ctx, db.InsertExchangeParams{
		Key: r.Key, Kind: kindCapture, AuthorizationKey: text(auth.Key), Mti: cardnet.CompletionAdvice,
		Amount: r.Amount.Minor(), Installments: auth.Installments, MerchantCode: auth.MerchantCode,
	})
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "recording a capture", "key", r.Key, "error", err)
		return payments.Result{Outcome: payments.Unknown}
	}
	if !fresh {
		return c.answer(ctx, ex)
	}
	resp, err := c.send(c.completion(ex, auth, cardnet.CompletionAdvice))
	if err != nil {
		// An advice must arrive: it is repeated until the network acknowledges it.
		return c.forwardLater(ctx, ex, stateAdvising)
	}
	return c.acknowledged(ctx, ex, resp)
}

func (c *Connector) completion(ex, auth db.AcquirerExchange, mti string) cardnet.Message {
	m := c.base(ex)
	m.MTI, m.OriginalData = mti, c.original(auth)
	m.Private = &cardnet.PrivateData{NetworkTransactionID: auth.NetworkTransactionID}
	if auth.Installments.Valid {
		m.Installments = fmt.Sprintf("%02d", auth.Installments.Int32)
	}
	return m
}

func (c *Connector) acknowledged(ctx context.Context, ex db.AcquirerExchange, resp cardnet.Message) payments.Result {
	result := payments.Result{}
	err := c.update(ctx, ex.Key, func(e *db.AcquirerExchange) bool {
		if e.State != stateSending && e.State != stateAdvising {
			result = c.result(*e)
			return false
		}
		e.State, e.ResponseCode = stateAcknowledged, resp.ResponseCode
		if resp.ResponseCode != cardnet.Approved {
			e.State, e.DeclineCode = stateDeclined, declineCode(resp.ResponseCode)
		}
		stopForwarding(e)
		result = c.result(*e)
		return true
	})
	if err != nil {
		return payments.Result{Outcome: payments.Unknown}
	}
	return result
}

func (c *Connector) Void(ctx context.Context, r payments.OperationRequest) payments.Result {
	auth, err := db.New(c.cfg.Pool).GetExchange(ctx, r.AuthorizationKey)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return c.voidUnsent(ctx, r)
	case err != nil:
		return payments.Result{Outcome: payments.Unknown}
	case auth.State == stateDeclined, auth.State == stateReversing, auth.State == stateReversed:
		return payments.Result{Outcome: payments.Approved, Reference: auth.Rrn}
	case auth.State == stateSending:
		c.abandon(ctx, auth, declineTimeout)
		return payments.Result{Outcome: payments.Approved, Reference: auth.Rrn}
	}
	ex, fresh, err := c.begin(ctx, db.InsertExchangeParams{
		Key: r.Key, Kind: kindVoid, AuthorizationKey: text(auth.Key), Mti: cardnet.ReversalRequest,
		Amount: auth.Amount, MerchantCode: auth.MerchantCode,
	})
	if err != nil {
		return payments.Result{Outcome: payments.Unknown}
	}
	if !fresh {
		return c.answer(ctx, ex)
	}
	m := c.base(ex)
	m.OriginalData = c.original(auth)
	m.Private = &cardnet.PrivateData{NetworkTransactionID: auth.NetworkTransactionID}
	resp, err := c.send(m)
	switch {
	case err != nil:
		// The reversal request becomes a reversal advice, repeated until acknowledged.
		return c.forwardLater(ctx, ex, stateReversing)
	case resp.ResponseCode == cardnet.Approved:
		result := c.settle(ctx, ex, stateReversed, resp.ResponseCode, "")
		c.authorizationReversed(ctx, auth.Key)
		return result
	default:
		return c.settle(ctx, ex, stateDeclined, resp.ResponseCode, declineCode(resp.ResponseCode))
	}
}

// authorizationReversed records that a void released an authorization, so nothing is
// captured or refunded against it afterwards.
func (c *Connector) authorizationReversed(ctx context.Context, key string) {
	err := c.update(ctx, key, func(e *db.AcquirerExchange) bool {
		if e.State != stateApproved {
			return false
		}
		e.State = stateReversed
		return true
	})
	if err != nil {
		c.cfg.Logger.ErrorContext(ctx, "recording a voided authorization", "key", key, "error", err)
	}
}

// voidUnsent voids an authorization before it was ever sent: it is recorded as
// reversed, so a request arriving later with its key is never sent.
func (c *Connector) voidUnsent(ctx context.Context, r payments.OperationRequest) payments.Result {
	ex, fresh, err := c.begin(ctx, db.InsertExchangeParams{
		Key: r.AuthorizationKey, Kind: kindAuthorize, Mti: cardnet.AuthorizationRequest, MerchantCode: merchantCode(""),
	})
	if err != nil {
		return payments.Result{Outcome: payments.Unknown}
	}
	if !fresh {
		return c.Void(ctx, r)
	}
	c.settle(ctx, ex, stateReversed, "", "reversed_before_sent")
	return payments.Result{Outcome: payments.Approved, Reference: ex.Rrn}
}

func (c *Connector) settle(ctx context.Context, ex db.AcquirerExchange, state, rc, code string) payments.Result {
	result := payments.Result{}
	err := c.update(ctx, ex.Key, func(e *db.AcquirerExchange) bool {
		if e.State != stateSending {
			result = c.result(*e)
			return false
		}
		e.State, e.ResponseCode, e.DeclineCode = state, rc, code
		result = c.result(*e)
		return true
	})
	if err != nil {
		return payments.Result{Outcome: payments.Unknown}
	}
	return result
}

func (c *Connector) Refund(ctx context.Context, r payments.OperationRequest) payments.Result {
	auth, ok := c.approvedAuthorization(ctx, r.AuthorizationKey)
	if !ok {
		return c.repeatedOr(ctx, r.Key, declined(declineNoAuthorizing))
	}
	if r.Amount.Minor() > auth.Amount {
		return declined("invalid_amount")
	}
	ex, fresh, err := c.begin(ctx, db.InsertExchangeParams{
		Key: r.Key, Kind: kindRefund, AuthorizationKey: text(auth.Key), Mti: cardnet.FinancialRequest,
		Amount: r.Amount.Minor(), MerchantCode: auth.MerchantCode,
	})
	if err != nil {
		return payments.Result{Outcome: payments.Unknown}
	}
	if !fresh {
		return c.answer(ctx, ex)
	}
	m := c.base(ex)
	m.ProcessingCode = cardnet.ProcessingRefund
	m.Private = &cardnet.PrivateData{NetworkTransactionID: auth.NetworkTransactionID}
	resp, err := c.send(m)
	return c.finishRequest(ctx, ex, resp, err)
}

// repeatedOr answers a repeated call from its record, or with fallback when there is none.
func (c *Connector) repeatedOr(ctx context.Context, key string, fallback payments.Result) payments.Result {
	ex, err := db.New(c.cfg.Pool).GetExchange(ctx, key)
	if err != nil {
		return fallback
	}
	return c.answer(ctx, ex)
}

func (c *Connector) approvedAuthorization(ctx context.Context, key string) (db.AcquirerExchange, bool) {
	auth, err := db.New(c.cfg.Pool).GetExchange(ctx, key)
	return auth, err == nil && auth.State == stateApproved && auth.NetworkTransactionID != ""
}

func (c *Connector) Query(ctx context.Context, key string) payments.Result {
	ex, err := db.New(c.cfg.Pool).GetExchange(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return payments.Result{Outcome: payments.NotFound}
	}
	if err != nil {
		return payments.Result{Outcome: payments.Unknown}
	}
	return c.answer(ctx, ex)
}

// answer is what the rail knows of an exchange. One still sending after twice the
// timeout was abandoned by a process that stopped: it is taken over as a timeout.
func (c *Connector) answer(ctx context.Context, ex db.AcquirerExchange) payments.Result {
	if ex.State == stateSending && c.cfg.Now().Sub(ex.UpdatedAt.Time) > 2*c.cfg.Timeout {
		switch ex.Kind {
		case kindAuthorize, kindRefund:
			return c.abandon(ctx, ex, declineTimeout)
		case kindCapture:
			c.forwardLater(ctx, ex, stateAdvising)
		case kindVoid:
			c.forwardLater(ctx, ex, stateReversing)
		}
		return payments.Result{Outcome: payments.Pending}
	}
	return c.result(ex)
}

func (c *Connector) result(ex db.AcquirerExchange) payments.Result {
	reference := ex.Rrn
	switch {
	case ex.State == stateSending, ex.State == stateAdvising, ex.State == stateReversing && ex.Kind == kindVoid:
		return payments.Result{Outcome: payments.Pending}
	case ex.State == stateApproved, ex.State == stateAcknowledged, ex.State == stateReversed && ex.Kind == kindVoid:
		return payments.Result{Outcome: payments.Approved, Reference: reference, NetworkTransactionID: ex.NetworkTransactionID}
	default:
		code := ex.DeclineCode
		if code == "" {
			code = "generic_decline"
		}
		return payments.Result{Outcome: payments.Declined, Reference: reference, DeclineCode: code}
	}
}

func declined(code string) payments.Result {
	return payments.Result{Outcome: payments.Declined, DeclineCode: code}
}

// declineCode names an ISO 8583 response code the way the API reports declines.
func declineCode(rc string) string {
	switch rc {
	case cardnet.DoNotHonour:
		return "do_not_honor"
	case cardnet.InvalidTransaction:
		return "invalid_transaction"
	case cardnet.InvalidAmount:
		return "invalid_amount"
	case cardnet.InvalidCardNumber:
		return "incorrect_number"
	case cardnet.InsufficientFunds:
		return "insufficient_funds"
	case cardnet.ExpiredCard:
		return "expired_card"
	case cardnet.NotPermittedToCardholder:
		return "transaction_not_allowed"
	case cardnet.IssuerUnavailable:
		return "issuer_not_available"
	default:
		return "generic_decline"
	}
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
