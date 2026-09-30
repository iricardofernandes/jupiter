package pix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/subscriptions"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Pix Automático through the API Pix: recurrences (rec), their locations (locrec) and
// requests to the payer's bank (solicrec), for subscriptions; recurring charges (cobr),
// for payments. As with Pix, the bank's notifications (at {webhook}/rec and
// {webhook}/cobr) only say what to read again.

var _ subscriptions.Bank = (*Connector)(nil)

func cobrPath(txid string) string { return "/cobr/" + url.PathEscape(txid) }

// CreateRecurringCharge creates a recurring charge (PUT /cobr/{txid}), to Jupiter's
// account, adjusted to a business day. A txid is never used twice, so one the bank
// refuses to create again is read back.
func (c *Connector) CreateRecurringCharge(ctx context.Context, r payments.RecurringChargeRequest) (payments.RecurringCharge, error) {
	var body pixapi.CobRSolicitada
	body.IdRec, body.AjusteDiaUtil, body.Valor.Original = r.RecurrenceID, true, pixapi.FormatValor(r.Amount.Minor())
	_ = body.Calendario.DataDeVencimento.UnmarshalText([]byte(r.DueDate))
	body.Recebedor = pixapi.DadosBancariosRecebedor{Conta: c.cfg.Account, TipoConta: "CORRENTE"}
	if c.cfg.Branch != "" {
		body.Recebedor.Agencia = pixapi.Ptr(c.cfg.Branch)
	}
	if r.Description != "" {
		body.InfoAdicional = pixapi.Ptr(r.Description)
	}
	var out pixapi.CobRCompleta
	err := c.call(ctx, http.MethodPut, cobrPath(r.TxID), body, &out)
	if errors.Is(err, payments.ErrPixRefused) {
		if existing, getErr := c.RecurringCharge(ctx, r.TxID); getErr == nil {
			return existing, nil
		}
	}
	if err != nil {
		return payments.RecurringCharge{}, err
	}
	return recurringOf(out), nil
}

func (c *Connector) RecurringCharge(ctx context.Context, txid string) (payments.RecurringCharge, error) {
	var out pixapi.CobRCompleta
	if err := c.call(ctx, http.MethodGet, cobrPath(txid), nil, &out); err != nil {
		return payments.RecurringCharge{}, err
	}
	return recurringOf(out), nil
}

// CancelRecurringCharge cancels a charge (PATCH with status CANCELADA); one the bank no
// longer lets be canceled is read as it is.
func (c *Connector) CancelRecurringCharge(ctx context.Context, txid string) (payments.RecurringCharge, error) {
	var out pixapi.CobRCompleta
	err := c.call(ctx, http.MethodPatch, cobrPath(txid), map[string]string{"status": payments.RecurringCanceled}, &out)
	if errors.Is(err, payments.ErrPixRefused) {
		return c.RecurringCharge(ctx, txid)
	}
	if err != nil {
		return payments.RecurringCharge{}, err
	}
	return recurringOf(out), nil
}

// RetryRecurringCharge asks for another attempt (POST /cobr/{txid}/retentativa/{data}).
func (c *Connector) RetryRecurringCharge(ctx context.Context, txid, date string) (payments.RecurringCharge, error) {
	var out pixapi.CobRCompleta
	if err := c.call(ctx, http.MethodPost, cobrPath(txid)+"/retentativa/"+url.PathEscape(date), nil, &out); err != nil {
		return payments.RecurringCharge{}, err
	}
	return recurringOf(out), nil
}

func recurringOf(out pixapi.CobRCompleta) payments.RecurringCharge {
	charge := payments.RecurringCharge{TxID: out.Txid, Status: string(out.Status)}
	if out.Tentativas != nil {
		for _, t := range *out.Tentativas {
			charge.Attempts = append(charge.Attempts, payments.RecurringAttempt{
				Date: t.DataLiquidacao.String(), Kind: string(t.Tipo), Status: string(t.Status), E2EID: t.EndToEndId,
			})
		}
	}
	if out.Pix != nil {
		for _, p := range *out.Pix {
			charge.Payments = append(charge.Payments, p.EndToEndId)
		}
	}
	charge.EndedBy, charge.EndCode = ending(out.Encerramento)
	return charge
}

func ending(e *pixapi.Encerramento) (by, code string) {
	switch {
	case e == nil:
	case e.Cancelamento != nil:
		if e.Cancelamento.Solicitante != nil {
			by = string(*e.Cancelamento.Solicitante)
		}
		if e.Cancelamento.Codigo != nil {
			code = *e.Cancelamento.Codigo
		}
	case e.Rejeicao != nil && e.Rejeicao.Codigo != nil:
		code = *e.Rejeicao.Codigo
	}
	return by, code
}

// CreateRecurrenceLocation makes a location for a recurrence's QR code (POST /locrec).
func (c *Connector) CreateRecurrenceLocation(ctx context.Context) (int64, error) {
	var out pixapi.PayloadLocationRecGerada
	if err := c.call(ctx, http.MethodPost, "/locrec", nil, &out); err != nil {
		return 0, bankError(err)
	}
	return out.Id, nil
}

// CreateRecurrence creates a recurrence (POST /rec).
func (c *Connector) CreateRecurrence(ctx context.Context, r subscriptions.RecurrenceRequest) (subscriptions.Recurrence, error) {
	body := pixapi.RecSolicitada{PoliticaRetentativa: "NAO_PERMITE"}
	if r.Retries {
		body.PoliticaRetentativa = "PERMITE_3R_7D"
	}
	body.Vinculo.Contrato, body.Vinculo.Objeto = r.Contract, pixapi.Ptr(r.Object)
	body.Vinculo.Devedor = pixapi.Pessoa{Nome: pixapi.Ptr(r.PayerName)}
	if len(r.PayerTaxID) == 11 {
		body.Vinculo.Devedor.Cpf = pixapi.Ptr(r.PayerTaxID)
	} else {
		body.Vinculo.Devedor.Cnpj = pixapi.Ptr(r.PayerTaxID)
	}
	_ = body.Calendario.DataInicial.UnmarshalText([]byte(r.Start))
	body.Calendario.Periodicidade = pixapi.RecSolicitadaCalendarioPeriodicidade(r.Period)
	if r.End != "" {
		_ = pixapi.New(&body.Calendario.DataFinal).UnmarshalText([]byte(r.End))
	}
	pixapi.New(&body.Valor).ValorRec = pixapi.Ptr(pixapi.FormatValor(r.Amount.Minor()))
	if r.Location != 0 {
		body.Loc = pixapi.Ptr(r.Location)
	}
	var out pixapi.RecCompleta
	if err := c.call(ctx, http.MethodPost, "/rec", body, &out); err != nil {
		return subscriptions.Recurrence{}, bankError(err)
	}
	return recurrenceOf(out), nil
}

func (c *Connector) Recurrence(ctx context.Context, id string) (subscriptions.Recurrence, error) {
	var out pixapi.RecCompleta
	if err := c.call(ctx, http.MethodGet, "/rec/"+url.PathEscape(id), nil, &out); err != nil {
		return subscriptions.Recurrence{}, bankError(err)
	}
	return recurrenceOf(out), nil
}

// FindRecurrence looks through the recurrences made for a payer since a time (GET /rec)
// for the one with a contract.
func (c *Connector) FindRecurrence(ctx context.Context, contract, payerTaxID string, since time.Time) (subscriptions.Recurrence, error) {
	q := url.Values{"inicio": {since.UTC().Format(time.RFC3339)}, "fim": {c.cfg.Now().UTC().Add(time.Minute).Format(time.RFC3339)}}
	if len(payerTaxID) == 11 {
		q.Set("cpf", payerTaxID)
	} else {
		q.Set("cnpj", payerTaxID)
	}
	var out struct {
		Recs []pixapi.RecCompleta `json:"recs"`
	}
	if err := c.call(ctx, http.MethodGet, "/rec?"+q.Encode(), nil, &out); err != nil {
		return subscriptions.Recurrence{}, bankError(err)
	}
	for _, rec := range out.Recs {
		if rec.Vinculo.Contrato == contract {
			return c.Recurrence(ctx, rec.IdRec)
		}
	}
	return subscriptions.Recurrence{}, fmt.Errorf("%w: no recurrence for contract %s", subscriptions.ErrBankNotFound, contract)
}

// RequestAuthorization asks the payer's bank to show them the recurrence (POST /solicrec).
func (c *Connector) RequestAuthorization(ctx context.Context, r subscriptions.AuthorizationRequest) error {
	body := pixapi.SolicRecBase{IdRec: r.RecurrenceID, Destinatario: pixapi.Destinatario{Conta: r.PayerAccount, IspbParticipante: r.PayerISPB}}
	body.Calendario.DataExpiracaoSolicitacao = r.Expires.UTC()
	if r.PayerBranch != "" {
		body.Destinatario.Agencia = pixapi.Ptr(r.PayerBranch)
	}
	if len(r.PayerTaxID) == 11 {
		body.Destinatario.Cpf = pixapi.Ptr(r.PayerTaxID)
	} else {
		body.Destinatario.Cnpj = pixapi.Ptr(r.PayerTaxID)
	}
	return bankError(c.call(ctx, http.MethodPost, "/solicrec", body, nil))
}

// CancelRecurrence cancels a recurrence (PATCH /rec/{idRec} with status CANCELADA); one
// already ended is read as it is.
func (c *Connector) CancelRecurrence(ctx context.Context, id string) (subscriptions.Recurrence, error) {
	var out pixapi.RecCompleta
	err := c.call(ctx, http.MethodPatch, "/rec/"+url.PathEscape(id), map[string]string{"status": subscriptions.RecurrenceCanceled}, &out)
	if errors.Is(err, payments.ErrPixRefused) {
		return c.Recurrence(ctx, id)
	}
	if err != nil {
		return subscriptions.Recurrence{}, bankError(err)
	}
	return recurrenceOf(out), nil
}

func recurrenceOf(out pixapi.RecCompleta) subscriptions.Recurrence {
	rec := subscriptions.Recurrence{ID: out.IdRec, Status: string(out.Status)}
	if out.DadosQR != nil && out.DadosQR.PixCopiaECola != nil {
		rec.QRCode = *out.DadosQR.PixCopiaECola
	}
	rec.EndedBy, rec.EndCode = ending(out.Encerramento)
	if out.Solicitacao != nil {
		for _, q := range *out.Solicitacao {
			pending := q.Status == pixapi.SolicRecCompletaStatusCRIADA || q.Status == pixapi.SolicRecCompletaStatusENVIADA ||
				q.Status == pixapi.SolicRecCompletaStatusRECEBIDA
			rec.PendingRequest = rec.PendingRequest || pending
			rec.RequestRejected = q.Status == pixapi.SolicRecCompletaStatusREJEITADA
		}
	}
	return rec
}

// bankError translates the payments rail's errors into the subscriptions module's.
func bankError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, payments.ErrPixNotFound):
		return fmt.Errorf("%w: %w", subscriptions.ErrBankNotFound, err)
	case errors.Is(err, payments.ErrPixRefused):
		return fmt.Errorf("%w: %w", subscriptions.ErrBankRefused, err)
	}
	return err
}

// RecurrenceSync is what the connector tells when the bank notifies it of a recurrence
// or a charge under one.
type RecurrenceSync interface {
	SyncRecurrence(ctx context.Context, pool *pgxpool.Pool, livemode bool, recurrenceID string) error
}

func (c *Connector) recurrenceRoutes(mux *http.ServeMux, sync RecurrenceSync) {
	mux.HandleFunc("POST /rec", func(w http.ResponseWriter, r *http.Request) {
		var body pixapi.WebhookRecBody
		if !c.notification(w, r, &body) || body.Recs == nil {
			return
		}
		ids := make([]string, 0, len(*body.Recs))
		for _, n := range *body.Recs {
			ids = append(ids, n.IdRec)
		}
		c.syncAll(w, r, sync, ids)
	})
	mux.HandleFunc("POST /cobr", func(w http.ResponseWriter, r *http.Request) {
		var body pixapi.WebhookCobRBody
		if !c.notification(w, r, &body) || body.Cobsr == nil {
			return
		}
		ids := make([]string, 0, len(*body.Cobsr))
		for _, n := range *body.Cobsr {
			ids = append(ids, n.IdRec)
		}
		c.syncAll(w, r, sync, ids)
	})
}

// notification reads a notification's body, admitting only the bank's certificate.
func (c *Connector) notification(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(w, "a client certificate is required", http.StatusUnauthorized)
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxNotification)).Decode(v); err != nil {
		http.Error(w, "not a notification", http.StatusBadRequest)
		return false
	}
	return true
}

// syncAll brings forward the subscriptions of the recurrences named, which read the
// bank again themselves.
func (c *Connector) syncAll(w http.ResponseWriter, r *http.Request, sync RecurrenceSync, ids []string) {
	if sync == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	seen := map[string]bool{}
	var failed []error
	for _, recID := range ids {
		if seen[recID] || len(seen) == maxNotified || !pixapi.ValidRecID(recID) {
			continue
		}
		seen[recID] = true
		if err := sync.SyncRecurrence(r.Context(), c.cfg.Pool, c.cfg.Livemode, recID); err != nil {
			failed = append(failed, err)
		}
	}
	if err := errors.Join(failed...); err != nil {
		c.cfg.Logger.ErrorContext(r.Context(), "applying a Pix Automático notification", "error", err)
		http.Error(w, "not applied", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
