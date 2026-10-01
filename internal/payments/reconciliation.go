package payments

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
)

// MerchantsOf answers the merchant each of some attempts and refunds belongs to.
func (s *Service) MerchantsOf(ctx context.Context, q db.DBTX, ids []string) (map[string]string, error) {
	rows, err := db.New(q).MerchantsOfObjects(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Object] = r.MerchantID
	}
	return out, nil
}

// Captured answers which of some attempts were captured: their money booked.
func (s *Service) Captured(ctx context.Context, q db.DBTX, ids []string) (map[string]bool, error) {
	rows, err := db.New(q).CapturedAttempts(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, attemptID := range rows {
		out[attemptID] = true
	}
	return out, nil
}

// Jupiter's side of its accounts' statements: each movement its ledger booked through its
// account at the Pix bank, or through its account at the bank that makes its transfers,
// keyed as the bank keys it.

// PixRecords are the movements of Jupiter's account at its Pix bank: Pix received; and
// out of it, refunds and strays returned, payouts, and MED returns, with the MED returns
// a contestation gave back.
func (s *Service) PixRecords(ctx context.Context, pool *pgxpool.Pool, livemode bool, since time.Time) ([]reconciliation.Record, error) {
	q := db.New(pool)
	from := pgtype.Timestamptz{Time: since, Valid: true}
	var out []reconciliation.Record
	received, err := q.PixReceivedSince(ctx, db.PixReceivedSinceParams{Livemode: livemode, Since: from})
	if err != nil {
		return nil, err
	}
	for _, r := range received {
		out = append(out, reconciliation.Record{
			Identity: "pix/" + r.E2eID, Key: r.E2eID, Direction: reconciliation.In, Amount: r.Amount, Date: reconciliation.Day(r.ReceivedAt.Time),
			Merchant: r.MerchantID, Reference: r.E2eID,
		})
		if r.Status == "returned" {
			key := unmatchedReturnID(r.E2eID)
			out = append(out, reconciliation.Record{
				Identity: "return/" + key, Key: key, Direction: reconciliation.Out, Amount: r.Amount, Date: reconciliation.Day(r.UpdatedAt.Time), Reference: r.E2eID,
			})
		}
	}
	refunds, err := q.PixRefundsSince(ctx, db.PixRefundsSinceParams{Livemode: livemode, Since: from})
	if err != nil {
		return nil, err
	}
	for _, r := range refunds {
		out = append(out, reconciliation.Record{
			Identity: r.ID, Key: bankID(r.ID), Direction: reconciliation.Out, Amount: r.Amount, Date: reconciliation.Day(r.UpdatedAt.Time),
			Merchant: r.MerchantID, Reference: r.ID,
		})
	}
	paid, err := s.payoutRecords(ctx, q, livemode, PayoutPix, since)
	if err != nil {
		return nil, err
	}
	out = append(out, paid...)
	disputed, err := q.PixDisputeFundsSince(ctx, db.PixDisputeFundsSinceParams{Livemode: livemode, Since: from})
	if err != nil {
		return nil, err
	}
	for _, f := range disputed {
		key := bankID(f.Reference)
		out = append(out, reconciliation.Record{
			Identity: f.Reference + "/out", Key: key, Direction: reconciliation.Out, Amount: f.Amount, Date: reconciliation.Day(f.WithdrawnAt.Time),
			Merchant: f.MerchantID, Reference: f.Reference,
		})
		if f.Status == fundsReinstated {
			out = append(out, reconciliation.Record{
				Identity: f.Reference + "/in", Key: key, Direction: reconciliation.In, Amount: f.Amount, Date: reconciliation.Day(f.UpdatedAt.Time),
				Merchant: f.MerchantID, Reference: f.Reference,
			})
		}
	}
	return out, nil
}

// BankTransferRecords are the movements of Jupiter's account at the bank that makes its
// transfers, that payouts made: each transfer out, and each one the receiving bank
// returned.
func (s *Service) BankTransferRecords(ctx context.Context, pool *pgxpool.Pool, livemode bool, since time.Time) ([]reconciliation.Record, error) {
	return s.payoutRecords(ctx, db.New(pool), livemode, PayoutBankTransfer, since)
}

func (s *Service) payoutRecords(ctx context.Context, q *db.Queries, livemode bool, method string, since time.Time) ([]reconciliation.Record, error) {
	rows, err := q.PayoutsSince(ctx, db.PayoutsSinceParams{Livemode: livemode, Method: method, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return nil, err
	}
	var out []reconciliation.Record
	for _, p := range rows {
		key := bankID(p.ID)
		out = append(out, reconciliation.Record{
			Identity: p.ID + "/out", Key: key, Direction: reconciliation.Out, Amount: p.Amount, Date: reconciliation.Day(p.ArrivedAt.Time),
			Merchant: p.MerchantID, Reference: p.ID,
		})
		if p.Status == "returned" {
			out = append(out, reconciliation.Record{
				Identity: p.ID + "/in", Key: key, Direction: reconciliation.In, Amount: p.Amount, Date: reconciliation.Day(p.ReturnedAt.Time),
				Merchant: p.MerchantID, Reference: p.ID,
			})
		}
	}
	return out, nil
}
