// Package authentication is Jupiter's 3-D Secure server. It implements the payments
// Authenticator: it sends the directory server an authentication request for a card and
// turns the answer into what payments needs. For a challenge it serves the page that takes
// the customer to the issuer, takes the result the directory server reports, and resumes
// the payment (ADR 0023).
package authentication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/authentication/db"
	"github.com/iricardofernandes/jupiter/internal/authentication/migrations"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secureurl"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

const (
	requestTimeout    = 10 * time.Second
	resultsTolerance  = 5 * time.Minute
	resumeTimeout     = time.Minute
	defaultRequestor  = "jupiter"
	defaultMCC        = "5999"
	merchantCountryBR = "076"
)

type Config struct {
	Pool *pgxpool.Pool
	// DirectoryURL is where authentication requests go.
	DirectoryURL string
	// PublicURL is where customers' browsers reach Jupiter, for the challenge pages and
	// the results the directory server sends.
	PublicURL string
	// ResultsSecret checks the directory server's signature on results.
	ResultsSecret string
	Cards         CardReader
	// AllowHTTP accepts ACS addresses over plain HTTP on this machine, for simulators on a
	// developer's; otherwise the customer is only sent to an ACS over HTTPS.
	AllowHTTP   bool
	RequestorID string
	Now         func() time.Time
	Logger      *slog.Logger
	HTTPClient  *http.Client
}

type Server struct {
	cfg      Config
	payments *payments.Service
	resume   func(ctx context.Context, owner payments.Owner, intentID string) error
}

// Attach gives the server what it needs once payments and the API exist, which both need
// the server first: the payments service a challenge's result goes to, and resume, which
// carries a payment on to its authorization after a successful challenge.
func (s *Server) Attach(p *payments.Service, resume func(ctx context.Context, owner payments.Owner, intentID string) error) {
	s.payments, s.resume = p, resume
}

var _ payments.Authenticator = (*Server)(nil)

// CardReader reads a saved card without spending its security code, which stays for the
// authorization.
type CardReader interface {
	ReadCard(ctx context.Context, token, owner string) (vault.CardData, error)
}

// minResultsSecret is the shortest secret the ACS's results are signed with.
const minResultsSecret = 32

func New(cfg Config) (*Server, error) {
	if cfg.Pool == nil || cfg.DirectoryURL == "" || cfg.PublicURL == "" || cfg.Cards == nil || cfg.ResultsSecret == "" {
		return nil, errors.New("authentication: a pool, the directory server's URL, Jupiter's public URL, a card source and the results secret are required")
	}
	// The directory server is sent card numbers, and the public URL is where results and
	// customers come back.
	if err := secureurl.Check(cfg.DirectoryURL); err != nil {
		return nil, fmt.Errorf("authentication: the directory server's URL %w", err)
	}
	if err := secureurl.Check(cfg.PublicURL); err != nil {
		return nil, fmt.Errorf("authentication: Jupiter's public URL %w", err)
	}
	if len(cfg.ResultsSecret) < minResultsSecret {
		return nil, fmt.Errorf("authentication: the results secret must be at least %d bytes", minResultsSecret)
	}
	if cfg.RequestorID == "" {
		cfg.RequestorID = defaultRequestor
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Server{cfg: cfg}, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "authentication", migrations.FS)
}

// Authenticate sends one authentication request per attempt: a repeat answers from the
// first. A request that gets no answer is an error, for payments to try again: going on
// without 3-D Secure because the directory server could not be reached would let anyone
// who can make it unreachable skip the authentication the merchant or the risk engine
// asked for.
func (s *Server) Authenticate(ctx context.Context, r payments.AuthenticationRequest) payments.Authentication {
	q := db.New(s.cfg.Pool)
	if tx, err := q.TransactionForAttempt(ctx, r.Key); err == nil {
		return s.authentication(tx)
	}
	if r.Card == nil {
		return payments.Authentication{Outcome: payments.AuthenticationUnavailable}
	}
	card, err := s.cfg.Cards.ReadCard(ctx, r.Card.Token, r.Card.Owner)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "reading a card for 3-D Secure", "attempt", r.Key, "error", err)
		return payments.Authentication{Outcome: payments.AuthenticationError}
	}
	serverTransID := uuid.NewV4().String()
	now := s.cfg.Now().UTC()
	areq := threeds.AReq{
		MessageType: threeds.TypeAReq, MessageVersion: threeds.Version, MessageCategory: "01", DeviceChannel: "02",
		ThreeDSServerTransID: serverTransID, ThreeDSServerURL: s.cfg.PublicURL + resultsPath,
		ThreeDSRequestorID: s.cfg.RequestorID, ThreeDSRequestorName: "Jupiter", ThreeDSRequestorAuthenticationInd: "01",
		AcquirerBIN: "400000", AcquirerMerchantID: r.Owner.Merchant.String(), MerchantName: r.Owner.Merchant.String(),
		MCC: defaultMCC, MerchantCountryCode: merchantCountryBR, AcctNumber: card.Number,
		CardExpiryDate: fmt.Sprintf("%02d%02d", card.ExpYear%100, card.ExpMonth),
		PurchaseAmount: strconv.FormatInt(r.Amount.Minor(), 10), PurchaseCurrency: "986", PurchaseExponent: "2",
		PurchaseDate: now.Format("20060102150405"), TransType: "01",
		NotificationURL: s.cfg.PublicURL + notifyPath + serverTransID,
	}
	ares, err := s.send(ctx, areq)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "the directory server did not authenticate", "attempt", r.Key, "error", err)
		return payments.Authentication{Outcome: payments.AuthenticationError}
	}
	row, err := q.InsertTransaction(ctx, db.InsertTransactionParams{
		ServerTransID: serverTransID, AttemptID: r.Key, IntentID: r.Intent, MerchantID: r.Owner.Merchant.String(),
		Livemode: r.Owner.Livemode, TransStatus: ares.TransStatus, DsTransID: ares.DSTransID, AcsTransID: ares.ACSTransID,
		AcsUrl: ares.ACSURL, Eci: ares.ECI, AuthenticationValue: ares.AuthenticationValue, ReturnUrl: r.ReturnURL,
		Now: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Another run of the same attempt recorded its answer first.
		if row, err = q.TransactionForAttempt(ctx, r.Key); err == nil {
			return s.authentication(row)
		}
	}
	if err != nil {
		s.cfg.Logger.ErrorContext(ctx, "recording a 3-D Secure transaction", "attempt", r.Key, "error", err)
		return payments.Authentication{Outcome: payments.AuthenticationError}
	}
	return s.authentication(row)
}

func (s *Server) send(ctx context.Context, areq threeds.AReq) (threeds.ARes, error) {
	body, err := json.Marshal(areq)
	if err != nil {
		return threeds.ARes{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.DirectoryURL, bytes.NewReader(body))
	if err != nil {
		return threeds.ARes{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return threeds.ARes{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return threeds.ARes{}, err
	}
	var ares threeds.ARes
	if err := json.Unmarshal(raw, &ares); err != nil {
		return threeds.ARes{}, fmt.Errorf("an answer that is not JSON (%d)", resp.StatusCode)
	}
	switch {
	case ares.MessageType == threeds.TypeError:
		var e threeds.Error
		_ = json.Unmarshal(raw, &e)
		return threeds.ARes{}, fmt.Errorf("error %.8s: %.200q", e.ErrorCode, e.ErrorDescription)
	case ares.MessageType != threeds.TypeARes || ares.ThreeDSServerTransID != areq.ThreeDSServerTransID:
		return threeds.ARes{}, fmt.Errorf("an answer that is not the ARes to %s", areq.ThreeDSServerTransID)
	case ares.TransStatus == threeds.StatusChallenge && !s.acceptableACS(ares.ACSURL):
		return threeds.ARes{}, errors.New("a challenge without an acceptable ACS URL")
	}
	return ares, nil
}

// acceptableACS reports whether the customer's browser may be sent to u.
func (s *Server) acceptableACS(u string) bool {
	acs, err := url.Parse(u)
	if err != nil || acs.Host == "" || acs.User != nil {
		return false
	}
	return acs.Scheme == "https" || (s.cfg.AllowHTTP && acs.Scheme == "http" && secureurl.Loopback(acs.Hostname()))
}

func (s *Server) authentication(tx db.AuthenticationTransaction) payments.Authentication {
	a := payments.Authentication{
		Version: threeds.Version, TransStatus: tx.TransStatus, ServerTransID: tx.ServerTransID, DSTransID: tx.DsTransID,
		ACSTransID: tx.AcsTransID, ECI: tx.Eci, AuthenticationValue: tx.AuthenticationValue,
	}
	switch tx.TransStatus {
	case threeds.StatusAuthenticated:
		a.Outcome = payments.Authenticated
	case threeds.StatusAttempted:
		a.Outcome = payments.Attempted
	case threeds.StatusChallenge:
		a.Outcome, a.ChallengeURL = payments.Challenge, s.cfg.PublicURL+challengePath+tx.ServerTransID
	case threeds.StatusNotAuthenticated:
		a.Outcome = payments.NotAuthenticated
	case threeds.StatusRejected:
		a.Outcome = payments.Rejected
	default:
		a.Outcome = payments.AuthenticationUnavailable
	}
	return a
}

// Owner reads the merchant a transaction belongs to.
func ownerOf(tx db.AuthenticationTransaction) (payments.Owner, error) {
	merchant, err := id.Parse(tx.MerchantID)
	return payments.Owner{Merchant: merchant, Livemode: tx.Livemode}, err
}

// FromEnv builds the 3DS server from JUPITER_3DS_DIRECTORY_URL, JUPITER_PUBLIC_URL,
// JUPITER_3DS_RESULTS_SECRET and JUPITER_3DS_ALLOW_HTTP. Without a directory server it
// returns nil: live payments then go without 3-D Secure.
func FromEnv(getenv func(string) string, pool *pgxpool.Pool, cards CardReader, logger *slog.Logger) (*Server, error) {
	directory := getenv("JUPITER_3DS_DIRECTORY_URL")
	if directory == "" {
		return nil, nil //nolint:nilnil // no directory server configured is not an error
	}
	cfg := Config{
		Pool: pool, DirectoryURL: directory, PublicURL: getenv("JUPITER_PUBLIC_URL"),
		ResultsSecret: getenv("JUPITER_3DS_RESULTS_SECRET"), Cards: cards, Logger: logger,
		AllowHTTP: getenv("JUPITER_3DS_ALLOW_HTTP") == "true",
	}
	// A directory server that knows Jupiter by its certificate gets it.
	if cert := getenv("JUPITER_3DS_CLIENT_CERT"); cert != "" {
		tlsConfig, err := mtls.LoadClientConfig(cert, getenv("JUPITER_3DS_CLIENT_KEY"), getenv("JUPITER_3DS_CA"))
		if err != nil {
			return nil, fmt.Errorf("JUPITER_3DS_CLIENT_CERT: %w", err)
		}
		cfg.HTTPClient = &http.Client{
			Timeout:       requestTimeout,
			Transport:     &http.Transport{TLSClientConfig: tlsConfig},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return New(cfg)
}
