package receivables

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/recipients"
)

// SchedulePayouts makes, for each recipient whose transfer settings fall on today, a
// payout of its whole available balance, once a day; the payouts' worker sends them. A
// balance below the least a payout carries waits for the next one.
func (s *Service) SchedulePayouts(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	made := 0
	for _, livemode := range []bool{false, true} {
		for after := ""; ; {
			recs, err := s.cfg.Recipients.Transferring(ctx, pool, livemode, after)
			if err != nil || len(recs) == 0 {
				if err != nil {
					return made, err
				}
				break
			}
			after = recs[len(recs)-1].ID.String()
			made += s.schedulePayouts(ctx, pool, recs)
		}
	}
	return made, nil
}

func (s *Service) schedulePayouts(ctx context.Context, pool *pgxpool.Pool, recs []recipients.Recipient) int {
	made := 0
	today := dayOf(s.cfg.Now())
	for _, rec := range recs {
		if !rec.Transfers.Due(today) {
			continue
		}
		ok, err := s.schedulePayout(ctx, pool, rec)
		if err != nil {
			s.cfg.Logger.WarnContext(ctx, "a scheduled payout was not made", "recipient", rec.ID, "error", truncate(err.Error()))
		}
		if ok {
			made++
		}
	}
	return made
}

func (s *Service) schedulePayout(ctx context.Context, pool *pgxpool.Pool, rec recipients.Recipient) (bool, error) {
	owner := payments.Owner{Merchant: rec.Owner.Merchant, Livemode: rec.Owner.Livemode}
	made := false
	method := payments.PayoutPix
	if rec.Destination.Method == "bank_account" {
		method = payments.PayoutBankTransfer
	}
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		b, err := s.Balance(ctx, tx, owner, rec.ID.String(), money.BRL)
		if err != nil || b.Available < payments.MinPayout(method) {
			return err // too little waits for the next one
		}
		amount, err := money.New(b.Available, money.BRL)
		if err != nil {
			return err
		}
		_, err = s.cfg.Payments.CreatePayout(ctx, tx, owner, payments.PayoutParams{Amount: amount, Recipient: rec.ID.String(), ScheduledOn: dayOf(s.cfg.Now())})
		if errors.Is(err, payments.ErrInvalidState) {
			return nil // made already today
		}
		made = err == nil
		return err
	})
	return made, err
}
