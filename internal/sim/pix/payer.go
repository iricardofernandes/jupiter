package pix

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/brcode"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

// The rest of the Pix world, driven by tests and by operators through the admin
// handler: DICT entries, and payers who pay BR Codes or send Pix to keys the way a
// payer's bank would, reading the code, fetching and checking a dynamic code's signed
// payload, and settling through the SPI.

// Entry is a DICT entry: a key and the account it points to.
type Entry struct {
	Key     string `json:"key"`
	ISPB    string `json:"ispb"`
	Branch  string `json:"branch"`
	Account string `json:"account"`
	Name    string `json:"name"`
	TaxID   string `json:"tax_id"`
	Closed  bool   `json:"closed"`
}

// Payment is a payer paying: a BR Code, or a key directly. Amount (in reais, "10.00")
// is needed when the code does not fix it.
type Payment struct {
	BRCode     string `json:"brcode"`
	Key        string `json:"key"`
	Amount     string `json:"amount"`
	PayerName  string `json:"payer_name"`
	PayerTaxID string `json:"payer_tax_id"`
	Message    string `json:"message"`
}

type PaymentResult struct {
	// Authorized is the recurrence a payer authorized by reading its QR code, when the
	// code was a recurrence's and not a payment's.
	Authorized string `json:"authorized,omitempty"`
	EndToEndID string `json:"end_to_end_id,omitempty"`
	Amount     string `json:"amount,omitempty"`
	TxID       string `json:"txid,omitempty"`
	Refused    string `json:"refused,omitempty"`
}

var errRefused = errors.New("refused")

const fetchTimeout = 10 * time.Second

// Pay carries out a payer's payment. A refusal, such as a charge already paid or
// expired, is a result with Refused set, not an error.
func (s *Sim) Pay(ctx context.Context, p Payment) (PaymentResult, error) {
	if code, err := brcode.Parse(p.BRCode); err == nil && code.RecurrenceURL != "" && code.Key == "" && code.URL == "" {
		res, err := s.authorizeByQR(ctx, code, p)
		if errors.Is(err, errRefused) {
			return PaymentResult{Refused: strings.TrimPrefix(err.Error(), errRefused.Error()+": ")}, nil
		}
		return res, err
	}
	target, err := s.resolve(ctx, p)
	if errors.Is(err, errRefused) {
		return PaymentResult{Refused: strings.TrimPrefix(err.Error(), errRefused.Error()+": ")}, nil
	}
	if err != nil {
		return PaymentResult{}, err
	}
	rec, err := s.settlePayment(target, p)
	if errors.Is(err, errRefused) {
		return PaymentResult{Refused: strings.TrimPrefix(err.Error(), errRefused.Error()+": ")}, nil
	}
	if err != nil {
		return PaymentResult{}, err
	}
	if rec.txid != "" {
		s.notify(ctx, rec)
	}
	return PaymentResult{EndToEndID: rec.e2eid, Amount: pixapi.FormatValor(rec.amount), TxID: rec.txid}, nil
}

// target is what the payer's bank settled on paying.
type target struct {
	key    string
	txid   string
	amount int64
	charge bool
}

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errRefused, fmt.Sprintf(format, args...))
}

func (s *Sim) resolve(ctx context.Context, p Payment) (target, error) {
	if p.BRCode == "" {
		amount, err := pixapi.ParseValor(p.Amount)
		if err != nil || p.Key == "" {
			return target{}, refused("a key and an amount are needed")
		}
		return target{key: p.Key, amount: amount}, nil
	}
	code, err := brcode.Parse(p.BRCode)
	if err != nil {
		return target{}, refused("%v", err)
	}
	if code.URL == "" {
		t := target{key: code.Key}
		if code.TxID != brcode.NoTxID {
			t.txid = code.TxID
		}
		amount := code.Amount
		if amount == "" {
			amount = p.Amount
		}
		if t.amount, err = pixapi.ParseValor(amount); err != nil || t.amount <= 0 {
			return target{}, refused("the code has no amount and none was given")
		}
		return t, nil
	}
	return s.fetchPayload(ctx, code.URL, p)
}

// fetchPayload reads a dynamic code's payload as a payer's bank does: over HTTPS, from a
// location whose signature checks out against the key set on the same host.
func (s *Sim) fetchPayload(ctx context.Context, location string, p Payment) (target, error) {
	body, err := s.fetchSigned(ctx, location, "")
	if err != nil {
		return target{}, err
	}
	var payload struct {
		Chave  string `json:"chave"`
		Txid   string `json:"txid"`
		Status string `json:"status"`
		Valor  struct {
			Original            string `json:"original"`
			Final               string `json:"final"`
			ModalidadeAlteracao int32  `json:"modalidadeAlteracao"`
		} `json:"valor"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Status != statusActive {
		return target{}, refused("the charge is not active")
	}
	amount := payload.Valor.Original
	switch {
	case payload.Valor.Final != "":
		amount = payload.Valor.Final
	case payload.Valor.ModalidadeAlteracao == 1 && p.Amount != "":
		amount = p.Amount
	}
	t := target{key: payload.Chave, txid: payload.Txid, charge: true}
	if t.amount, err = pixapi.ParseValor(amount); err != nil || t.amount <= 0 {
		return target{}, refused("the payload has no amount to pay")
	}
	return t, nil
}

// fetchSigned fetches a payload location's JWS over HTTPS and returns its payload once
// its signature checks out against the key set its header names, which must be on the
// location's own host.
func (s *Sim) fetchSigned(ctx context.Context, location, query string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	host, _, _ := strings.Cut(location, "/")
	jws, err := s.fetch(ctx, "https://"+location+query)
	if err != nil {
		return nil, err
	}
	var header map[string]string
	if parts := splitJWS(string(jws)); parts != nil {
		raw, _ := decodeSegment(parts[0])
		_ = json.Unmarshal(raw, &header)
	}
	if header["jku"] != "https://"+host+jwksPath {
		return nil, refused("the payload's key set is not on the location's host")
	}
	rawKeys, err := s.fetch(ctx, header["jku"])
	if err != nil {
		return nil, err
	}
	var keys struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(rawKeys, &keys); err != nil {
		return nil, refused("an unreadable key set")
	}
	body, _, err := verifyJWS(string(jws), keys.Keys)
	if err != nil {
		return nil, refused("%v", err)
	}
	return body, nil
}

func (s *Sim) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, refused("a location that is not a URL")
	}
	resp, err := s.payers.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, refused("the location answered %d", resp.StatusCode)
	}
	return body, nil
}

// settlePayment moves the money to the key's account and, for a charge, concludes it.
// The receiving bank checks the charge again: the payload may have been read before it
// was paid, removed or expired.
func (s *Sim) settlePayment(t target, p Payment) (*received, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entry, ok := s.dict[t.key]
	switch {
	case !ok:
		return nil, refused("key %s is not in DICT", t.key)
	case entry.Closed:
		return nil, refused("the receiver's account is closed")
	case entry.ISPB != s.cfg.ISPB || s.clients[entry.Account] == nil:
		return nil, refused("key %s is at another participant, which is not simulated", t.key)
	}
	var ch *charge
	if t.charge {
		ch = s.charges[entry.Account+"/"+t.txid]
		switch {
		case ch == nil:
			return nil, refused("no charge %s", t.txid)
		case ch.status != statusActive:
			return nil, refused("the charge is %s", ch.status)
		case ch.expired(now):
			return nil, refused("the charge has expired")
		case !ch.due && !ch.changeable && t.amount != ch.original:
			return nil, refused("the charge is for %s", pixapi.FormatValor(ch.original))
		case ch.due && t.amount != ch.amountOn(dateOf(now)).total():
			return nil, refused("the charge is for %s today", pixapi.FormatValor(ch.amountOn(dateOf(now)).total()))
		}
	}
	rec := &received{
		client: entry.Account, e2eid: endToEndID("E", PayerISPB, now), txid: t.txid, key: t.key, amount: t.amount, at: now,
		message: p.Message, payerTaxID: p.PayerTaxID,
	}
	if ch != nil {
		ch.status = statusCompleted
		ch.e2eids = append(ch.e2eids, rec.e2eid)
		if ch.due {
			c := ch.amountOn(dateOf(now))
			rec.components = &c
		}
	}
	s.pix[rec.e2eid] = rec
	s.clients[entry.Account].balance += t.amount
	return rec, nil
}

func decodeSegment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// AddKey registers a key in DICT.
func (s *Sim) AddKey(e Entry) {
	s.mu.Lock()
	s.dict[e.Key] = e
	s.mu.Unlock()
}

// ClosePayer closes a payer's account: returns to it fail.
func (s *Sim) ClosePayer(taxID string) {
	s.mu.Lock()
	s.closedPayers[taxID] = true
	s.mu.Unlock()
}

// Balance is what a client's account holds, in centavos.
func (s *Sim) Balance(client string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.clients[client]; a != nil {
		return a.balance
	}
	return 0
}

// AdminHandler serves the operator's controls: paying, DICT, and what happened. It
// takes no credentials and must listen on loopback only.
func (s *Sim) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/pay", func(w http.ResponseWriter, r *http.Request) {
		var p Payment
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&p); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res, err := s.Pay(r.Context(), p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		status := http.StatusOK
		if res.Refused != "" {
			status = http.StatusUnprocessableEntity
		}
		writeJSON(w, status, res)
	})
	mux.HandleFunc("POST /admin/dict/keys", func(w http.ResponseWriter, r *http.Request) {
		var e Entry
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&e); err != nil || e.Key == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if e.ISPB == "" {
			e.ISPB = "30000003"
		}
		s.AddKey(e)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /admin/payers/{tax_id}/close", func(w http.ResponseWriter, r *http.Request) {
		s.ClosePayer(r.PathValue("tax_id"))
		w.WriteHeader(http.StatusNoContent)
	})
	s.adminRecurrenceRoutes(mux)
	mux.HandleFunc("GET /admin/transfers", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Transfers())
	})
	mux.HandleFunc("GET /admin/deliveries", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Deliveries())
	})
	mux.HandleFunc("POST /admin/infraction-reports", func(w http.ResponseWriter, r *http.Request) {
		var p InfractionParams
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&p); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		report, err := s.ReportInfraction(r.Context(), p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, http.StatusCreated, report)
	})
	mux.HandleFunc("GET /admin/balances/{client}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"balance": pixapi.FormatValor(s.Balance(r.PathValue("client")))})
	})
	return loopbackJSON(mux)
}

// adminRecurrenceRoutes are the payer's side of Pix Automático: answering a request,
// revoking an authorization, and how much money they have.
func (s *Sim) adminRecurrenceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/recurrence-requests/{id}/decide", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Accept bool `json:"accept"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.DecideRequest(r.Context(), r.PathValue("id"), body.Accept); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /admin/recurrences/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := s.CancelAsPayer(r.Context(), r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /admin/payers/{tax_id}/funds", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Balance *string `json:"balance"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		amount := int64(-1)
		if body.Balance != nil {
			v, err := pixapi.ParseValor(*body.Balance)
			if err != nil {
				http.Error(w, "bad balance", http.StatusBadRequest)
				return
			}
			amount = v
		}
		s.SetPayerFunds(r.PathValue("tax_id"), amount)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /admin/tick", func(w http.ResponseWriter, r *http.Request) {
		s.Tick(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
}

// loopbackJSON admits only requests addressed to a loopback host, and POSTs only with a
// JSON body: a web page the operator visits can neither reach the controls by a
// rebinding name nor post to them without a preflight.
func loopbackJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "the admin controls answer on loopback only", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "a JSON body is required", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}
