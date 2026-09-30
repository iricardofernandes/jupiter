package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
)

const (
	clientTimeout   = 5 * time.Second
	maxResponseSize = 64 << 10
)

// Client calls the vault's internal interface over mTLS. Every failure to get an answer
// wraps ErrUnavailable; the vault's own refusals wrap ErrInvalidCard, ErrNotFound or
// ErrConflict.
type Client struct {
	base string
	http *http.Client
}

func NewClient(baseURL string, tlsConfig *tls.Config) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the documented type of DefaultTransport
	transport.TLSClientConfig = tlsConfig
	return &Client{base: baseURL, http: &http.Client{Transport: transport, Timeout: clientTimeout}}
}

func (c *Client) Tokenize(ctx context.Context, r TokenizeRequest) (Card, error) {
	var card Card
	err := c.call(ctx, http.MethodPost, CardsPath, TokenizeBody{Owner: r.Owner, RequestKey: r.RequestKey, Card: WireCardOf(r.Card)}, &card)
	return card, err
}

// Card describes a token, whoever owns it.
func (c *Client) Card(ctx context.Context, token string) (Card, error) {
	var card Card
	err := c.call(ctx, http.MethodGet, cardPath(token, ""), nil, &card)
	return card, err
}

// Claim gives an unclaimed token to owner. Claiming a token owner already holds
// succeeds; claiming one another owner holds, or one past its claim window, fails with
// ErrNotFound.
func (c *Client) Claim(ctx context.Context, token, owner string) (Card, error) {
	var card Card
	err := c.call(ctx, http.MethodPost, cardPath(token, "/claim"), OwnerBody{Owner: owner}, &card)
	return card, err
}

// Detokenize returns the card in the clear, for the one rail call that needs it. The
// security code comes back the first time only.
func (c *Client) Detokenize(ctx context.Context, token, owner string) (CardData, error) {
	var wire WireCard
	err := c.call(ctx, http.MethodPost, cardPath(token, "/detokenize"), DetokenizeBody{Owner: owner}, &wire)
	return wire.Data(), err
}

// ReadCard returns the card in the clear without its security code, which stays for the
// authorization it is meant for.
func (c *Client) ReadCard(ctx context.Context, token, owner string) (CardData, error) {
	var wire WireCard
	err := c.call(ctx, http.MethodPost, cardPath(token, "/detokenize"), DetokenizeBody{Owner: owner, KeepCVC: true}, &wire)
	wire.CVC = ""
	return wire.Data(), err
}

// StoreNetworkToken keeps the network token issued for a card.
func (c *Client) StoreNetworkToken(ctx context.Context, token, owner string, nt NetworkToken) error {
	var card Card
	return c.call(ctx, http.MethodPost, cardPath(token, "/network_token"), NetworkTokenBody{Owner: owner, NetworkToken: nt}, &card)
}

func cardPath(token, action string) string {
	return CardsPath + "/" + url.PathEscape(token) + action
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("%w: reading the answer: %w", ErrUnavailable, err)
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: undecodable answer: %w", ErrUnavailable, err)
		}
		return nil
	}
	return errorFrom(resp.StatusCode, raw)
}

func errorFrom(status int, raw []byte) error {
	var body ErrorBody
	if err := json.Unmarshal(raw, &body); err != nil || status >= http.StatusInternalServerError {
		return fmt.Errorf("%w: status %d", ErrUnavailable, status)
	}
	switch body.Error.Code {
	case CodeNotFound:
		return ErrNotFound
	case CodeConflict:
		return ErrConflict
	case CodeInvalidRequest, CodeUnauthorized, CodeForbidden, CodeRateLimited:
		return fmt.Errorf("vault refused the request: %s", body.Error.Message)
	default:
		return &CardError{Code: body.Error.Code, Param: body.Error.Param}
	}
}

// ClientFromEnv builds the client Jupiter's API and worker use, from JUPITER_VAULT_URL
// and the files named by JUPITER_VAULT_CLIENT_CERT, JUPITER_VAULT_CLIENT_KEY and
// JUPITER_VAULT_CA.
func ClientFromEnv(getenv func(string) string) (*Client, error) {
	baseURL := getenv("JUPITER_VAULT_URL")
	if !strings.HasPrefix(baseURL, "https://") {
		return nil, errors.New("JUPITER_VAULT_URL must be an https:// URL: card numbers never travel in the clear")
	}
	cfg, err := mtls.LoadClientConfig(getenv("JUPITER_VAULT_CLIENT_CERT"), getenv("JUPITER_VAULT_CLIENT_KEY"), getenv("JUPITER_VAULT_CA"))
	if err != nil {
		return nil, err
	}
	return NewClient(baseURL, cfg), nil
}
