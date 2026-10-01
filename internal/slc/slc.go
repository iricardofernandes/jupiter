// Package slc is Jupiter's connector to centralized settlement, as a participant: it
// submits each day's grade, reads how it settled, and reports anticipations. It speaks
// the simulator's format (pkg/slcapi); Núclea's own layouts were not available.
package slc

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
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/pkg/slcapi"
)

const (
	callTimeout = 30 * time.Second
	maxResponse = 32 << 20
)

type Config struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Connectors connects to the settlement system in each mode that has one: live mode
// with JUPITER_SLC_URL and JUPITER_SLC_TOKEN, test mode with the same under
// JUPITER_SLC_TEST_.
func Connectors(getenv func(string) string) (live, test *Connector, err error) {
	for _, m := range []struct {
		prefix string
		into   **Connector
	}{{"JUPITER_SLC_", &live}, {"JUPITER_SLC_TEST_", &test}} {
		base := getenv(m.prefix + "URL")
		if base == "" {
			continue
		}
		if *m.into, err = New(Config{BaseURL: base, Token: getenv(m.prefix + "TOKEN")}); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", m.prefix+"URL", err)
		}
	}
	if live != nil && test != nil && (live.cfg.Token == test.cfg.Token || live.cfg.BaseURL == test.cfg.BaseURL) {
		return nil, nil, errors.New("slc: live and test mode must use different settlement systems and tokens")
	}
	return live, test, nil
}

// Configure sets the settlement systems that exist on cfg.
func Configure(cfg *receivables.Config, live, test *Connector) {
	if live != nil {
		cfg.LiveSettlement = live
	}
	if test != nil {
		cfg.TestSettlement = test
	}
}

type Connector struct {
	cfg Config
}

var _ receivables.Settlement = (*Connector)(nil)

func New(cfg Config) (*Connector, error) {
	if cfg.BaseURL == "" || cfg.Token == "" {
		return nil, errors.New("slc: a connector needs a URL and a token")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
		return nil, errors.New("slc: the URL must be https, or http on this machine")
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

func (c *Connector) Submit(ctx context.Context, g slcapi.Grade) (slcapi.Grade, error) {
	var out slcapi.Grade
	err := c.call(ctx, http.MethodPost, "/v1/grades", g, &out)
	return out, err
}

func (c *Connector) Grade(ctx context.Context, date string) (slcapi.Grade, error) {
	var out slcapi.Grade
	err := c.call(ctx, http.MethodGet, "/v1/grades/"+url.PathEscape(date), nil, &out)
	return out, err
}

func (c *Connector) Report(ctx context.Context, reports []slcapi.Report) error {
	return c.call(ctx, http.MethodPost, "/v1/anticipation-reports", slcapi.ReportsRequest{Reports: reports}, nil)
}

func (c *Connector) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.cfg.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		if uerr := (*url.Error)(nil); errors.As(err, &uerr) {
			err = uerr.Err // it repeats the URL
		}
		return fmt.Errorf("slc: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	switch {
	case err != nil:
		return fmt.Errorf("slc: %s %s: %w", method, path, err)
	case len(raw) > maxResponse:
		return fmt.Errorf("slc: %s %s: an answer of more than %d bytes", method, path, maxResponse)
	case resp.StatusCode >= 300:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		err := fmt.Errorf("slc: %s %s: %d %.200s", method, path, resp.StatusCode, e.Error)
		if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnprocessableEntity {
			return fmt.Errorf("%w: %w", receivables.ErrSettlementRefused, err)
		}
		return err
	case out == nil || len(raw) == 0:
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("slc: %s %s: an answer that is not the settlement system's: %w", method, path, err)
	}
	return nil
}
