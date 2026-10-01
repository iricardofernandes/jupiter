package bank

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/bank/db"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
)

// Returns is the bank's CNAB returns, reconciled: each boleto Jupiter booked as paid, by
// its nosso número, against the paid records of the return files it read.
func (c *Connector) Returns(p *payments.Service) reconciliation.Stream {
	return reconciliation.Stream{
		Counterparty: "bank", Name: "cnab_return",
		Ours: []reconciliation.OursFunc{func(ctx context.Context, pool *pgxpool.Pool, _ bool, since time.Time) ([]reconciliation.Record, error) {
			return c.paidTitles(ctx, pool, p, since, false)
		}},
		Theirs: c.returnRecords,
	}
}

// BoletoCredits are Jupiter's side of the statement's boleto credits: each boleto it
// booked as paid, on the day its return said it is credited.
func (c *Connector) BoletoCredits(p *payments.Service) reconciliation.OursFunc {
	return func(ctx context.Context, pool *pgxpool.Pool, _ bool, since time.Time) ([]reconciliation.Record, error) {
		return c.paidTitles(ctx, pool, p, since, true)
	}
}

// paidTitles are the boletos whose payment Jupiter booked: by when it learned of it, or,
// for the statement, by when it is credited.
func (c *Connector) paidTitles(ctx context.Context, pool *pgxpool.Pool, p *payments.Service, since time.Time, credited bool) ([]reconciliation.Record, error) {
	rows, err := db.New(pool).PaidTitlesSince(ctx, db.PaidTitlesSinceParams{Livemode: c.cfg.Livemode, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return nil, err
	}
	attempts := make([]string, len(rows))
	for i, r := range rows {
		attempts[i] = r.AttemptID
	}
	booked, err := p.Captured(ctx, pool, attempts)
	if err != nil {
		return nil, err
	}
	merchants, err := p.MerchantsOf(ctx, pool, attempts)
	if err != nil {
		return nil, err
	}
	out := make([]reconciliation.Record, 0, len(rows))
	for _, r := range rows {
		if !booked[r.AttemptID] {
			continue // paid at the bank, not booked: what the counterparty's side shows alone
		}
		rec := reconciliation.Record{
			Identity: r.AttemptID, Key: r.OurNumber, Direction: reconciliation.In, Amount: r.Amount, Date: day(r.UpdatedAt.Time),
			Merchant: merchants[r.AttemptID], Reference: r.AttemptID,
		}
		if credited {
			if !r.CreditOn.Valid {
				continue
			}
			rec.Date = r.CreditOn.Time
		}
		out = append(out, rec)
	}
	return out, nil
}

func (c *Connector) returnRecords(ctx context.Context, pool *pgxpool.Pool, _ bool, d time.Time) ([]reconciliation.Record, error) {
	rows, err := db.New(pool).ReturnRecordsImportedOn(ctx, db.ReturnRecordsImportedOnParams{Livemode: c.cfg.Livemode, Day: pgtype.Date{Time: d, Valid: true}})
	if err != nil {
		return nil, err
	}
	var out []reconciliation.Record
	for _, r := range rows {
		if !cnab240.Occurrence(r.Occurrence).Paid() || r.Paid <= 0 {
			continue
		}
		occurred := d
		if r.OccurredOn.Valid {
			occurred = r.OccurredOn.Time
		}
		out = append(out, reconciliation.Record{
			Identity: fmt.Sprintf("%d/%d", r.ReturnSequence, r.Line), Key: r.OurNumber, Direction: reconciliation.In, Amount: r.Paid,
			Date: occurred, Reference: fmt.Sprintf("return %d", r.ReturnSequence),
		})
	}
	return out, nil
}

// statementLine is a line of the bank's statement.
type statementLine struct {
	ID          string `json:"id"`
	Date        string `json:"date"`
	Kind        string `json:"kind"`
	Amount      int64  `json:"amount"`
	Reference   string `json:"reference"`
	Description string `json:"description"`
}

// Statement is the bank's side of Jupiter's account: the day's statement, a record a line.
func (c *Connector) Statement(ctx context.Context, _ *pgxpool.Pool, _ bool, d time.Time) ([]reconciliation.Record, error) {
	var out struct {
		Entries []statementLine `json:"entries"`
	}
	if _, _, err := c.call(ctx, http.MethodGet, "/v1/statements?date="+url.QueryEscape(d.Format(time.DateOnly)), "", nil, &out); err != nil {
		return nil, err
	}
	records := make([]reconciliation.Record, 0, len(out.Entries))
	for _, e := range out.Entries {
		direction := reconciliation.In
		if e.Kind == "debit" {
			direction = reconciliation.Out
		}
		lineDay, err := time.Parse(time.DateOnly, e.Date)
		if err != nil || e.ID == "" {
			return nil, fmt.Errorf("bank: a statement line without an id or a date: %q", e.ID)
		}
		records = append(records, reconciliation.Record{
			Identity: e.ID, Key: e.Reference, Direction: direction, Amount: e.Amount, Date: lineDay, Reference: e.Description,
		})
	}
	return records, nil
}
