// Package pix simulates the Pix side of the world Jupiter deals with: the partner bank
// that holds Jupiter's account and offers it the Banco Central's API Pix, the SPI that
// settles between participants, the DICT that resolves keys, and every payer. It stands
// for other companies: nothing in Jupiter imports it, and Jupiter reaches it only over
// HTTPS with mutual TLS, as it would a real bank. The README says what is simulated and
// how faithfully.
package pix

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// Participants' ISPBs. The simulated bank is Jupiter's; payers bank elsewhere.
const (
	DefaultISPB = "30000001"
	PayerISPB   = "30000002"
)

// brasilia is the time zone of due dates and payment dates. Brazil has kept no summer
// time since 2019.
var brasilia = time.FixedZone("BRT", -3*60*60)

// Client is a user of the API Pix at the simulated bank: an account, the keys registered
// to it, and the credentials it gets tokens with.
type Client struct {
	ID     string
	Secret string
	// Name and TaxID identify the account holder, as DICT answers for its keys.
	Name  string
	TaxID string
	Keys  []string
	// Branch and Account are the client's account, where Pix Automático is paid (it
	// uses no keys); the client's id and 0001 unless set.
	Branch  string
	Account string
	// Balance is what the account holds to start with, in centavos: what pays for
	// returns and transfers before any Pix arrives.
	Balance int64
}

type Config struct {
	Now    func() time.Time
	Logger *slog.Logger
	ISPB   string
	// Host is where payload locations are served, as host[:port], without a scheme:
	// payers fetch them over HTTPS.
	Host    string
	Clients []Client
	// WebhookTLS is the client configuration notifications are sent with: the bank's
	// certificate, and the CAs of the receivers it trusts.
	WebhookTLS *tls.Config
	// PayerTLS is how payers reach payload locations: the CAs they trust.
	PayerTLS *tls.Config
	// Faults, if set, is asked before each notification, transfer and return.
	Faults   func(Event) Fault
	TokenTTL time.Duration
}

// Event is something the simulator is about to do that a test may want to disturb.
type Event struct {
	Kind   string // "charge", "webhook", "transfer" or "return"
	Client string
	TxID   string
	E2EID  string
	ID     string
}

type Fault struct {
	// Drop loses a notification.
	Drop bool
	// Pending leaves a transfer or return in processing for PendingFor.
	Pending    bool
	PendingFor time.Duration
	// LoseResponse carries out a transfer, or creates a charge, and drops the connection
	// before answering.
	LoseResponse bool
}

const (
	defaultTokenTTL = time.Hour
	notifyTimeout   = 10 * time.Second
)

type Sim struct {
	cfg     Config
	signing *rsa.PrivateKey
	keyID   string
	// notifier sends notifications, payers fetch payload locations.
	notifier *http.Client
	payers   *http.Client

	mu        sync.Mutex
	clients   map[string]*account
	tokens    map[string]token
	dict      map[string]Entry
	charges   map[string]*charge // by client + "/" + txid
	locations map[string]*charge // by url access token
	nextLoc   int64
	pix       map[string]*received // by endToEndId
	transfers map[string]*transfer // by client + "/" + idEnvio
	// closedPayers are payers whose accounts were closed: returns to them fail.
	closedPayers map[string]bool
	// payerFunds are the balances of payers whose funds are limited, by tax id; a payer
	// not here always has enough.
	payerFunds map[string]int64
	recs       map[string]*recurrence // by idRec
	recLocs    map[int64]*recLocation
	recTokens  map[string]*recLocation
	nextRecLoc int64
	solicRecs  map[string]*solicRec
	cobrs      map[string]*recurringCharge // by client + "/" + txid
	delivered  []Delivery
	background sync.WaitGroup
}

type account struct {
	Client
	balance  int64
	webhooks map[string]string // key -> URL
	// recWebhook and cobrWebhook receive the notifications of Pix Automático.
	recWebhook  string
	cobrWebhook string
}

func New(cfg Config) (*Sim, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.ISPB == "" {
		cfg.ISPB = DefaultISPB
	}
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = defaultTokenTTL
	}
	if cfg.Host == "" {
		return nil, errors.New("pix sim: the host payload locations are served at is required")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	noRedirects := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := &Sim{
		cfg: cfg, signing: key, keyID: randomAlnum(16),
		notifier: &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.WebhookTLS}, CheckRedirect: noRedirects},
		payers:   &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.PayerTLS}, CheckRedirect: noRedirects},
		clients:  map[string]*account{}, tokens: map[string]token{}, dict: map[string]Entry{},
		charges: map[string]*charge{}, locations: map[string]*charge{}, pix: map[string]*received{},
		transfers: map[string]*transfer{}, closedPayers: map[string]bool{},
		payerFunds: map[string]int64{}, recs: map[string]*recurrence{}, recLocs: map[int64]*recLocation{},
		recTokens: map[string]*recLocation{}, solicRecs: map[string]*solicRec{}, cobrs: map[string]*recurringCharge{},
	}
	for _, c := range cfg.Clients {
		if c.Branch == "" {
			c.Branch = "0001"
		}
		if c.Account == "" {
			c.Account = c.ID
		}
		s.clients[c.ID] = &account{Client: c, balance: c.Balance, webhooks: map[string]string{}}
		for _, k := range c.Keys {
			s.dict[k] = Entry{Key: k, ISPB: cfg.ISPB, Branch: c.Branch, Account: c.ID, Name: c.Name, TaxID: c.TaxID}
		}
	}
	return s, nil
}

// Close waits for the returns still settling.
func (s *Sim) Close() { s.background.Wait() }

func (s *Sim) now() time.Time { return s.cfg.Now().UTC() }

func (s *Sim) fault(e Event) Fault {
	if s.cfg.Faults == nil {
		return Fault{}
	}
	return s.cfg.Faults(e)
}

// endToEndID makes an id as the Manual de Padrões describes it: E, the ISPB of the
// participant that made it, the minute in UTC, and 11 letters and digits.
func endToEndID(prefix, ispb string, at time.Time) string {
	return prefix + ispb + at.UTC().Format("200601021504") + randomAlnum(11)
}

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func randomAlnum(n int) string {
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alnum))))
		if err != nil {
			panic(err) // crypto/rand does not fail on supported platforms
		}
		b[i] = alnum[k.Int64()]
	}
	return string(b)
}

func (s *Sim) logWarn(ctx context.Context, msg string, args ...any) {
	s.cfg.Logger.WarnContext(ctx, msg, args...)
}

// Handler serves the API Pix, the token endpoint and the payload locations. It must be
// served over TLS asking for client certificates (TLSConfig): the API and the token
// endpoint require one, payload locations, fetched by payers, do not.
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", s.issueToken)
	s.chargeRoutes(mux)
	s.pixRoutes(mux)
	s.webhookRoutes(mux)
	s.transferRoutes(mux)
	s.recurrenceRoutes(mux)
	s.recurringChargeRoutes(mux)
	s.locationRoutes(mux)
	return mux
}

// ServerTLS is the configuration the Handler needs: the bank's certificate, and the CAs
// whose client certificates the API accepts. A connection without a certificate is let
// in, for payers fetching payload locations; the API refuses it.
func ServerTLS(cert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    clientCAs,
	}
}

func problemf(w http.ResponseWriter, status int, kind, title, format string, args ...any) {
	writeProblem(w, status, kind, title, fmt.Sprintf(format, args...))
}
