// Package bank is Jupiter's connector to the bank that collects its boletos and makes its
// transfers. Boletos go to the bank in CNAB 240 remittances and come back, registered,
// paid or written off, in its return files; the connector numbers them, builds their
// barcodes, and tells payments what each return says.
package bank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/bank/db"
	"github.com/iricardofernandes/jupiter/internal/bank/migrations"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/pkg/boleto"
	"github.com/iricardofernandes/jupiter/pkg/brcode"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
)

const (
	callTimeout = 30 * time.Second
	maxResponse = 16 << 20
)

// Config is one mode's connection to the bank.
type Config struct {
	BaseURL  string
	Token    string
	Livemode bool
	// Profile is the bank's way of numbering titles and laying out the barcode.
	Profile cnab240.BankProfile
	// The account boletos are paid into, and the collection agreement.
	Account   cnab240.Account
	Agreement string
	// Jupiter, as the bank knows it.
	TaxID string
	Name  string
	Pool  *pgxpool.Pool
	HTTP  *http.Client
	Now   func() time.Time
	// Logger reports returns about titles Jupiter no longer waits on.
	Logger *slog.Logger
}

type Connector struct {
	cfg Config
}

var _ payments.BoletoRail = (*Connector)(nil)

func New(cfg Config) (*Connector, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || cfg.Token == "" || (u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
		return nil, errors.New("bank: a connector needs a token and an https URL, or http on this machine")
	}
	if cfg.Profile == nil || cfg.Pool == nil {
		return nil, errors.New("bank: a connector needs the bank's profile and a pool")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: callTimeout}
	}
	client := *cfg.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &client
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Connector{cfg: cfg}, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "bank", migrations.FS)
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// Issue numbers a boleto from the mode's sequence, lays out its barcode, and queues it for
// the next remittance. The same attempt again answers the same boleto.
func (c *Connector) Issue(ctx context.Context, b payments.BoletoIssue) (payments.BoletoTitle, error) {
	var out payments.BoletoTitle
	err := postgres.InTx(ctx, c.cfg.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if t, err := q.GetTitle(ctx, b.Attempt); err == nil {
			out = titleOf(t)
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := q.LockSequence(ctx, strconv.FormatBool(c.cfg.Livemode)); err != nil {
			return err
		}
		sequence, err := q.NextSequence(ctx, c.cfg.Livemode)
		if err != nil {
			return err
		}
		ourNumber, err := c.cfg.Profile.OurNumber(sequence)
		if err != nil {
			return err
		}
		free, err := c.cfg.Profile.FreeField(c.cfg.Account, ourNumber)
		if err != nil {
			return err
		}
		factor, err := boleto.Factor(b.Due)
		if err != nil {
			return err
		}
		barcode, err := boleto.Boleto{Bank: c.cfg.Profile.Code(), Currency: boleto.Real, Factor: factor, Amount: b.Amount.Minor(), FreeField: free}.Barcode()
		if err != nil {
			return err
		}
		now := pgtype.Timestamptz{Time: c.cfg.Now().UTC(), Valid: true}
		t := db.BankTitle{
			AttemptID: b.Attempt, Livemode: c.cfg.Livemode, Sequence: sequence, OurNumber: ourNumber, Barcode: string(barcode),
			Line: barcode.Line(), Due: pgtype.Date{Time: b.Due, Valid: true}, Amount: b.Amount.Minor(), PayerName: strings.ToUpper(b.Payer.Name),
			PayerTaxID: b.Payer.TaxID, Hybrid: b.Hybrid, DaysAfterDue: int32(b.DaysAfterDue), //nolint:gosec // 0 to 60
		}
		if err := q.InsertTitle(ctx, db.InsertTitleParams{
			AttemptID: t.AttemptID, Livemode: t.Livemode, Sequence: t.Sequence, OurNumber: t.OurNumber, Barcode: t.Barcode, Line: t.Line,
			Due: t.Due, Amount: t.Amount, PayerName: t.PayerName, PayerTaxID: t.PayerTaxID, Hybrid: t.Hybrid, DaysAfterDue: t.DaysAfterDue, Now: now,
		}); err != nil {
			return err
		}
		out = titleOf(t)
		return nil
	})
	return out, err
}

func titleOf(t db.BankTitle) payments.BoletoTitle {
	return payments.BoletoTitle{OurNumber: t.OurNumber, Barcode: t.Barcode, Line: t.Line, DueDate: t.Due.Time.Format(time.DateOnly), PixCode: t.PixCode}
}

// WriteOff asks for a boleto to be written off: in the next remittance, or, never sent,
// at once.
func (c *Connector) WriteOff(ctx context.Context, attemptID string) error {
	_, err := db.New(c.cfg.Pool).RequestWriteOff(ctx, db.RequestWriteOffParams{AttemptID: attemptID, Now: pgtype.Timestamptz{Time: c.cfg.Now().UTC(), Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // paid, rejected or written off already: the return says so
	}
	return err
}

// call sends one request; out, when set, takes a JSON answer, raw a file.
func (c *Connector) call(ctx context.Context, method, path, contentType string, body []byte, out any) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.cfg.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	label, _, _ := strings.Cut(path, "?")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		if uerr := (*url.Error)(nil); errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, 0, fmt.Errorf("bank: %s %s: %w", method, label, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	switch {
	case err != nil:
		return nil, resp.StatusCode, fmt.Errorf("bank: %s %s: %w", method, label, err)
	case len(raw) > maxResponse:
		return nil, resp.StatusCode, fmt.Errorf("bank: %s %s: an answer of more than %d bytes", method, label, maxResponse)
	case resp.StatusCode >= 300:
		const most = 200
		if len(raw) > most {
			raw = raw[:most]
		}
		return nil, resp.StatusCode, fmt.Errorf("bank: %s %s: %d %s", method, label, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, resp.StatusCode, fmt.Errorf("bank: %s %s: an answer that is not the bank's: %w", method, label, err)
		}
	}
	return raw, resp.StatusCode, nil
}

// pixCode is a hybrid boleto's Pix QR code, from the location the bank registered.
func (c *Connector) pixCode(location string) string {
	name := strings.ToUpper(c.cfg.Name)
	if len(name) > 25 {
		name = name[:25]
	}
	code, err := brcode.Pix{PointOfInitiation: brcode.SingleUse, URL: location, MerchantName: name, MerchantCity: "SAO PAULO"}.Encode()
	if err != nil {
		return ""
	}
	return code
}
