// Package pix is Jupiter's connector to its Pix bank: a client of the Banco Central's
// API Pix (ADR 0025), over mutual TLS with certificate-bound OAuth tokens. It creates the
// charges Pix payments wait on, returns Pix for refunds, sends payouts, and takes the
// bank's notifications of Pix received, each of which it confirms with the bank before
// payments applies it. One connector serves one mode.
package pix

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

type Config struct {
	// BaseURL is the bank's API Pix, such as https://pix.bank.example/api/v2.
	BaseURL      string
	ClientID     string
	ClientSecret string
	// Key is Jupiter's Pix key at the bank, which every charge is paid to.
	Key string
	// Branch and Account are Jupiter's account at the bank, which Pix Automático, using
	// no keys, is paid to.
	Branch  string
	Account string
	// TLS carries Jupiter's client certificate and the CAs the bank's certificate is
	// checked against.
	TLS *tls.Config
	// WebhookURL is where the bank notifies Jupiter: the Handler, served over mutual TLS.
	WebhookURL string
	Livemode   bool
	Pool       *pgxpool.Pool
	Logger     *slog.Logger
	Now        func() time.Time
	Timeout    time.Duration
}

type Connector struct {
	cfg    Config
	http   *http.Client
	tokens tokenCache
}

const (
	defaultTimeout = 10 * time.Second
	maxResponse    = 1 << 20
	tokenMargin    = 30 * time.Second
)

var scopes = strings.Join([]string{
	pixapi.ScopeCobWrite, pixapi.ScopeCobRead, pixapi.ScopeCobVWrite, pixapi.ScopeCobVRead,
	pixapi.ScopePixWrite, pixapi.ScopePixRead, pixapi.ScopeWebhookWrite, pixapi.ScopeWebhookRead,
	pixapi.ScopeRecWrite, pixapi.ScopeRecRead, pixapi.ScopeSolicRecWrite, pixapi.ScopeSolicRecRead,
	pixapi.ScopeCobRWrite, pixapi.ScopeCobRRead, pixapi.ScopeWebhookRecWrite, pixapi.ScopeWebhookCobRWrite,
	pixapi.ScopePayloadLocationRecWrite, pixapi.ScopeInfractionRead, pixapi.ScopeInfractionWrite,
}, " ")

var _ payments.PixRail = (*Connector)(nil)

func New(cfg Config) (*Connector, error) {
	u, err := url.Parse(cfg.BaseURL)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "":
		return nil, errors.New("pix: the bank's API must be an https URL")
	case cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Key == "" || cfg.TLS == nil || len(cfg.TLS.Certificates) == 0:
		return nil, errors.New("pix: a client id and secret, Jupiter's key and a client certificate are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	return &Connector{cfg: cfg, http: &http.Client{
		Timeout:       cfg.Timeout,
		Transport:     &http.Transport{TLSClientConfig: cfg.TLS},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// FromEnv builds a connector from the variables named with prefix (JUPITER_PIX_ for live
// mode, JUPITER_PIX_TEST_ for test mode): URL, CLIENT_ID, CLIENT_SECRET, KEY, BRANCH,
// ACCOUNT, CERT, CERT_KEY, CA and WEBHOOK_URL. Without a URL it returns nil: the mode has
// no Pix.
func FromEnv(getenv func(string) string, prefix string, livemode bool, pool *pgxpool.Pool, logger *slog.Logger) (*Connector, error) {
	base := getenv(prefix + "URL")
	if base == "" {
		return nil, nil //nolint:nilnil // no bank configured is not an error
	}
	tlsConfig, err := mtls.LoadClientConfig(getenv(prefix+"CERT"), getenv(prefix+"CERT_KEY"), getenv(prefix+"CA"))
	if err != nil {
		return nil, fmt.Errorf("%sCERT: %w", prefix, err)
	}
	return New(Config{
		BaseURL: base, ClientID: getenv(prefix + "CLIENT_ID"), ClientSecret: getenv(prefix + "CLIENT_SECRET"),
		Key: getenv(prefix + "KEY"), Branch: getenv(prefix + "BRANCH"), Account: getenv(prefix + "ACCOUNT"), TLS: tlsConfig, WebhookURL: getenv(prefix + "WEBHOOK_URL"), Livemode: livemode,
		Pool: pool, Logger: logger,
	})
}

// Rails returns the connectors configured for live mode (JUPITER_PIX_*) and test mode
// (JUPITER_PIX_TEST_*), either nil when its mode has no Pix.
func Rails(getenv func(string) string, pool *pgxpool.Pool, logger *slog.Logger) (live, test *Connector, err error) {
	if live, err = FromEnv(getenv, "JUPITER_PIX_", true, pool, logger); err != nil {
		return nil, nil, err
	}
	if test, err = FromEnv(getenv, "JUPITER_PIX_TEST_", false, pool, logger); err != nil {
		return nil, nil, err
	}
	if live != nil && test != nil && (live.cfg.Key == test.cfg.Key || live.cfg.ClientID == test.cfg.ClientID ||
		(live.cfg.WebhookURL != "" && live.cfg.WebhookURL == test.cfg.WebhookURL)) {
		// One account for both would give test payments real charges, and one mode's
		// notifications to the other.
		return nil, nil, errors.New("pix: live and test mode must use different keys, clients and webhook URLs")
	}
	return live, test, nil
}

// Configure sets the payments rails of the connectors that exist.
func Configure(cfg *payments.Config, live, test *Connector) {
	if live != nil {
		cfg.LivePix = live
	}
	if test != nil {
		cfg.TestPix = test
	}
}

// tokenCache keeps the access token until shortly before it expires. The bank binds it
// to the certificate it was issued over, which is the connector's own.
type tokenCache struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

func (c *Connector) token(ctx context.Context) (string, error) {
	c.tokens.mu.Lock()
	defer c.tokens.mu.Unlock()
	if c.tokens.value != "" && c.cfg.Now().Add(tokenMargin).Before(c.tokens.expires) {
		return c.tokens.value, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scopes}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("pix: getting a token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out) != nil || out.AccessToken == "" {
		return "", fmt.Errorf("pix: the bank refused a token (%d)", resp.StatusCode)
	}
	c.tokens.value, c.tokens.expires = out.AccessToken, c.cfg.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return c.tokens.value, nil
}

func (c *Connector) forgetToken() {
	c.tokens.mu.Lock()
	c.tokens.value = ""
	c.tokens.mu.Unlock()
}

// call sends a request to the API Pix and decodes a 2xx answer into out. A 404 wraps
// payments.ErrPixNotFound; 400, 410 and 422, the bank judging the request itself, wrap
// payments.ErrPixRefused with its problem. Anything else leaves the outcome unknown: a
// timeout, a 5xx, and the 4xx that say nothing of the request (401, 403, 408, 409, 429).
// A read (GET) is never refused: whatever it answers besides 404 is unknown.
func (c *Connector) call(ctx context.Context, method, path string, body, out any) error {
	// Errors name the path without its query, which may carry a payer's CPF or CNPJ.
	label, _, _ := strings.Cut(path, "?")
	for attempt := 0; ; attempt++ {
		status, raw, err := c.send(ctx, method, path, body)
		if err != nil {
			return err
		}
		switch {
		case status == http.StatusUnauthorized && attempt == 0:
			c.forgetToken() // expired or revoked early: ask for a new one, once
			continue
		case status >= 200 && status < 300:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("pix: %s %s: an answer that is not the API's: %w", method, label, err)
			}
			return nil
		case status == http.StatusNotFound:
			return fmt.Errorf("%w: %s %s: %s", payments.ErrPixNotFound, method, label, problem(raw))
		case method != http.MethodGet && (status == http.StatusBadRequest || status == http.StatusGone || status == http.StatusUnprocessableEntity):
			return fmt.Errorf("%w: %s %s: %d %s", payments.ErrPixRefused, method, label, status, problem(raw))
		default:
			return fmt.Errorf("pix: %s %s: %d %s", method, label, status, problem(raw))
		}
	}
}

func (c *Connector) send(ctx context.Context, method, path string, body any) (int, []byte, error) {
	token, err := c.token(ctx)
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error repeats the whole URL, query included.
		if uerr := (*url.Error)(nil); errors.As(err, &uerr) {
			err = uerr.Err
		}
		return 0, nil, fmt.Errorf("pix: %s %s: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("pix: %s %s: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	return resp.StatusCode, raw, nil
}

// problem is the bank's explanation of an error, cut short for logs.
func problem(raw []byte) string {
	var p pixapi.Problema
	if json.Unmarshal(raw, &p) != nil || p.Title == "" {
		return ""
	}
	detail := p.Title
	if p.Detail != nil {
		detail += " " + *p.Detail
	}
	if r := []rune(detail); len(r) > 200 {
		detail = string(r[:200])
	}
	return taxIDs.ReplaceAllString(detail, "***")
}

// taxIDs are runs of digits as long as a CPF or longer, kept out of logs.
var taxIDs = regexp.MustCompile(`\d{11,}`)
