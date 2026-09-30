package pix

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// Notifications: a client registers a URL per key, and the bank posts to {url}/pix, over
// mutual TLS, each Pix with a txid it receives on that key and each return that reaches
// a final status. A Pix without a txid is not notified; the client finds it with GET /pix.

// Delivery is a notification the bank sent, or dropped, for tests to look at.
type Delivery struct {
	URL     string
	E2EID   string
	Status  int
	Dropped bool
	Error   string
}

func (s *Sim) webhookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /webhook/{chave}", s.authorized(pixapi.ScopeWebhookWrite, func(w http.ResponseWriter, r *http.Request) {
		var body pixapi.WebhookSolicitado
		if err := decodeBody(r, &body); err != nil {
			problemf(w, http.StatusBadRequest, "WebhookOperacaoInvalida", "Webhook inválido.", "%v", err)
			return
		}
		u, err := url.Parse(body.WebhookUrl)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			problemf(w, http.StatusBadRequest, "WebhookOperacaoInvalida", "Webhook inválido.", "webhookUrl deve ser uma URL https.")
			return
		}
		key := r.PathValue("chave")
		s.mu.Lock()
		defer s.mu.Unlock()
		if e, ok := s.dict[key]; !ok || e.Account != clientOf(r) {
			problemf(w, http.StatusBadRequest, "WebhookOperacaoInvalida", "Webhook inválido.", "A chave %s não pertence ao usuário.", key)
			return
		}
		s.clients[clientOf(r)].webhooks[key] = body.WebhookUrl
		w.WriteHeader(http.StatusOK)
	}))
	mux.HandleFunc("GET /webhook/{chave}", s.authorized(pixapi.ScopeWebhookRead, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		u, ok := s.clients[clientOf(r)].webhooks[r.PathValue("chave")]
		s.mu.Unlock()
		if !ok {
			problemf(w, http.StatusNotFound, "NaoEncontrado", "Webhook não encontrado.", "Nenhum webhook para a chave %s.", r.PathValue("chave"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"webhookUrl": u, "chave": r.PathValue("chave")})
	}))
	mux.HandleFunc("DELETE /webhook/{chave}", s.authorized(pixapi.ScopeWebhookWrite, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delete(s.clients[clientOf(r)].webhooks, r.PathValue("chave"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
}

// notify posts a Pix, with its returns, to the webhook registered for its key.
func (s *Sim) notify(ctx context.Context, p *received) {
	s.mu.Lock()
	base, registered := s.clients[p.client].webhooks[p.key]
	body, _ := json.Marshal(pixapi.WebhookPixBody{Pix: &[]pixapi.Pix{p.render()}})
	s.mu.Unlock()
	if !registered {
		return
	}
	d := Delivery{URL: base + "/pix", E2EID: p.e2eid}
	if s.fault(Event{Kind: "webhook", Client: p.client, TxID: p.txid, E2EID: p.e2eid}).Drop {
		d.Dropped = true
		s.record(d)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		d.Error = err.Error()
		s.record(d)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.notifier.Do(req)
	if err != nil {
		d.Error = err.Error()
		s.logWarn(ctx, "delivering a Pix notification", "url", d.URL, "error", err)
		s.record(d)
		return
	}
	_ = resp.Body.Close()
	d.Status = resp.StatusCode
	s.record(d)
}

func (s *Sim) record(d Delivery) {
	s.mu.Lock()
	s.delivered = append(s.delivered, d)
	s.mu.Unlock()
}

// Deliveries returns the notifications sent so far.
func (s *Sim) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Delivery(nil), s.delivered...)
}
