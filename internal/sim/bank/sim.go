// Package bank simulates the bank that holds Jupiter's account for boletos and transfers:
// it takes CNAB 240 collection remittances, registers boletos (hybrid ones with a Pix QR
// code), lets payers pay them by their typed line or by Pix, writes them off past their
// term, and answers in CNAB 240 return files; it makes transfers out of the account, some
// of which come back; and it gives account statements. It stands for another company:
// nothing in Jupiter imports it, and Jupiter reaches it only over HTTP. The README says
// what is simulated and how faithfully.
package bank

import (
	"crypto/rand"
	"encoding/base32"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/cnab240"
)

// brasilia is the time zone of due dates and business days.
var brasilia = time.FixedZone("BRT", -3*60*60)

// Code is the simulated bank's code: not one any real bank has.
const Code = "999"

// Client is an account holder: Jupiter, with its collection agreement.
type Client struct {
	Token     string
	TaxID     string
	Name      string
	Agreement string
	Account   cnab240.Account
}

type Config struct {
	Now    func() time.Time
	Logger *slog.Logger
	// Host is where hybrid boletos' Pix QR codes point, as host[:port].
	Host    string
	Clients []Client
}

type Sim struct {
	cfg     Config
	profile cnab240.Febraban
	byToken map[string]*client

	mu sync.Mutex
}

type client struct {
	Client
	titles      map[string]*title // by nosso número
	byLocation  map[string]*title // by Pix location token
	remittances map[int64][]byte  // by NSA
	pending     []cnab240.ReturnTitle
	returns     []returnFile
	transfers   map[string]*transfer
	statement   []Entry
}

type returnFile struct {
	sequence int64
	created  time.Time
	data     []byte
}

// Entry is a line of an account statement.
type Entry struct {
	Date        string `json:"date"`
	Kind        string `json:"kind"` // credit or debit
	Amount      int64  `json:"amount"`
	Reference   string `json:"reference"`
	Description string `json:"description"`
}

func New(cfg Config) *Sim {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Sim{cfg: cfg, profile: cnab240.Febraban{BankCode: Code, BankName: "BANCO SIMULADO"}, byToken: map[string]*client{}}
	for _, c := range cfg.Clients {
		s.byToken[c.Token] = &client{
			Client: c, titles: map[string]*title{}, byLocation: map[string]*title{}, remittances: map[int64][]byte{},
			transfers: map[string]*transfer{},
		}
	}
	return s
}

func (s *Sim) today() time.Time {
	y, m, d := s.cfg.Now().In(brasilia).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func randomToken() string {
	b := make([]byte, 15)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// Tick lets the bank's day move: titles past their term are written off, transfers made
// and returned, and what happened since the last return file goes into a new one.
func (s *Sim) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.byToken {
		s.writeOffDue(c)
		s.advanceTransfers(c)
		s.flush(c)
	}
}
