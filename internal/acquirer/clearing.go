package acquirer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/acquirer/db"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

var ErrClearingNotReady = errors.New("acquirer: the network has not closed that business day yet")

const (
	maxClearingFile    = 64 << 20
	maxClearingRecords = 100_000
	exceptionBatch     = 500
)

type ClearingReport struct {
	BusinessDate    time.Time
	Records         int
	Cleared         int
	Exceptions      int
	AlreadyImported bool
}

// ImportClearing fetches the network's clearing file for day and marks what it lists as
// cleared. A day is imported once: the same file again changes nothing, and a different
// one is refused. A record that matches no capture or refund, or not for its amount, is
// kept as an exception for reconciliation.
func (c *Connector) ImportClearing(ctx context.Context, day time.Time, p *payments.Service) (ClearingReport, error) {
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	data, err := c.fetchClearing(ctx, day)
	if err != nil {
		return ClearingReport{}, err
	}
	file, err := cardnet.DecodeClearing(data)
	if err != nil {
		return ClearingReport{}, err
	}
	if len(file.Records) > maxClearingRecords {
		return ClearingReport{}, fmt.Errorf("%w: %d records, more than %d", cardnet.ErrClearingFile, len(file.Records), maxClearingRecords)
	}
	if !file.BusinessDate.Equal(day) || file.AcquirerID != cardnet.PadAcquirer(c.cfg.AcquirerID) {
		return ClearingReport{}, fmt.Errorf("%w: the file is for %s, day %s", cardnet.ErrClearingFile, file.AcquirerID, file.BusinessDate.Format(time.DateOnly))
	}
	sum := sha256.Sum256(data)
	var report ClearingReport
	err = postgres.InTx(ctx, c.cfg.Pool, func(tx pgx.Tx) error {
		var err error
		report, err = c.apply(ctx, tx, p, file, sum[:])
		return err
	})
	return report, err
}

// apply records the file as imported and each of its records as cleared, or as an
// exception, in one transaction.
func (c *Connector) apply(ctx context.Context, tx pgx.Tx, p *payments.Service, file cardnet.ClearingFile, sum []byte) (ClearingReport, error) {
	report := ClearingReport{BusinessDate: file.BusinessDate, Records: len(file.Records)}
	q := db.New(tx)
	date := pgtype.Date{Time: file.BusinessDate, Valid: true}
	_, err := q.InsertClearingFile(ctx, db.InsertClearingFileParams{
		BusinessDate: date, Sha256: sum, Records: int32(len(file.Records)), Now: timestamptz(c.cfg.Now()), //nolint:gosec // bounded by maxClearingRecords
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := q.GetClearingFile(ctx, date)
		if err != nil {
			return report, err
		}
		if !bytes.Equal(existing.Sha256, sum) {
			return report, fmt.Errorf("%w: the network sent a different file for %s, already imported", cardnet.ErrClearingFile, file.BusinessDate.Format(time.DateOnly))
		}
		report.AlreadyImported = true
		return report, nil
	}
	if err != nil {
		return report, err
	}
	seen := map[string]bool{}
	for _, r := range file.Records {
		var reason string
		if key := string(r.Kind) + r.RRN; seen[key] {
			reason = "the file lists this record twice"
		} else {
			seen[key] = true
			if reason, err = c.clear(ctx, tx, p, r, file.BusinessDate); err != nil {
				return report, err
			}
		}
		if reason == "" {
			report.Cleared++
			continue
		}
		report.Exceptions++
		if err := q.InsertClearingException(ctx, db.InsertClearingExceptionParams{
			BusinessDate: date, Kind: string(r.Kind), Rrn: r.RRN, NetworkTransactionID: r.NetworkTransactionID,
			Amount: r.Amount, MerchantCode: r.MerchantID, Reason: reason,
		}); err != nil {
			return report, err
		}
	}
	return report, nil
}

// clear applies one record, returning why it does not match, or "". The record must
// agree with what Jupiter sent under its RRN: the kind, the merchant and the network
// transaction.
func (c *Connector) clear(ctx context.Context, tx pgx.Tx, p *payments.Service, r cardnet.ClearingRecord, day time.Time) (string, error) {
	q := db.New(tx)
	ex, err := q.ExchangeByRRN(ctx, r.RRN)
	if errors.Is(err, pgx.ErrNoRows) {
		return "no capture or refund was sent with this RRN", nil
	}
	if err != nil {
		return "", err
	}
	wantKind := map[cardnet.ClearingKind]string{cardnet.ClearingCompletion: kindCapture, cardnet.ClearingRefund: kindRefund}[r.Kind]
	if ex.Kind != wantKind {
		return fmt.Sprintf("the RRN belongs to a %s, not a %s record", ex.Kind, r.Kind), nil
	}
	if ex.MerchantCode != r.MerchantID {
		return "the merchant differs from the one the RRN was sent for", nil
	}
	auth, err := q.GetExchange(ctx, ex.AuthorizationKey.String)
	if err != nil {
		return "", err
	}
	if auth.NetworkTransactionID != r.NetworkTransactionID {
		return "the network transaction differs from the authorization's", nil
	}
	if ex.Kind == kindCapture {
		err = p.MarkCleared(ctx, tx, ex.AuthorizationKey.String, r.Amount, day)
	} else {
		err = p.MarkRefundCleared(ctx, tx, ex.Key, r.Amount, day)
	}
	if errors.Is(err, payments.ErrClearingMismatch) || errors.Is(err, payments.ErrNotFound) {
		return err.Error(), nil
	}
	return "", err
}

// RetryExceptions applies again the clearing records that did not match before, such
// as a completion cleared while its capture was still waiting for the network's
// acknowledgement, and returns how many now match.
func (c *Connector) RetryExceptions(ctx context.Context, p *payments.Service) (int, error) {
	resolved := 0
	err := postgres.InTx(ctx, c.cfg.Pool, func(tx pgx.Tx) error {
		resolved = 0
		q := db.New(tx)
		open, err := q.OpenClearingExceptions(ctx, exceptionBatch)
		if err != nil {
			return err
		}
		for _, e := range open {
			r := cardnet.ClearingRecord{
				Kind: cardnet.ClearingKind(e.Kind), RRN: e.Rrn, NetworkTransactionID: e.NetworkTransactionID,
				Amount: e.Amount, MerchantID: e.MerchantCode,
			}
			reason, err := c.clear(ctx, tx, p, r, e.BusinessDate.Time)
			switch {
			case err != nil:
				return err
			case reason == "":
				resolved++
				err = q.ResolveClearingException(ctx, db.ResolveClearingExceptionParams{ID: e.ID, Now: timestamptz(c.cfg.Now())})
			case reason != e.Reason:
				err = q.SetClearingExceptionReason(ctx, db.SetClearingExceptionReasonParams{ID: e.ID, Reason: reason})
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return resolved, err
}

func (c *Connector) fetchClearing(ctx context.Context, day time.Time) ([]byte, error) {
	url := fmt.Sprintf("%s/v1/acquirers/%s/clearing/%s", c.cfg.ClearingURL, cardnet.PadAcquirer(c.cfg.AcquirerID), day.Format(time.DateOnly))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the clearing file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrClearingNotReady
	default:
		return nil, fmt.Errorf("fetching the clearing file: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxClearingFile))
	if err != nil {
		return nil, fmt.Errorf("reading the clearing file: %w", err)
	}
	return data, nil
}

// ImportRecentClearing imports every closed business day of the last days not imported
// yet, then retries the exceptions, and returns how many days it imported. A day that
// fails does not stop the others.
func (c *Connector) ImportRecentClearing(ctx context.Context, p *payments.Service, days int) (int, error) {
	imported := 0
	var failures []error
	today := c.cfg.Now().UTC()
	for back := days; back >= 1; back-- {
		day := today.AddDate(0, 0, -back)
		day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		if _, err := db.New(c.cfg.Pool).GetClearingFile(ctx, pgtype.Date{Time: day, Valid: true}); err == nil {
			continue
		}
		report, err := c.ImportClearing(ctx, day, p)
		switch {
		case errors.Is(err, ErrClearingNotReady):
		case err != nil:
			failures = append(failures, fmt.Errorf("importing clearing for %s: %w", day.Format(time.DateOnly), err))
		case !report.AlreadyImported:
			imported++
		}
	}
	if _, err := c.RetryExceptions(ctx, p); err != nil {
		failures = append(failures, fmt.Errorf("retrying clearing exceptions: %w", err))
	}
	return imported, errors.Join(failures...)
}
