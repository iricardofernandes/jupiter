package cardnetwork

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// The network token service: it issues a token (a DPAN, a card number of the network's
// own) for a card, one-time cryptograms for payments with it, and tells the token
// requestor when the card behind a token changes. Authorizations with a token carry the
// DPAN in DE 2 and the cryptogram in DE 48; the network checks the cryptogram and swaps
// in the real card before the issuer sees the request.
type tokenService struct {
	mu          sync.Mutex
	byReference map[string]*networkToken
	byNumber    map[string]*networkToken
	sequence    int64
}

type networkToken struct {
	Reference   string
	Number      string
	ExpMonth    int
	ExpYear     int
	PAN         string
	Requestor   string
	Status      string
	cryptograms map[string]int64
}

func newTokenService() *tokenService {
	return &tokenService{byReference: map[string]*networkToken{}, byNumber: map[string]*networkToken{}}
}

// dpanPrefix is the range the simulated network issues tokens from.
const dpanPrefix = "489537"

func (t *tokenService) provision(requestor, pan string, month, year int) (*networkToken, error) {
	if !luhn(pan) {
		return nil, errors.New("not a card number")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sequence++
	body := fmt.Sprintf("%s%09d", dpanPrefix, t.sequence)
	token := &networkToken{
		Reference: fmt.Sprintf("DNITHE%014d", t.sequence), Number: body + checkDigit(body), ExpMonth: month, ExpYear: year,
		PAN: pan, Requestor: requestor, Status: "active", cryptograms: map[string]int64{},
	}
	t.byReference[token.Reference] = token
	t.byNumber[token.Number] = token
	return token, nil
}

func checkDigit(body string) string {
	for d := range 10 {
		if luhn(body + strconv.Itoa(d)) {
			return strconv.Itoa(d)
		}
	}
	return "0"
}

// cryptogram issues a one-time cryptogram for a payment of amount with the token.
func (t *tokenService) cryptogram(reference string, amount int64) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	token, ok := t.byReference[reference]
	if !ok || token.Status != "active" {
		return "", fmt.Errorf("no active token %s", reference)
	}
	raw := make([]byte, 20)
	_, _ = rand.Read(raw)
	c := base64.StdEncoding.EncodeToString(raw)
	token.cryptograms[c] = amount
	return c, nil
}

// redeem checks an authorization made with a token and returns the real card. A
// cryptogram is good once, for the amount it was issued for.
func (t *tokenService) redeem(dpan, cryptogram string, amount int64) (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	token, ok := t.byNumber[dpan]
	switch {
	case !ok:
		return "", ""
	case token.Status != "active":
		return "", cardnet.NotPermittedToCardholder
	}
	issued, ok := token.cryptograms[cryptogram]
	if !ok || issued != amount {
		return "", cardnet.DoNotHonour
	}
	delete(token.cryptograms, cryptogram)
	return token.PAN, cardnet.Approved
}

// replace moves every token of a card to its replacement, and returns them.
func (t *tokenService) replace(pan, newPAN string, month, year int) []networkToken {
	t.mu.Lock()
	defer t.mu.Unlock()
	var changed []networkToken
	for _, token := range t.byReference {
		if token.PAN == pan {
			token.PAN, token.ExpMonth, token.ExpYear = newPAN, month, year
			changed = append(changed, *token)
		}
	}
	return changed
}

func (t *tokenService) suspend(reference string) (networkToken, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	token, ok := t.byReference[reference]
	if !ok {
		return networkToken{}, false
	}
	token.Status = "suspended"
	return *token, true
}

// notify tells the token requestor about a token, signed as the network signs.
func (n *Network) notify(ctx context.Context, token networkToken, eventType string) {
	if n.cfg.TokenEventsURL == "" {
		return
	}
	body, _ := json.Marshal(cardnet.TokenEvent{
		Type: eventType, Reference: token.Reference, Status: token.Status, Last4: token.PAN[len(token.PAN)-4:],
		ExpMonth: token.ExpMonth, ExpYear: token.ExpYear, OccurredAt: n.cfg.Now().UTC(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.TokenEventsURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(cardnet.EventSignatureHeader, cardnet.SignEvent(body, n.cfg.TokenEventsSecret, n.cfg.Now()))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		n.cfg.Logger.WarnContext(ctx, "telling the token requestor", "error", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		n.cfg.Logger.WarnContext(ctx, "the token requestor refused an event", "status", resp.StatusCode)
	}
}

func (n *Network) tokenRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/tokens", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Requestor string `json:"token_requestor_id"`
			PAN       string `json:"pan"`
			ExpMonth  int    `json:"exp_month"`
			ExpYear   int    `json:"exp_year"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		token, err := n.tokens.provision(body.Requestor, body.PAN, body.ExpMonth, body.ExpYear)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, map[string]any{
			"token_reference": token.Reference, "token": token.Number, "exp_month": token.ExpMonth, "exp_year": token.ExpYear, "status": token.Status,
		})
	})
	mux.HandleFunc("POST /v1/tokens/{reference}/cryptograms", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Amount int64 `json:"amount"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body) != nil || body.Amount <= 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		c, err := n.tokens.cryptogram(r.PathValue("reference"), body.Amount)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"cryptogram": c})
	})
	mux.HandleFunc("POST /admin/cards/replace", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PAN      string `json:"pan"`
			NewPAN   string `json:"new_pan"`
			ExpMonth int    `json:"exp_month"`
			ExpYear  int    `json:"exp_year"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body) != nil || !luhn(body.NewPAN) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for _, token := range n.tokens.replace(body.PAN, body.NewPAN, body.ExpMonth, body.ExpYear) {
			n.notify(r.Context(), token, "token.updated")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /admin/tokens/{reference}/suspend", func(w http.ResponseWriter, r *http.Request) {
		token, ok := n.tokens.suspend(r.PathValue("reference"))
		if !ok {
			http.Error(w, "no such token", http.StatusNotFound)
			return
		}
		n.notify(r.Context(), token, "token.suspended")
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
