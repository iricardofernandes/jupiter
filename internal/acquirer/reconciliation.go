package acquirer

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer/db"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// Clearing is the card network's clearing, reconciled: each capture the network
// acknowledged and refund it approved, by its RRN, against the records of its clearing
// files. A capture is due in the file of the day, in UTC, it was sent, as the network
// closes its days.
func (c *Connector) Clearing(p *payments.Service) reconciliation.Stream {
	return reconciliation.Stream{
		Counterparty: "card_network", Name: "clearing",
		Ours: []reconciliation.OursFunc{func(ctx context.Context, pool *pgxpool.Pool, _ bool, since time.Time) ([]reconciliation.Record, error) {
			return c.clearingOurs(ctx, pool, p, since)
		}},
		Theirs: c.clearingTheirs,
	}
}

func (c *Connector) clearingOurs(ctx context.Context, pool *pgxpool.Pool, p *payments.Service, since time.Time) ([]reconciliation.Record, error) {
	rows, err := db.New(pool).ClearedExchangesSince(ctx, pgtype.Timestamptz{Time: since, Valid: true})
	if err != nil {
		return nil, err
	}
	objects := make([]string, 0, len(rows))
	for _, r := range rows {
		objects = append(objects, objectOf(r))
	}
	merchants, err := p.MerchantsOf(ctx, pool, objects)
	if err != nil {
		return nil, err
	}
	out := make([]reconciliation.Record, 0, len(rows))
	for _, r := range rows {
		direction := reconciliation.In
		if r.Kind == kindRefund {
			direction = reconciliation.Out
		}
		out = append(out, reconciliation.Record{
			Identity: r.Key, Key: r.Rrn, Direction: direction, Amount: r.Amount, Date: reconciliation.UTCDay(r.CreatedAt.Time),
			Merchant: merchants[objectOf(r)], Reference: objectOf(r),
		})
	}
	return out, nil
}

// objectOf is the payments object an exchange stands for: a capture's attempt, a refund.
func objectOf(r db.ClearedExchangesSinceRow) string {
	if r.Kind == kindCapture && r.AuthorizationKey.Valid {
		return r.AuthorizationKey.String
	}
	return r.Key
}

func (c *Connector) clearingTheirs(ctx context.Context, pool *pgxpool.Pool, _ bool, day time.Time) ([]reconciliation.Record, error) {
	rows, err := db.New(pool).ClearingRecordsOn(ctx, pgtype.Date{Time: day, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make([]reconciliation.Record, 0, len(rows))
	for _, r := range rows {
		direction := reconciliation.In
		if cardnet.ClearingKind(r.Kind) == cardnet.ClearingRefund {
			direction = reconciliation.Out
		}
		out = append(out, reconciliation.Record{
			Identity: fmt.Sprintf("%s/%d", day.Format(time.DateOnly), r.Line), Key: r.Rrn, Direction: direction, Amount: r.Amount, Date: day,
			Reference: r.NetworkTransactionID,
		})
	}
	return out, nil
}
