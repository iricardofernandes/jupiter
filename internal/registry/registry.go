// Package registry is Jupiter's connector to a receivables registry, as an accreditor
// (subcredenciador): it sends the registry its units, tells it of settlements, passes on
// merchants' opt-ins, and reads back positions for reconciliation. It speaks the
// simulator's format (pkg/registryapi); the registries' own layouts were not available.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

const (
	callTimeout = 30 * time.Second
	maxResponse = 32 << 20
)

type Config struct {
	BaseURL string
	// Token is Jupiter's as an accreditor; FinancierToken, as the financier of the units it
	// anticipates.
	Token          string
	FinancierToken string
	HTTP           *http.Client
}

// Registries connects to the registry in each mode that has one: live mode with
// JUPITER_REGISTRY_URL, JUPITER_REGISTRY_TOKEN and JUPITER_REGISTRY_FINANCIER_TOKEN, test
// mode with the same under JUPITER_REGISTRY_TEST_. A mode without a URL has no registry.
func Registries(getenv func(string) string) (live, test *Connector, err error) {
	for _, m := range []struct {
		prefix string
		into   **Connector
	}{{"JUPITER_REGISTRY_", &live}, {"JUPITER_REGISTRY_TEST_", &test}} {
		if m.prefix == "JUPITER_REGISTRY_TEST_" && getenv(m.prefix+"URL") != "" && getenv(m.prefix+"URL") == getenv("JUPITER_REGISTRY_URL") {
			// A merchant's units in both modes share their keys.
			return nil, nil, errors.New("JUPITER_REGISTRY_TEST_URL must not be live mode's registry")
		}
		base := getenv(m.prefix + "URL")
		if base == "" {
			continue
		}
		if getenv(m.prefix+"FINANCIER_TOKEN") == "" || getenv(m.prefix+"FINANCIER_TOKEN") == getenv(m.prefix+"TOKEN") {
			return nil, nil, fmt.Errorf("%s needs a FINANCIER_TOKEN of its own, for anticipations", m.prefix+"URL")
		}
		if !taxid.Valid(getenv("JUPITER_TAX_ID")) {
			return nil, nil, errors.New("JUPITER_TAX_ID must be Jupiter's CNPJ when a registry is configured")
		}
		if *m.into, err = New(Config{BaseURL: base, Token: getenv(m.prefix + "TOKEN"), FinancierToken: getenv(m.prefix + "FINANCIER_TOKEN")}); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", m.prefix+"URL", err)
		}
	}
	return live, test, nil
}

// Domicile is where merchants' units settle, their payment accounts at Jupiter:
// JUPITER_REGISTRY_ISPB and JUPITER_REGISTRY_BRANCH.
func Domicile(getenv func(string) string) registryapi.Domicile {
	return registryapi.Domicile{ISPB: getenv("JUPITER_REGISTRY_ISPB"), Branch: getenv("JUPITER_REGISTRY_BRANCH")}
}

// Configure sets the registries that exist on cfg.
func Configure(cfg *receivables.Config, live, test *Connector) {
	if live != nil {
		cfg.LiveRegistry = live
	}
	if test != nil {
		cfg.TestRegistry = test
	}
}

type Connector struct {
	cfg Config
}

var _ receivables.Registry = (*Connector)(nil)

func New(cfg Config) (*Connector, error) {
	if cfg.BaseURL == "" || cfg.Token == "" {
		return nil, errors.New("registry: a connector needs a URL and a token")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
		return nil, errors.New("registry: the URL must be https, or http on this machine")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: callTimeout}
	}
	// A redirect would carry the token elsewhere.
	client := *cfg.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &client
	return &Connector{cfg: cfg}, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Connector) SetUnits(ctx context.Context, units []registryapi.Unit) ([]registryapi.UnitResult, error) {
	var out registryapi.UnitsResponse
	err := c.call(ctx, http.MethodPut, "/v1/units", registryapi.UnitsRequest{Units: units}, &out)
	return out.Results, err
}

func (c *Connector) Units(ctx context.Context, settled *bool, holder, from, to string) ([]registryapi.Position, error) {
	q := url.Values{}
	if holder != "" {
		q.Set("holder", holder)
	}
	if settled != nil {
		q.Set("settled", strconv.FormatBool(*settled))
	}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	var out registryapi.PositionsResponse
	err := c.call(ctx, http.MethodGet, "/v1/units?"+q.Encode(), nil, &out)
	return out.Units, err
}

func (c *Connector) Instructions(ctx context.Context, holder, arrangement, settlementDate string) ([]registryapi.Payment, error) {
	q := url.Values{"holder": {holder}, "arrangement": {arrangement}, "settlement_date": {settlementDate}}
	var out registryapi.InstructionsResponse
	err := c.call(ctx, http.MethodGet, "/v1/instructions?"+q.Encode(), nil, &out)
	return out.Payments, err
}

func (c *Connector) Settle(ctx context.Context, n registryapi.Settlement) ([]registryapi.Payment, error) {
	var out registryapi.SettlementResponse
	err := c.call(ctx, http.MethodPost, "/v1/settlements", n, &out)
	return out.Payments, err
}

func (c *Connector) SetOptIn(ctx context.Context, o registryapi.OptIn, on bool) error {
	path := "/v1/opt-ins"
	if !on {
		path += "/revoke"
	}
	return c.call(ctx, http.MethodPost, path, o, nil)
}

func (c *Connector) HoldersWithContracts(ctx context.Context) ([]string, error) {
	var out registryapi.HoldersResponse
	err := c.call(ctx, http.MethodGet, "/v1/holders", nil, &out)
	return out.Holders, err
}

// call sends one request. A 404 is ErrRegistryNotFound and another 4xx
// ErrRegistryRefused; anything else that is not a success is an error whose outcome is
// unknown, which every operation here can repeat safely.
// AcceptContract places a contract as Jupiter the financier. A contract the registry
// already has, the same, is placed already.
func (c *Connector) AcceptContract(ctx context.Context, contract registryapi.Contract) error {
	if c.cfg.FinancierToken == "" {
		return errors.New("registry: Jupiter has no financier token at this registry")
	}
	return c.callAs(ctx, c.cfg.FinancierToken, http.MethodPost, "/v1/contracts", contract, nil)
}

func (c *Connector) EndContract(ctx context.Context, contractID string) error {
	if c.cfg.FinancierToken == "" {
		return errors.New("registry: Jupiter has no financier token at this registry")
	}
	err := c.callAs(ctx, c.cfg.FinancierToken, http.MethodPost, "/v1/contracts/"+url.PathEscape(contractID)+"/end", nil, nil)
	if errors.Is(err, receivables.ErrRegistryNotFound) {
		return nil
	}
	return err
}

func (c *Connector) call(ctx context.Context, method, path string, body, out any) error {
	return c.callAs(ctx, c.cfg.Token, method, path, body, out)
}

func (c *Connector) callAs(ctx context.Context, token, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	label, _, _ := strings.Cut(path, "?")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		if uerr := (*url.Error)(nil); errors.As(err, &uerr) {
			err = uerr.Err // it repeats the URL, query included
		}
		return fmt.Errorf("registry: %s %s: %w", method, label, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("registry: %s %s: %w", method, label, err)
	}
	if len(raw) > maxResponse {
		return fmt.Errorf("registry: %s %s: an answer of more than %d bytes", method, label, maxResponse)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("registry: %s %s: an answer that is not the registry's: %w", method, label, err)
		}
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s: %s", receivables.ErrRegistryNotFound, method, label, problem(raw))
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: %s %s: %d %s", receivables.ErrRegistryRefused, method, label, resp.StatusCode, problem(raw))
	default:
		return fmt.Errorf("registry: %s %s: %d %s", method, label, resp.StatusCode, problem(raw))
	}
}

// problem is the registry's explanation of an error, cut short for logs.
func problem(raw []byte) string {
	const most = 200
	var p registryapi.Problem
	if json.Unmarshal(raw, &p) == nil && p.Error != "" {
		raw = []byte(p.Error)
	}
	if len(raw) > most {
		raw = raw[:most]
	}
	return string(raw)
}
