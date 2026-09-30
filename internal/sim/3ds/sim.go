// Package threedssim simulates a card scheme's 3-D Secure directory server and the
// issuers' access control servers behind it. Jupiter's 3DS server reaches it over HTTP
// with the messages of pkg/threeds; the cardholder's browser reaches the challenge pages.
package threedssim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
	"uuid"

	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

// ChallengeCode is the one-time code that passes a challenge.
const ChallengeCode = "123456"

type Config struct {
	Now    func() time.Time
	Logger *slog.Logger
	// PublicURL is where browsers reach this simulator, for the ACS URL.
	PublicURL string
	// AuthenticationKey makes the authentication values the card network simulator
	// checks: the two simulators share it, as a scheme and its issuers share keys.
	AuthenticationKey []byte
	// ResultsSecret signs the results requests sent to 3DS servers.
	ResultsSecret string
	HTTPClient    *http.Client
}

type Server struct {
	cfg Config
	mu  sync.Mutex
	txs map[string]*transaction
}

type transaction struct {
	areq       threeds.AReq
	dsTransID  string
	acsTransID string
	done       bool
}

func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Server{cfg: cfg, txs: map[string]*transaction{}}
}

// Handler serves the directory server and the ACS:
//
//	POST /ds/areq                 authentication request (JSON AReq, answered with ARes or Erro)
//	POST /acs/challenge           the browser posts a CReq and gets the challenge page
//	POST /acs/challenge/complete  the cardholder submits the one-time code
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ds/areq", s.areq)
	mux.HandleFunc("POST /acs/challenge", s.challenge)
	mux.HandleFunc("POST /acs/challenge/complete", s.complete)
	return mux
}

type behaviour string

const (
	frictionless     behaviour = threeds.StatusAuthenticated
	challenge        behaviour = threeds.StatusChallenge
	notAuthenticated behaviour = threeds.StatusNotAuthenticated
	rejected         behaviour = threeds.StatusRejected
	unavailable      behaviour = threeds.StatusUnavailable
	attempted        behaviour = threeds.StatusAttempted
)

// TestCards choose what the ACS does; every other card is authenticated frictionless.
var TestCards = map[string]behaviour{
	"4000000000003220": challenge,
	"4000000000003238": notAuthenticated,
	"4000000000003246": rejected,
	"4000000000003253": unavailable,
	"4000000000003261": attempted,
}

func (s *Server) areq(w http.ResponseWriter, r *http.Request) {
	var a threeds.AReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&a); err != nil {
		s.fail(w, "203", "The AReq is not valid JSON.")
		return
	}
	if problem := check(a); problem != "" {
		s.fail(w, "203", problem)
		return
	}
	tx := &transaction{areq: a, dsTransID: uuid.NewV4().String(), acsTransID: uuid.NewV4().String()}
	res := threeds.ARes{
		MessageType: threeds.TypeARes, MessageVersion: threeds.Version, ThreeDSServerTransID: a.ThreeDSServerTransID,
		DSTransID: tx.dsTransID, ACSTransID: tx.acsTransID,
	}
	b, ok := TestCards[a.AcctNumber]
	if !ok {
		b = frictionless
	}
	res.TransStatus = string(b)
	switch b {
	case frictionless, attempted:
		res.ECI, res.AuthenticationValue = s.authenticate(a, tx.dsTransID, b)
	case challenge:
		res.ACSChallengeMandated, res.ACSURL = "Y", s.cfg.PublicURL+"/acs/challenge"
		s.mu.Lock()
		s.txs[tx.acsTransID] = tx
		s.mu.Unlock()
	case notAuthenticated, rejected:
		res.TransStatusReason = "01"
	case unavailable:
		res.TransStatusReason = "08"
	}
	writeJSON(w, http.StatusOK, res)
}

func check(a threeds.AReq) string {
	switch {
	case a.MessageType != threeds.TypeAReq || a.MessageVersion != threeds.Version:
		return "Only AReq messages of version " + threeds.Version + " are accepted."
	case a.ThreeDSServerTransID == "" || a.ThreeDSServerURL == "" || a.NotificationURL == "":
		return "threeDSServerTransID, threeDSServerURL and notificationURL are required."
	case len(a.AcctNumber) < 12 || len(a.AcctNumber) > 19:
		return "acctNumber is not a card number."
	case a.PurchaseCurrency != "986" || a.PurchaseExponent != "2":
		return "Only purchases in BRL (986, exponent 2) are accepted."
	}
	if _, err := strconv.ParseInt(a.PurchaseAmount, 10, 64); err != nil {
		return "purchaseAmount must be digits."
	}
	return ""
}

// authenticate returns the ECI and authentication value of a successful authentication,
// or of an attempt: Mastercard's ECIs are 02 and 01, the others' 05 and 06.
func (s *Server) authenticate(a threeds.AReq, dsTransID string, b behaviour) (string, string) {
	amount, _ := strconv.ParseInt(a.PurchaseAmount, 10, 64)
	eci := map[behaviour]string{frictionless: "05", attempted: "06"}[b]
	if a.AcctNumber[0] == '5' || a.AcctNumber[0] == '2' {
		eci = map[behaviour]string{frictionless: "02", attempted: "01"}[b]
	}
	return eci, threeds.AuthenticationValue(s.cfg.AuthenticationKey, a.AcctNumber, amount, dsTransID)
}

func (s *Server) fail(w http.ResponseWriter, code, description string) {
	writeJSON(w, http.StatusOK, threeds.Error{
		MessageType: threeds.TypeError, MessageVersion: threeds.Version, ErrorCode: code,
		ErrorComponent: "D", ErrorDescription: description,
	})
}

var challengePage = template.Must(template.New("challenge").Parse(`<!doctype html>
<html lang="pt-BR"><head><meta charset="utf-8"><title>Autenticação do cartão</title></head>
<body>
<h1>Confirme a compra</h1>
<p>Compra de R$ {{.Amount}} em {{.Merchant}} com o cartão final {{.Last4}}.</p>
<p>Digite o código enviado ao seu celular.</p>
<form method="post" action="/acs/challenge/complete">
<input type="hidden" name="acsTransID" value="{{.ACSTransID}}">
<input type="hidden" name="threeDSSessionData" value="{{.SessionData}}">
<input name="otp" autocomplete="one-time-code" inputmode="numeric">
<button type="submit">Confirmar</button>
</form>
</body></html>`))

func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	var c threeds.CReq
	if err := threeds.DecodeForm(r.PostFormValue("creq"), &c); err != nil || c.MessageType != threeds.TypeCReq {
		http.Error(w, "invalid creq", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	tx, ok := s.txs[c.ACSTransID]
	s.mu.Unlock()
	if !ok || tx.areq.ThreeDSServerTransID != c.ThreeDSServerTransID || tx.done {
		http.Error(w, "unknown or finished challenge", http.StatusNotFound)
		return
	}
	amount, _ := strconv.ParseInt(tx.areq.PurchaseAmount, 10, 64)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = challengePage.Execute(w, map[string]string{
		"Amount": fmt.Sprintf("%d,%02d", amount/100, amount%100), "Merchant": tx.areq.MerchantName,
		"Last4": tx.areq.AcctNumber[len(tx.areq.AcctNumber)-4:], "ACSTransID": tx.acsTransID,
		"SessionData": r.PostFormValue("threeDSSessionData"),
	})
}

var notifyPage = template.Must(template.New("notify").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Voltando à loja</title></head>
<body onload="document.forms[0].submit()">
<form method="post" action="{{.URL}}">
<input type="hidden" name="cres" value="{{.CRes}}">
<input type="hidden" name="threeDSSessionData" value="{{.SessionData}}">
<noscript><button type="submit">Continuar</button></noscript>
</form>
</body></html>`))

// complete checks the code, reports the result to the 3DS server through the directory
// server (RReq), and sends the browser back to the notification URL with the CRes.
func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	tx, ok := s.txs[r.PostFormValue("acsTransID")]
	if ok && !tx.done {
		tx.done = true
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown challenge", http.StatusNotFound)
		return
	}
	status := threeds.StatusNotAuthenticated
	rreq := threeds.RReq{
		MessageType: threeds.TypeRReq, MessageVersion: threeds.Version, ThreeDSServerTransID: tx.areq.ThreeDSServerTransID,
		DSTransID: tx.dsTransID, ACSTransID: tx.acsTransID, InteractionCounter: "01",
	}
	if r.PostFormValue("otp") == ChallengeCode {
		status = threeds.StatusAuthenticated
		rreq.ECI, rreq.AuthenticationValue = s.authenticate(tx.areq, tx.dsTransID, frictionless)
	}
	rreq.TransStatus = status
	if err := s.results(r.Context(), tx.areq.ThreeDSServerURL, rreq); err != nil {
		s.cfg.Logger.WarnContext(r.Context(), "the 3DS server did not take the results", "error", err)
	}
	cres, err := threeds.EncodeForm(threeds.CRes{
		MessageType: threeds.TypeCRes, MessageVersion: threeds.Version, ThreeDSServerTransID: tx.areq.ThreeDSServerTransID,
		ACSTransID: tx.acsTransID, TransStatus: status, ChallengeCompletionInd: "Y",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = notifyPage.Execute(w, map[string]string{"URL": tx.areq.NotificationURL, "CRes": cres, "SessionData": r.PostFormValue("threeDSSessionData")})
}

func (s *Server) results(ctx context.Context, url string, rreq threeds.RReq) error {
	body, err := json.Marshal(rreq)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(threeds.SignatureHeader, threeds.Sign(body, s.cfg.ResultsSecret, s.cfg.Now()))
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var rres threeds.RRes
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&rres); err != nil || rres.MessageType != threeds.TypeRRes {
		return fmt.Errorf("the 3DS server answered %d without an RRes", resp.StatusCode)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
