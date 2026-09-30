package pix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// The bank's notifications. They arrive over mutual TLS, from the bank's certificate
// only, but are still not taken on their word: each Pix a notification names is read
// back from the bank (GET /pix/{e2eid}) before it is applied, so a notification can at
// worst make Jupiter look something up. Pix the bank does not notify (without a txid)
// or whose notification was lost are found by Reconcile, from GET /pix.

const (
	maxNotification = 1 << 20
	// maxNotified is the most Pix one notification is read for; the rest wait for
	// reconciliation.
	maxNotified = 100
	// reconcileWindow is how far back each reconciliation reads: every Pix received in
	// it is looked at again, and those already applied cost nothing.
	reconcileWindow   = 72 * time.Hour
	reconcilePageSize = 100
	reconcileMaxPages = 500
)

// Handler takes the bank's notifications, posted to {WebhookURL}/pix, and those of Pix
// Automático, to {WebhookURL}/rec and /cobr, which sync, if not nil, is told of. It must
// be served over mutual TLS that admits only the bank's client certificate.
func (c *Connector) Handler(p *payments.Service, sync RecurrenceSync) http.Handler {
	mux := http.NewServeMux()
	c.recurrenceRoutes(mux, sync)
	mux.HandleFunc("POST /pix", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "a client certificate is required", http.StatusUnauthorized)
			return
		}
		var body pixapi.WebhookPixBody
		if err := json.NewDecoder(io.LimitReader(r.Body, maxNotification)).Decode(&body); err != nil || body.Pix == nil {
			http.Error(w, "not a Pix notification", http.StatusBadRequest)
			return
		}
		var failed []error
		seen := map[string]bool{}
		for _, n := range *body.Pix {
			if seen[n.EndToEndId] || len(seen) == maxNotified {
				continue
			}
			seen[n.EndToEndId] = true
			if err := c.receive(r.Context(), p, n.EndToEndId); err != nil {
				failed = append(failed, err)
			}
		}
		if err := errors.Join(failed...); err != nil {
			c.cfg.Logger.ErrorContext(r.Context(), "applying a Pix notification", "error", err)
			http.Error(w, "not applied", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// receive reads a Pix from the bank and applies it.
func (c *Connector) receive(ctx context.Context, p *payments.Service, e2eID string) error {
	payment, err := c.Payment(ctx, e2eID)
	if errors.Is(err, payments.ErrPixNotFound) {
		c.cfg.Logger.WarnContext(ctx, "a notification of a Pix the bank does not have", "e2e_id", e2eID)
		return nil
	}
	if err != nil {
		return err
	}
	return p.ReceivePix(ctx, c.cfg.Pool, c.cfg.Livemode, payment)
}

// RegisterWebhook tells the bank where to notify Jupiter: of Pix to its key, and of
// recurrences and recurring charges.
func (c *Connector) RegisterWebhook(ctx context.Context) error {
	if c.cfg.WebhookURL == "" {
		return nil
	}
	body := pixapi.WebhookSolicitado{WebhookUrl: c.cfg.WebhookURL}
	return errors.Join(
		c.call(ctx, "PUT", "/webhook/"+url.PathEscape(c.cfg.Key), body, nil),
		c.call(ctx, "PUT", "/webhookrec", body, nil),
		c.call(ctx, "PUT", "/webhookcobr", body, nil),
	)
}

// Reconcile reads from the bank every Pix received in the last reconcileWindow, and
// applies those Jupiter did not: what notifications missed. A Pix it cannot read is
// logged and skipped. It returns how many it read.
func (c *Connector) Reconcile(ctx context.Context, p *payments.Service) (int, error) {
	now := c.cfg.Now().UTC()
	start := now.Add(-reconcileWindow)
	read := 0
	for page := range reconcileMaxPages {
		q := url.Values{
			"inicio": {start.Format(time.RFC3339)}, "fim": {now.Add(time.Second).Format(time.RFC3339)},
			"paginacao.paginaAtual": {strconv.Itoa(page)}, "paginacao.itensPorPagina": {strconv.Itoa(reconcilePageSize)},
		}
		var out pixapi.PixConsultados
		if err := c.call(ctx, "GET", "/pix?"+q.Encode(), nil, &out); err != nil {
			return read, err
		}
		if out.Pix == nil || len(*out.Pix) == 0 {
			return read, nil
		}
		var failed []error
		for _, raw := range *out.Pix {
			payment, err := paymentOf(raw)
			if err != nil {
				c.cfg.Logger.ErrorContext(ctx, "a Pix the bank lists that cannot be read", "e2e_id", raw.EndToEndId, "error", err)
				continue
			}
			if err := p.ReceivePix(ctx, c.cfg.Pool, c.cfg.Livemode, payment); err != nil {
				failed = append(failed, err)
				continue
			}
			read++
		}
		if err := errors.Join(failed...); err != nil {
			return read, err
		}
		if page+1 >= out.Parametros.Paginacao.QuantidadeDePaginas { //nolint:misspell // the API Pix's field name
			return read, nil
		}
	}
	c.cfg.Logger.WarnContext(ctx, "reconciliation stopped at its page limit", "pages", reconcileMaxPages)
	return read, nil
}
