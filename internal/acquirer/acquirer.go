// Package acquirer is Jupiter's acquirer connector: it implements the payments rail over
// ISO 8583 (pkg/cardnet) to the card network. Every message is recorded before it is
// sent; a request the network does not answer in time is reversed, an advice it does not
// acknowledge is repeated, and both until the network confirms (ADR 0019).
package acquirer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moov-io/iso8583"
	connection "github.com/moov-io/iso8583-connection"

	"github.com/iricardofernandes/jupiter/internal/acquirer/db"
	"github.com/iricardofernandes/jupiter/internal/acquirer/migrations"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

const (
	defaultAcquirerID = "10000000001"
	defaultTerminalID = "JUPITER1"
	defaultTimeout    = 10 * time.Second
	// A merchant category code of 5999, miscellaneous retail, until merchants have their own.
	defaultMCC = "5999"
	echoEvery  = 30 * time.Second
)

type Config struct {
	Pool *pgxpool.Pool
	// Addr is the network's ISO 8583 address, host:port.
	Addr string
	// NetworkURL is the network's HTTP base: its clearing files and its token service.
	NetworkURL string
	// EventsSecret checks the signature on the network's token events.
	EventsSecret string
	// Tokens, if set, lets the connector provision network tokens for live cards and
	// keep them in the vault.
	Tokens     TokenVault
	AcquirerID string
	TerminalID string
	// Timeout is how long to wait for the network's answer before a request is reversed
	// or an advice repeated.
	Timeout    time.Duration
	Cards      payments.CardSource
	Now        func() time.Time
	Logger     *slog.Logger
	HTTPClient *http.Client
}

type Connector struct {
	cfg  Config
	conn *connection.Pool
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "acquirer", migrations.FS)
}

func New(cfg Config) (*Connector, error) {
	if cfg.Pool == nil || cfg.Addr == "" || cfg.Cards == nil {
		return nil, errors.New("acquirer: a database pool, the network's address and a card source are required")
	}
	if cfg.AcquirerID == "" {
		cfg.AcquirerID = defaultAcquirerID
	}
	if cfg.TerminalID == "" {
		cfg.TerminalID = defaultTerminalID
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if strings.Trim(cfg.AcquirerID, "0123456789") != "" || len(cfg.AcquirerID) > 11 {
		return nil, errors.New("acquirer: the acquirer id must be up to 11 digits")
	}
	if cfg.NetworkURL != "" {
		if err := checkNetworkURL(cfg.NetworkURL); err != nil {
			return nil, err
		}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout: 30 * time.Second,
			// A clearing file comes from the address configured, not from wherever that
			// address points to.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	c := &Connector{cfg: cfg}
	pool, err := connection.NewPool(c.dial, []string{cfg.Addr},
		connection.PoolMinConnections(0),
		connection.PoolReconnectWait(time.Second),
		connection.PoolMaxReconnectWait(30*time.Second),
		connection.PoolErrorHandler(func(err error) { cfg.Logger.Warn("card network connection", "error", err) }),
	)
	if err != nil {
		return nil, fmt.Errorf("acquirer: %w", err)
	}
	c.conn = pool
	return c, nil
}

// Connect opens the connection to the network and signs on. With the network down it
// keeps trying in the background, and payments are declined until it is up.
func (c *Connector) Connect(ctx context.Context) error {
	return c.conn.ConnectCtx(ctx)
}

func (c *Connector) Close() error {
	return c.conn.Close()
}

func (c *Connector) dial(addr string) (*connection.Connection, error) {
	return connection.New(addr, cardnet.Spec, cardnet.ReadLength, cardnet.WriteLength,
		connection.SendTimeout(c.cfg.Timeout),
		connection.ConnectTimeout(5*time.Second),
		connection.IdleTime(echoEvery),
		connection.PingHandler(func(conn *connection.Connection) {
			if _, err := c.sendOn(conn, c.network(cardnet.Echo)); err != nil {
				c.cfg.Logger.Warn("card network echo failed", "error", err)
			}
		}),
		connection.OnConnect(func(conn *connection.Connection) error {
			resp, err := c.sendOn(conn, c.network(cardnet.SignOn))
			if err != nil {
				return fmt.Errorf("signing on: %w", err)
			}
			if resp.ResponseCode != cardnet.Approved {
				return fmt.Errorf("signing on: response code %s", resp.ResponseCode)
			}
			return nil
		}),
		connection.InboundMessageHandler(c.inbound),
		connection.ErrorHandler(func(err error) { c.cfg.Logger.Warn("card network connection", "error", err) }),
	)
}

// network builds a network management message. Its STAN comes from the same sequence
// as every other, so it can never be mistaken for a payment's answer.
func (c *Connector) network(code string) cardnet.Message {
	stan, err := db.New(c.cfg.Pool).NextSTAN(context.Background())
	if err != nil {
		c.cfg.Logger.Warn("drawing a STAN for a network message", "error", err)
	}
	return cardnet.Message{
		MTI: cardnet.NetworkRequest, TransmissionDateTime: cardnet.TransmissionTime(c.cfg.Now()),
		STAN: stan, AcquirerID: c.cfg.AcquirerID, NetworkCode: code,
	}
}

func (c *Connector) sendOn(conn *connection.Connection, m cardnet.Message) (cardnet.Message, error) {
	msg, err := cardnet.Pack(m)
	if err != nil {
		return cardnet.Message{}, err
	}
	resp, err := conn.Send(msg)
	if err != nil {
		return cardnet.Message{}, err
	}
	return cardnet.Unpack(resp)
}

// errNotSent says a message never left: no connection to the network.
var errNotSent = errors.New("acquirer: not connected to the card network")

// send sends m and waits for its answer. Only errNotSent means the message never left:
// a connection that closes afterwards may have closed after the network processed it.
func (c *Connector) send(m cardnet.Message) (cardnet.Message, error) {
	conn, err := c.conn.Get()
	if err != nil {
		return cardnet.Message{}, fmt.Errorf("%w: %w", errNotSent, err)
	}
	resp, err := c.sendOn(conn, m)
	if err != nil {
		return cardnet.Message{}, err
	}
	if resp.MTI != cardnet.ResponseTo(m.MTI) || resp.STAN != m.STAN || (m.RRN != "" && resp.RRN != m.RRN) {
		return cardnet.Message{}, fmt.Errorf("acquirer: the answer %s does not match the message %s", resp, m)
	}
	return resp, nil
}

// inbound handles what the network sends unasked: echoes, and answers that arrive after
// their request gave up on them.
func (c *Connector) inbound(conn *connection.Connection, raw *iso8583.Message) {
	m, err := cardnet.Unpack(raw)
	if err != nil {
		c.cfg.Logger.Warn("card network sent an unreadable message", "error", err)
		return
	}
	if m.MTI == cardnet.NetworkRequest {
		resp := m
		resp.MTI, resp.ResponseCode = cardnet.NetworkResponse, cardnet.Approved
		if msg, err := cardnet.Pack(resp); err == nil {
			_ = conn.Reply(msg)
		}
		return
	}
	if err := c.late(context.Background(), m); err != nil {
		c.cfg.Logger.Error("recording a late answer from the card network", "message", m, "error", err)
	}
}

// late records an answer that came after its sender stopped waiting. A late approval of
// a reversed authorization changes nothing: the reversal stands. A late acknowledgement
// of a repeated advice or reversal ends its repetition.
func (c *Connector) late(ctx context.Context, m cardnet.Message) error {
	ex, err := db.New(c.cfg.Pool).ExchangeByMessage(ctx, db.ExchangeByMessageParams{Stan: m.STAN, Rrn: m.RRN})
	if errors.Is(err, pgx.ErrNoRows) {
		c.cfg.Logger.WarnContext(ctx, "card network answered a message Jupiter did not send", "message", m)
		return nil
	}
	if err != nil {
		return err
	}
	return c.update(ctx, ex.Key, func(ex *db.AcquirerExchange) bool {
		forwarded := ex.ForwardStan.Valid && ex.ForwardStan.String == m.STAN
		switch {
		case forwarded && ex.State == stateReversing && m.MTI == cardnet.ResponseTo(cardnet.ReversalAdvice):
			ex.State = stateReversed
			if m.ResponseCode != cardnet.Approved {
				ex.State, ex.ResponseCode, ex.DeclineCode = stateDeclined, m.ResponseCode, "reversal_refused"
			}
			stopForwarding(ex)
		case forwarded && ex.State == stateAdvising && m.MTI == cardnet.CompletionResponse:
			ex.State, ex.ResponseCode = stateAcknowledged, m.ResponseCode
			if m.ResponseCode != cardnet.Approved {
				ex.State, ex.DeclineCode = stateDeclined, declineCode(m.ResponseCode)
			}
			stopForwarding(ex)
		default:
			ex.LateResponseCode = m.ResponseCode
			c.cfg.Logger.InfoContext(ctx, "card network answered late; the answer is recorded and ignored", "message", m, "state", ex.State)
		}
		return true
	})
}

// update changes an exchange under its row lock.
func (c *Connector) update(ctx context.Context, key string, change func(*db.AcquirerExchange) bool) error {
	return postgres.InTx(ctx, c.cfg.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		ex, err := q.LockExchange(ctx, key)
		if err != nil {
			return err
		}
		if !change(&ex) {
			return nil
		}
		return save(ctx, q, ex, c.cfg.Now())
	})
}

func save(ctx context.Context, q *db.Queries, ex db.AcquirerExchange, now time.Time) error {
	return q.SaveExchange(ctx, db.SaveExchangeParams{
		Key: ex.Key, State: ex.State, ResponseCode: ex.ResponseCode, AuthorizationCode: ex.AuthorizationCode,
		NetworkTransactionID: ex.NetworkTransactionID, DeclineCode: ex.DeclineCode, LateResponseCode: ex.LateResponseCode,
		ForwardStan: ex.ForwardStan, ForwardAttempts: ex.ForwardAttempts, NextForwardAt: ex.NextForwardAt,
		Now: timestamptz(now),
	})
}

// merchantCode is DE 42 for a merchant: fifteen characters that name it without its id.
func merchantCode(merchant string) string {
	sum := sha256.Sum256([]byte(merchant))
	return "M" + strings.ToUpper(hex.EncodeToString(sum[:]))[:14]
}

// checkNetworkURL accepts https, or http to this machine for the simulator.
func checkNetworkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("acquirer: the clearing URL %q is not an absolute URL", raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("acquirer: the clearing URL must be https, or http to this machine, not %q", raw)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// FromEnv builds and connects the connector from JUPITER_CARDNET_ADDR (the network's
// ISO 8583 address), JUPITER_CARDNET_URL and JUPITER_ACQUIRER_ID. Without an
// address it returns nil: live mode then has no rail.
func FromEnv(ctx context.Context, getenv func(string) string, pool *pgxpool.Pool, cards payments.CardSource, logger *slog.Logger) (*Connector, error) {
	addr := getenv("JUPITER_CARDNET_ADDR")
	if addr == "" {
		return nil, nil //nolint:nilnil // no network configured is not an error
	}
	// The link carries card numbers in plain TCP: to anywhere but this machine it must be
	// a private circuit, which only the operator can vouch for.
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("acquirer: JUPITER_CARDNET_ADDR: %w", err)
	}
	if !isLoopback(host) && getenv("JUPITER_CARDNET_PRIVATE_LINK") != "true" {
		return nil, fmt.Errorf("acquirer: %s is not on this machine; set JUPITER_CARDNET_PRIVATE_LINK=true only over a private circuit", addr)
	}
	cfg := Config{
		Pool: pool, Addr: addr, NetworkURL: getenv("JUPITER_CARDNET_URL"), EventsSecret: getenv("JUPITER_CARDNET_EVENTS_SECRET"),
		AcquirerID: getenv("JUPITER_ACQUIRER_ID"), Cards: cards, Logger: logger,
	}
	if tokens, ok := cards.(TokenVault); ok {
		cfg.Tokens = tokens
	}
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Connect(ctx); err != nil {
		return nil, fmt.Errorf("acquirer: connecting to %s: %w", addr, err)
	}
	return c, nil
}
