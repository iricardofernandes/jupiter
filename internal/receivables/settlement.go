package receivables

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/pkg/bizday"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
	"github.com/iricardofernandes/jupiter/pkg/slcapi"
)

// Settlement through the SLC, once a business day, per mode:
//
//  1. Each unit due is settled with the registry, which fixes how it splits: to its
//     holder, to the financiers of its contracts, Jupiter among them, and the block back
//     to Jupiter.
//  2. The day's grade is built from them: the registry's payments and Jupiter's fee,
//     which together are the gross the network still owes on each unit. It is kept as built, so that it is sent again the same.
//  3. The grade is submitted; once the SLC has settled it, its cash is posted: what was
//     paid into Jupiter's settlement account, and what went straight to other
//     institutions, which discharges what Jupiter owed them, against what the networks
//     owed.

func (s *Service) settlement(livemode bool) Settlement {
	if livemode {
		return s.cfg.LiveSettlement
	}
	return s.cfg.TestSettlement
}

// SettleDay runs the day's settlement in each mode that has a settlement system, and
// moves on any grade of an earlier day still open.
func (s *Service) SettleDay(ctx context.Context, pool *pgxpool.Pool) error {
	if s.cfg.Payments == nil {
		return errors.New("receivables: settling needs payments, for the accounts the cash moves")
	}
	var errs []error
	for _, livemode := range []bool{false, true} {
		if s.settlement(livemode) == nil {
			continue
		}
		if err := s.settleMode(ctx, pool, livemode); err != nil {
			errs = append(errs, fmt.Errorf("settling livemode=%t: %w", livemode, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) settleMode(ctx context.Context, pool *pgxpool.Pool, livemode bool) error {
	if err := s.gradeToday(ctx, pool, livemode); err != nil {
		return err
	}
	open, err := db.New(pool).OpenGrades(ctx, livemode)
	if err != nil {
		return err
	}
	// One grade that cannot move on holds up none of the others.
	var errs []error
	for _, g := range open {
		if err := s.advanceGrade(ctx, pool, g); err != nil {
			errs = append(errs, fmt.Errorf("the grade of %s: %w", g.Date.Time.Format(time.DateOnly), err))
		}
	}
	return errors.Join(errs...)
}

// gradeToday settles the units due and builds the day's grade, on a business day without
// one yet.
func (s *Service) gradeToday(ctx context.Context, pool *pgxpool.Pool, livemode bool) error {
	today := dayOf(s.cfg.Now())
	if !bizday.IsBusinessDay(today) {
		return nil
	}
	_, err := db.New(pool).GetGrade(ctx, db.GetGradeParams{Livemode: livemode, Date: dateOf(today)})
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	s.settleUnits(ctx, pool, livemode, today)
	return s.buildGrade(ctx, pool, livemode, today)
}

// settleUnits settles with the registry each unit due by day. One that cannot be (not
// registered as it is, its instructions off) waits for the next day, logged.
func (s *Service) settleUnits(ctx context.Context, pool *pgxpool.Pool, livemode bool, day time.Time) {
	for after := ""; ; {
		ids, err := db.New(pool).UnitsDue(ctx, db.UnitsDueParams{Livemode: livemode, Day: dateOf(day), After: after})
		if err != nil {
			s.cfg.Logger.ErrorContext(ctx, "listing the units due", "error", err)
			return
		}
		if len(ids) == 0 {
			return
		}
		for _, unitID := range ids {
			after = unitID
			if _, err := s.Settle(ctx, pool, unitID, day); err != nil {
				s.cfg.Logger.WarnContext(ctx, "a unit due was not settled", "unit", unitID, "error", truncate(err.Error()))
			}
		}
	}
}

// buildGrade gathers the units settled on day into its grade.
func (s *Service) buildGrade(ctx context.Context, pool *pgxpool.Pool, livemode bool, day time.Time) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		units, err := q.UnitsToGrade(ctx, db.UnitsToGradeParams{Livemode: livemode, Day: dateOf(day)})
		if err != nil || len(units) == 0 {
			return err
		}
		entries, ids, err := s.gradeEntries(ctx, tx, units)
		if err != nil || len(entries) == 0 {
			return err
		}
		var total, credited int64
		for _, e := range entries {
			total += e.Amount
			if e.Domicile.ISPB == s.cfg.Domicile.ISPB {
				credited += e.Amount
			}
		}
		raw, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		if err := q.InsertGrade(ctx, db.InsertGradeParams{
			Livemode: livemode, Date: dateOf(day), Entries: raw, Total: total, Credited: credited, Now: ts(s.cfg.Now()),
		}); err != nil {
			return err
		}
		return q.MarkGraded(ctx, db.MarkGradedParams{Units: ids, Day: dateOf(day)})
	})
}

// gradeEntries are the payments of each unit: as the registry split it, the block
// included, and Jupiter's fee.
func (s *Service) gradeEntries(ctx context.Context, tx pgx.Tx, units []db.ReceivablesUnit) ([]slcapi.Entry, []string, error) {
	q := db.New(tx)
	ids := make([]string, len(units))
	for i, u := range units {
		ids[i] = u.ID
	}
	rows, err := q.UnitFees(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	fees := map[string]db.UnitFeesRow{}
	for _, r := range rows {
		fees[r.UnitID] = r
	}
	domicile := slcapi.Domicile{ISPB: s.cfg.Domicile.ISPB, Branch: s.cfg.Domicile.Branch, Account: s.cfg.Domicile.Account}
	holders := map[string]string{}
	var entries []slcapi.Entry
	for _, u := range units {
		if fees[u.ID].Net != u.Value {
			return nil, nil, fmt.Errorf("receivables: unit %s is worth %d, its installments %d", u.ID, u.Value, fees[u.ID].Net)
		}
		holder, err := s.holderOf(ctx, tx, holders, u)
		if err != nil {
			return nil, nil, err
		}
		var paid []registryapi.Payment
		if err := json.Unmarshal(u.Payments, &paid); err != nil {
			return nil, nil, err
		}
		entry := func(n int, to, contract string, amount int64, d slcapi.Domicile) {
			if amount > 0 {
				entries = append(entries, slcapi.Entry{
					ID: fmt.Sprintf("%s/%d", u.ID, n), Holder: holder, Arrangement: u.Arrangement,
					SettlementDate: u.SettlementDate.Time.Format(time.DateOnly), Beneficiary: to, Contract: contract, Amount: amount, Domicile: d,
				})
			}
		}
		for i, p := range paid {
			d := slcapi.Domicile(p.Domicile)
			if d.ISPB == "" {
				d = domicile // the block, which stays with the accreditor
			}
			entry(i+1, p.To, p.Contract, p.Amount, d)
		}
		entry(len(paid)+1, s.cfg.TaxID, "", fees[u.ID].Fee, domicile)
	}
	return entries, ids, nil
}

// advanceGrade submits a grade built, and posts one the SLC settled.
func (s *Service) advanceGrade(ctx context.Context, pool *pgxpool.Pool, g db.ReceivablesGrade) error {
	slc := s.settlement(g.Livemode)
	date := g.Date.Time.Format(time.DateOnly)
	if g.Status == "built" {
		var entries []slcapi.Entry
		if err := json.Unmarshal(g.Entries, &entries); err != nil {
			return err
		}
		_, err := slc.Submit(ctx, slcapi.Grade{Date: date, Entries: entries})
		status, reason := "submitted", ""
		switch {
		case errors.Is(err, ErrSettlementRefused):
			// Not for the worker to fix: an operator looks at it.
			s.cfg.Logger.ErrorContext(ctx, "the SLC refused a grade", "date", date, "error", truncate(err.Error()))
			status, reason = "refused", truncate(err.Error())
		case err != nil:
			return err
		}
		moved, err := db.New(pool).SetGradeStatus(ctx, db.SetGradeStatusParams{
			Livemode: g.Livemode, Date: g.Date, Status: status, Error: reason, Now: ts(s.cfg.Now()), FromStatus: "built",
		})
		if err != nil || moved == 0 || status != "submitted" {
			return err // another pass moved it on
		}
	}
	out, err := slc.Grade(ctx, date)
	if err != nil || out.Status != slcapi.Settled {
		return err
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error { return s.postGrade(ctx, tx, g.Livemode, g.Date.Time, out) })
}

// postGrade posts a settled grade's cash, once.
func (s *Service) postGrade(ctx context.Context, tx pgx.Tx, livemode bool, day time.Time, out slcapi.Grade) error {
	q := db.New(tx)
	g, err := q.LockGrade(ctx, db.LockGradeParams{Livemode: livemode, Date: dateOf(day)})
	if err != nil || g.Status == "settled" {
		return err
	}
	if out.Total != g.Total || out.Credited != g.Credited || out.Credited+out.PaidOther != out.Total {
		return fmt.Errorf("receivables: the SLC settled the grade of %s as %d, %d to Jupiter; it was %d, %d to Jupiter",
			day.Format(time.DateOnly), out.Total, out.Credited, g.Total, g.Credited)
	}
	currency := money.BRL
	network, bank, err := s.cfg.Payments.SettlementAccounts(ctx, tx, livemode, currency)
	if err != nil {
		return err
	}
	total, _ := money.New(g.Total, currency)
	legs := []ledger.Leg{ledger.Credit(network, total)}
	if g.Credited > 0 {
		credited, _ := money.New(g.Credited, currency)
		legs = append(legs, ledger.Debit(bank, credited))
	}
	if other := g.Total - g.Credited; other > 0 {
		financiers, err := s.account(ctx, tx, "", livemode, currency, roleFinanciers)
		if err != nil {
			return err
		}
		amount, _ := money.New(other, currency)
		legs = append(legs, ledger.Debit(financiers, amount))
	}
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{Description: "settlement of " + day.Format(time.DateOnly), Legs: legs})
	if err != nil {
		return fmt.Errorf("posting the settlement: %w", err)
	}
	moved, err := q.SetGradeStatus(ctx, db.SetGradeStatusParams{
		Livemode: livemode, Date: dateOf(day), Status: "settled", LedgerTxn: txn.ID.String(), Now: ts(s.cfg.Now()), FromStatus: g.Status,
	})
	if err == nil && moved == 0 {
		err = fmt.Errorf("receivables: the grade of %s moved on while it was posted", day.Format(time.DateOnly))
	}
	return err
}

// ReportAnticipations tells the SLC of the units Jupiter anticipated, in each mode that
// has one: due by the business day after each anticipation.
func (s *Service) ReportAnticipations(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	reported := 0
	for _, livemode := range []bool{false, true} {
		if slc := s.settlement(livemode); slc != nil {
			n, err := s.reportMode(ctx, pool, slc, livemode)
			reported += n
			if err != nil {
				return reported, err
			}
		}
	}
	return reported, nil
}

func (s *Service) reportMode(ctx context.Context, pool *pgxpool.Pool, slc Settlement, livemode bool) (int, error) {
	q := db.New(pool)
	rows, err := q.AnticipationsToReport(ctx, livemode)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	reports := make([]slcapi.Report, 0, len(rows))
	cache := map[string]string{}
	for _, r := range rows {
		u, err := q.GetUnit(ctx, r.UnitID)
		if err != nil {
			return 0, err
		}
		holder, err := s.holderOf(ctx, pool, cache, u)
		if err != nil {
			return 0, err
		}
		reports = append(reports, slcapi.Report{
			ID: r.AnticipationID + "/" + r.UnitID, Holder: holder, Arrangement: u.Arrangement,
			SettlementDate: u.SettlementDate.Time.Format(time.DateOnly), Amount: r.Amount,
			AnticipatedOn: dayOf(r.CreatedAt.Time).Format(time.DateOnly),
		})
	}
	if err := slc.Report(ctx, reports); err != nil {
		return 0, err
	}
	for i, r := range rows {
		if err := q.MarkReported(ctx, db.MarkReportedParams{AnticipationID: r.AnticipationID, UnitID: r.UnitID, Now: ts(s.cfg.Now())}); err != nil {
			return i, err
		}
	}
	return len(rows), nil
}
