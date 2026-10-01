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

	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// MED claims reach Jupiter through its bank's relay of the DICT's infraction reports
// (pkg/pixapi/med.go): notified at {WebhookURL}/infracoes, read back before they are
// applied, and answered at the bank.

var _ disputes.Bank = (*Connector)(nil)

func (c *Connector) Infraction(ctx context.Context, id string) (disputes.Infraction, error) {
	var out pixapi.InfractionReport
	if err := c.medCall(ctx, http.MethodGet, "/infracoes/"+url.PathEscape(id), nil, &out); err != nil {
		return disputes.Infraction{}, err
	}
	return infractionOf(out)
}

func (c *Connector) Infractions(ctx context.Context, since, until time.Time) ([]disputes.Infraction, error) {
	q := url.Values{"inicio": {since.UTC().Format(time.RFC3339)}, "fim": {until.UTC().Format(time.RFC3339)}}
	var out pixapi.InfractionReports
	if err := c.medCall(ctx, http.MethodGet, "/infracoes?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	claims := make([]disputes.Infraction, 0, len(out.InfractionReports))
	for _, r := range out.InfractionReports {
		inf, err := infractionOf(r)
		if err != nil {
			c.cfg.Logger.ErrorContext(ctx, "a MED claim the bank lists that cannot be read", "infraction", r.ID, "error", err)
			continue
		}
		claims = append(claims, inf)
	}
	return claims, nil
}

func (c *Connector) Analyze(ctx context.Context, id string, a disputes.Analysis) (disputes.Infraction, error) {
	body := pixapi.InfractionAnalysis{AnalysisResult: a.Result, AnalysisDetails: a.Details, RefundID: a.RefundID}
	if a.RefundAmount > 0 {
		body.Valor = pixapi.FormatValor(a.RefundAmount)
	}
	for _, h := range a.Trace {
		body.FundsTrace = append(body.FundsTrace, pixapi.TraceHop{EndToEndID: h.EndToEndID, Valor: pixapi.FormatValor(h.Amount)})
	}
	var out pixapi.InfractionReport
	if err := c.medCall(ctx, http.MethodPost, "/infracoes/"+url.PathEscape(id)+"/analise", body, &out); err != nil {
		return disputes.Infraction{}, err
	}
	return infractionOf(out)
}

func (c *Connector) Contest(ctx context.Context, id, details string) (disputes.Infraction, error) {
	var out pixapi.InfractionReport
	if err := c.medCall(ctx, http.MethodPost, "/infracoes/"+url.PathEscape(id)+"/contestacao", pixapi.InfractionContestation{Details: details}, &out); err != nil {
		return disputes.Infraction{}, err
	}
	return infractionOf(out)
}

// medCall is call, with the bank's refusals as the disputes service knows them.
func (c *Connector) medCall(ctx context.Context, method, path string, body, out any) error {
	err := c.call(ctx, method, path, body, out)
	if errors.Is(err, payments.ErrPixRefused) || (method != http.MethodGet && errors.Is(err, payments.ErrPixNotFound)) {
		return fmt.Errorf("%w: %w", disputes.ErrRefused, err)
	}
	return err
}

func infractionOf(r pixapi.InfractionReport) (disputes.Infraction, error) {
	amount, err := pixapi.ParseValor(r.Valor)
	if err != nil {
		return disputes.Infraction{}, err
	}
	inf := disputes.Infraction{
		ID: r.ID, EndToEndID: r.EndToEndID, Amount: amount, Reason: r.Reason, Details: r.ReportDetails, Status: r.Status,
		Result: r.AnalysisResult, ContestedAt: r.ContestedAt, NotifiedAt: r.CreationTime, RefundID: r.RefundID, RefundStatus: r.RefundStatus,
	}
	if r.RefundValor != "" {
		if inf.Refunded, err = pixapi.ParseValor(r.RefundValor); err != nil {
			return disputes.Infraction{}, err
		}
	}
	if r.Contestation != nil {
		inf.Contestation = r.Contestation.Status
	}
	return inf, nil
}

// infractionRoute takes the bank's notifications of MED claims: each named is read back
// from the bank and applied.
func (c *Connector) infractionRoute(mux *http.ServeMux, claims *disputes.Service) {
	mux.HandleFunc("POST /infracoes", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "a client certificate is required", http.StatusUnauthorized)
			return
		}
		var body pixapi.InfractionReports
		if err := json.NewDecoder(io.LimitReader(r.Body, maxNotification)).Decode(&body); err != nil {
			http.Error(w, "not a MED notification", http.StatusBadRequest)
			return
		}
		var failed []error
		seen := map[string]bool{}
		for _, n := range body.InfractionReports {
			if seen[n.ID] || len(seen) == maxNotified {
				continue // the rest wait for reconciliation
			}
			seen[n.ID] = true
			failed = append(failed, c.applyClaim(r.Context(), claims, n.ID))
		}
		if err := errors.Join(failed...); err != nil {
			c.cfg.Logger.ErrorContext(r.Context(), "applying a MED notification", "error", err)
			http.Error(w, "not applied", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// applyClaim reads a claim back from the bank and applies it; one the bank does not have
// is ignored.
func (c *Connector) applyClaim(ctx context.Context, claims *disputes.Service, id string) error {
	inf, err := c.Infraction(ctx, id)
	if errors.Is(err, payments.ErrPixNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return claims.ApplyInfraction(ctx, c.cfg.Pool, c.cfg.Livemode, inf)
}
