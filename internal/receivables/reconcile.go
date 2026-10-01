package receivables

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/internal/receivables/rules"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

// The reconciliations Res. BCB 264 art. 11 asks of an accreditor.
const (
	Daily       = "daily"       // constituted amounts and contract effects
	Weekly      = "weekly"      // settlement amounts and domiciles
	Fortnightly = "fortnightly" // merchants with an active contract
)

var cadence = map[string]string{Daily: rules.ReconcileDaily, Weekly: rules.ReconcileWeekly, Fortnightly: rules.ReconcileFortnightly}

// Report is what a reconciliation found.
type Report struct {
	Livemode    bool
	Kind        string
	Divergences []Divergence
}

type Divergence struct {
	Subject string
	Detail  string
}

// Reconcile runs, in each mode with a registry, every reconciliation that is due.
func (s *Service) Reconcile(ctx context.Context, pool *pgxpool.Pool) ([]Report, error) {
	var reports []Report
	var failures []error
	today := dayOf(s.cfg.Now())
	for _, livemode := range []bool{false, true} {
		if _, err := s.registry(livemode); errors.Is(err, ErrNoRegistry) {
			continue
		}
		for _, kind := range []string{Daily, Weekly, Fortnightly} {
			last, err := db.New(pool).LastReconciliation(ctx, db.LastReconciliationParams{Livemode: livemode, Kind: kind})
			if err != nil {
				return reports, err
			}
			if today.Before(rules.DeadlineFor(cadence[kind], today).Due(last.Time)) {
				continue
			}
			r, err := s.ReconcileNow(ctx, pool, livemode, kind)
			reports = append(reports, r)
			failures = append(failures, err)
		}
	}
	return reports, errors.Join(failures...)
}

// ReconcileNow runs one reconciliation, records its divergences and resolves those it
// no longer finds.
func (s *Service) ReconcileNow(ctx context.Context, pool *pgxpool.Pool, livemode bool, kind string) (Report, error) {
	reg, err := s.registry(livemode)
	if err != nil {
		return Report{}, err
	}
	report := Report{Livemode: livemode, Kind: kind}
	switch kind {
	case Daily:
		report.Divergences, err = s.reconcileUnits(ctx, pool, reg, livemode)
	case Weekly:
		report.Divergences, err = s.reconcileSettlements(ctx, pool, reg, livemode)
	case Fortnightly:
		report.Divergences, err = s.reconcileHolders(ctx, pool, reg, livemode)
	default:
		return report, fmt.Errorf("%w: reconciliation %q", ErrInvalid, kind)
	}
	if err != nil {
		return report, err
	}
	return report, s.record(ctx, pool, report)
}

func (s *Service) record(ctx context.Context, pool *pgxpool.Pool, r Report) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error { return s.recordIn(ctx, db.New(tx), r) })
}

func (s *Service) recordIn(ctx context.Context, q *db.Queries, r Report) error {
	now := s.cfg.Now()
	subjects := []string{}
	for _, d := range r.Divergences {
		subjects = append(subjects, d.Subject)
		if err := q.OpenDivergence(ctx, db.OpenDivergenceParams{Livemode: r.Livemode, Kind: r.Kind, Subject: d.Subject, Detail: d.Detail, FoundAt: ts(now)}); err != nil {
			return err
		}
	}
	if err := q.ResolveDivergences(ctx, db.ResolveDivergencesParams{Livemode: r.Livemode, Kind: r.Kind, Still: subjects, Now: ts(now)}); err != nil {
		return err
	}
	return q.InsertReconciliation(ctx, db.InsertReconciliationParams{
		Livemode: r.Livemode, Kind: r.Kind, RanOn: dateOf(dayOf(now)), RanAt: ts(now), Divergences: int32(len(r.Divergences)), //nolint:gosec // bounded by the units
	})
}

type key struct{ holder, arrangement, date string }

// String names a unit the registry has and Jupiter does not, with the holder's tax id
// masked: divergences are logged.
func (k key) String() string { return mask(k.holder) + "/" + k.arrangement + "/" + k.date }

func mask(taxID string) string {
	if len(taxID) <= 4 {
		return "****"
	}
	return "****" + taxID[len(taxID)-4:]
}

// units are Jupiter's units of a mode by their registry key: those unsettled, and those
// settled within the reconciliations' reach.
func (s *Service) units(ctx context.Context, pool *pgxpool.Pool, livemode bool) (map[key]db.ReceivablesUnit, error) {
	since := dayOf(s.cfg.Now()).AddDate(0, 0, -rules.DeadlineFor(rules.ReconcileWeekly, s.cfg.Now()).Days)
	rows, err := db.New(pool).UnitsToReconcile(ctx, db.UnitsToReconcileParams{Livemode: livemode, Since: dateOf(since)})
	if err != nil {
		return nil, err
	}
	holders := map[string]string{}
	out := map[key]db.ReceivablesUnit{}
	for _, u := range rows {
		holder, err := s.holderOf(ctx, pool, holders, u)
		if err != nil {
			return nil, err
		}
		if holder != "" {
			out[key{holder, u.Arrangement, u.SettlementDate.Time.Format(time.DateOnly)}] = u
		}
	}
	return out, nil
}

// sentAsIs reports whether the registry should have the unit as Jupiter has it:
// the registry took its current version by the time its positions were read.
func sentAsIs(u db.ReceivablesUnit, read time.Time) bool {
	return u.RegisteredVersion > 0 && u.RegisteredVersion == u.Version && u.RegisteredAt.Valid && !u.RegisteredAt.Time.After(read)
}

// reconcileUnits compares what each unsettled unit is worth and holds blocked, sending
// a unit the registry disagrees on again, and keeps what the registry says is committed
// to contracts. A unit changed or sent since the registry was read is left for the next
// reconciliation.
func (s *Service) reconcileUnits(ctx context.Context, pool *pgxpool.Pool, reg Registry, livemode bool) ([]Divergence, error) {
	read := s.cfg.Now()
	unsettled := false
	positions, err := reg.Units(ctx, &unsettled, "", "", "")
	if err != nil {
		return nil, err
	}
	mine, err := s.units(ctx, pool, livemode)
	if err != nil {
		return nil, err
	}
	q := db.New(pool)
	today := dateOf(dayOf(s.cfg.Now()))
	var out []Divergence
	seen := map[key]bool{}
	for _, p := range positions {
		k := key{p.Holder, p.Arrangement, p.SettlementDate}
		seen[k] = true
		u, ok := mine[k]
		if !ok {
			out = append(out, Divergence{k.String(), "the registry has a unit Jupiter does not"})
			continue
		}
		found, err := s.compareUnit(ctx, q, reg, u, p, read, today)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	for k, u := range mine {
		if !seen[k] && !u.SettledOn.Valid && sentAsIs(u, read) {
			out = append(out, Divergence{u.ID, "registered by Jupiter, missing at the registry"})
			if err := q.Reregister(ctx, db.ReregisterParams{ID: u.ID, Today: today}); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// compareUnit compares a unit with the registry's position of it, sending it again when
// the two disagree on its value, and records what the registry says is committed.
func (s *Service) compareUnit(ctx context.Context, q *db.Queries, reg Registry, u db.ReceivablesUnit, p registryapi.Position, read time.Time, today pgtype.Date) ([]Divergence, error) {
	if u.SettledOn.Valid {
		return []Divergence{{u.ID, "settled at Jupiter, not at the registry"}}, nil
	}
	var out []Divergence
	asSent := sentAsIs(u, read)
	if asSent && (p.Value != u.Value || p.Blocked != u.Blocked) {
		out = append(out, Divergence{u.ID, fmt.Sprintf("the registry has %d (%d blocked), Jupiter %d (%d blocked)", p.Value, p.Blocked, u.Value, u.Blocked)})
		if err := q.Reregister(ctx, db.ReregisterParams{ID: u.ID, Today: today}); err != nil {
			return nil, err
		}
	}
	if err := s.snapshot(ctx, q, u.ID, p.Committed); err != nil {
		return nil, err
	}
	var jupiters int64
	for _, c := range p.Committed {
		if c.Beneficiary == s.cfg.TaxID {
			jupiters += c.Amount
		}
	}
	if asSent && jupiters != u.Anticipated {
		out = append(out, Divergence{u.ID, fmt.Sprintf("the registry commits %d to Jupiter; Jupiter bought %d", jupiters, u.Anticipated)})
		if err := s.replaceContracts(ctx, reg, u, p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// replaceContracts makes the registry commit to Jupiter, on a unit, what Jupiter holds of
// it: Jupiter's contracts there end, and one for what it holds takes their place. That
// mends a contract the registry took for a purchase Jupiter then undid, and one a refund
// left larger than what Jupiter still holds. The new contract comes after any accepted
// since, which the registry pays first.
func (s *Service) replaceContracts(ctx context.Context, reg Registry, u db.ReceivablesUnit, p registryapi.Position) error {
	for _, c := range p.Committed {
		if c.Beneficiary == s.cfg.TaxID {
			if err := reg.EndContract(ctx, c.Contract); err != nil {
				return err
			}
		}
	}
	if u.Anticipated == 0 {
		return nil
	}
	return reg.AcceptContract(ctx, registryapi.Contract{
		ID: fmt.Sprintf("unit/%s/v%d", u.ID, u.Version), Holder: p.Holder, Effect: "ownership_transfer", Rule: "fixed", Amount: u.Anticipated,
		Arrangements: []string{u.Arrangement}, Accreditors: []string{s.cfg.TaxID}, From: p.SettlementDate, To: p.SettlementDate,
		Domicile: s.cfg.Domicile,
	})
}

// snapshot records what is committed on a unit when it differs from what was last seen.
func (s *Service) snapshot(ctx context.Context, q *db.Queries, unitID string, committed []registryapi.Commitment) error {
	if committed == nil {
		committed = []registryapi.Commitment{}
	}
	raw, err := json.Marshal(committed)
	if err != nil {
		return err
	}
	now := s.cfg.Now()
	last, err := q.LatestSnapshot(ctx, db.LatestSnapshotParams{UnitID: unitID, At: ts(now)})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if len(committed) == 0 {
			return nil // nothing committed, and never was
		}
	case err != nil:
		return err
	case sameJSON(last, raw):
		return nil
	}
	return q.InsertSnapshot(ctx, db.InsertSnapshotParams{UnitID: unitID, ObservedAt: ts(now), Commitments: raw})
}

func sameJSON(a, b []byte) bool {
	var x, y []registryapi.Commitment
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return slices.Equal(x, y)
}

// reconcileSettlements compares the units settled in the last week: for how much, and
// to whom and where.
func (s *Service) reconcileSettlements(ctx context.Context, pool *pgxpool.Pool, reg Registry, livemode bool) ([]Divergence, error) {
	today := dayOf(s.cfg.Now())
	from := today.AddDate(0, 0, -rules.DeadlineFor(rules.ReconcileWeekly, today).Days).Format(time.DateOnly)
	settled := true
	positions, err := reg.Units(ctx, &settled, "", from, today.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	mine, err := s.units(ctx, pool, livemode)
	if err != nil {
		return nil, err
	}
	var out []Divergence
	seen := map[key]bool{}
	for _, p := range positions {
		k := key{p.Holder, p.Arrangement, p.SettlementDate}
		seen[k] = true
		u, ok := mine[k]
		if !ok || !u.SettledOn.Valid {
			subject := k.String()
			if ok {
				subject = u.ID
			}
			out = append(out, Divergence{subject, "settled at the registry, not at Jupiter"})
			continue
		}
		var recorded []registryapi.Payment
		if err := json.Unmarshal(u.Payments, &recorded); err != nil {
			return nil, err
		}
		if !slices.Equal(recorded, p.Payments) {
			out = append(out, Divergence{u.ID, "the registry split the settlement differently"})
		}
	}
	for k, u := range mine {
		if day := u.SettlementDate.Time.Format(time.DateOnly); u.SettledOn.Valid && day >= from && day <= today.Format(time.DateOnly) && !seen[k] {
			out = append(out, Divergence{u.ID, "settled at Jupiter, not at the registry"})
		}
	}
	return out, nil
}

// reconcileHolders compares the merchants with a live contract on their units.
func (s *Service) reconcileHolders(ctx context.Context, pool *pgxpool.Pool, reg Registry, livemode bool) ([]Divergence, error) {
	theirs, err := reg.HoldersWithContracts(ctx)
	if err != nil {
		return nil, err
	}
	mine, err := s.units(ctx, pool, livemode)
	if err != nil {
		return nil, err
	}
	snapshots, err := db.New(pool).LatestSnapshots(ctx, livemode)
	if err != nil {
		return nil, err
	}
	committed := map[string]bool{}
	for _, snap := range snapshots {
		var c []registryapi.Commitment
		if err := json.Unmarshal(snap.Commitments, &c); err != nil {
			return nil, err
		}
		committed[snap.UnitID] = len(c) > 0
	}
	// The merchants with something committed on an unsettled unit, by tax id.
	known := map[string]string{}
	for k, u := range mine {
		if !u.SettledOn.Valid && committed[u.ID] {
			known[k.holder] = u.MerchantID
		}
	}
	var out []Divergence
	for _, h := range theirs {
		if _, ok := known[h]; !ok {
			out = append(out, Divergence{mask(h), "the registry has a contract on this merchant's units that Jupiter has not seen"})
		}
		delete(known, h)
	}
	for _, merchantID := range known {
		out = append(out, Divergence{merchantID, "Jupiter saw a contract on this merchant's units the registry no longer has"})
	}
	return out, nil
}
